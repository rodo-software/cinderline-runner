package run

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/redact"
	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// sleep waits d or until ctx is done; a variable so tests need not wait.
var sleep = forge.Sleep

// nextWaits are the waits between the tries of a next call the app did not answer.
var nextWaits = []time.Duration{2 * time.Second, 8 * time.Second}

// errStopped is the cause of a turn the poll's stop ended.
var errStopped = errors.New("stopped by Cinderline")

// job is a run's fixed facts for its steps.
type job struct {
	ref, title, body string // the issue's; body is untrusted
	slot             string // the run's slot, as the budget and the provider places name it (Project.slot)
	branch, base     string
	dir              string
	team             cinderline.Team // the lead first
	check            string          // the project's check command
	idle             time.Duration   // the project's idle limit for a turn
	fg               forge.Forge
	ws               cinderline.RunWorkspace
	skills           []client.SkillMount // the skill packages loaded, for every turn
	tools            *tools.Mount        // the tools mount from the run's prepare, for every turn; nil: the CLI's
	// manager is a run in the Project Manager slot: it reads a fresh clone
	// of the base, never commits or pushes, and refuses a check, publish or
	// resolve step (CLR-194).
	manager bool
	// blocked is the merge agent's "Blocked" line when it could not resolve
	// the base merge in this run, for the publish's reason (CLR-245).
	blocked string
}

func (j *job) isTeam() bool { return len(j.team.Agents) > 1 }

// session is a client session the run can resume: an agent's on an issue,
// with its usage total so far for a client that reports one (Codex); nil:
// none yet.
type session struct {
	issue, agent, id, origin string
	total                    *client.Usage
}

// continued records a turn that continued the session as id, with the
// session's usage total if the turn reported one. A new id is a new session,
// whose total starts at zero.
func (s *session) continued(id string, total *client.Usage) {
	if id != s.id {
		s.total = nil
	}
	s.id, s.origin = id, "turn"
	if total != nil {
		s.total = total
	}
}

// held is what a run holds between its steps, in memory only: the lead's
// session on the run's issue, the last turn's when that is another's (one
// session per agent and issue), and each issue's last report and agent, for
// a check.
type held struct {
	lead, other session
	reports     map[string]string
	agents      map[string]string
}

// sessions are the sessions for next, the lead's first.
func (h *held) sessions() []cinderline.Session {
	out := []cinderline.Session{}
	for _, s := range []session{h.lead, h.other} {
		if s.id != "" {
			out = append(out, cinderline.Session{Issue: s.issue, Agent: s.agent, Origin: s.origin})
		}
	}
	return out
}

// session is the session the run holds for agent on issue; its id is "": none.
func (h *held) session(issue, agent string) session {
	for _, s := range []session{h.lead, h.other} {
		if s.issue == issue && s.agent == agent {
			return s
		}
	}
	return session{}
}

// turned records a turn of agent on issue that ended in session id, with the
// session's usage total if the client reports one.
func (h *held) turned(issue, agent, id, report string, total *client.Usage) {
	switch {
	case issue == h.lead.issue && agent == h.lead.agent:
		if id != "" {
			h.lead.continued(id, total)
		}
		h.other = session{}
	case id != "":
		if h.other.issue != issue || h.other.agent != agent {
			h.other = session{issue: issue, agent: agent}
		}
		h.other.continued(id, total)
	default:
		h.other = session{}
	}
	h.agents[issue] = agent
	if report != "" {
		h.reports[issue] = report
	}
}

// malformed is a step the runner cannot do: it ends the run with an unasked stop.
func malformed(c *cinderline.Client, kind, why string) error {
	return fmt.Errorf("runner %s cannot do the %q step Cinderline asked for: %s; update the runner", c.Version(), kind, why)
}

// run prepares and does one job in the slot, following next until done. It reports
// started false when the run did not start; err then says why, or is nil
// when the app took the job back (a 409 on the first call). Nothing is
// claimed before the first call, so a run that did not start wrote nothing.
func (p *Project) run(ctx context.Context, c *cinderline.Client, proj cinderline.Project, jb cinderline.Job, slot int) (started bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			p.Log.Error("panic", "project", p.Config.Key, "issue", jb.Issue.Ref, "stack", string(debug.Stack()))
			if !started {
				err = fmt.Errorf("panic: %v", r)
			}
		}
	}()
	j, cf, h, err := p.prepareRun(ctx, proj, jb, slot)
	if errors.Is(err, errNoMemory) {
		// Nothing is claimed yet, so the job is dropped as one taken back; the next poll offers no slot it does not fit.
		p.Log.Info("job dropped: the base's compose file makes the run larger than the memory left", "project", p.Config.Key, "issue", jb.Issue.Ref, "err", err)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// A manager run never commits or pushes: its clone is only read.
	if j.manager {
		defer p.removeClone(context.WithoutCancel(ctx), j) // also after a plain stop
	}
	return p.steps(ctx, c, j, cf, h)
}

// steps asks next at every step boundary and does the step it gets, until
// done. It holds no policy: it sends facts and echoes state.
func (p *Project) steps(ctx context.Context, c *cinderline.Client, j *job, cf *conflict, h *held) (started bool, err error) {
	log := p.Log.With("project", p.Config.Key, "issue", j.ref)
	req := cinderline.NextRequest{Workspace: j.ws}
	pending := cf // the conflict the request reports, for a resolve step
	if cf != nil {
		req.Last = &cf.last
	}
	failing := false // the last request was the runner's own stop
	for first := true; ; first = false {
		req.ID = rand.Text()
		req.Sessions = h.sessions()
		ans, err := p.next(ctx, c, j.ref, req)
		var taken *cinderline.ConflictError
		switch {
		case ctx.Err() != nil:
			return !first, nil // a plain stop: the app blocks the issue after the restart
		case err == nil:
		case first && errors.As(err, &taken):
			log.Info("job taken back, dropped", "err", err)
			p.keepResumed(j, h)
			return false, nil
		case first:
			// Never queued: nothing was claimed, so the run did not start.
			p.keepResumed(j, h)
			return false, err
		case cinderline.Unavailable(err):
			log.Warn("next step not reached, run paused and its report queued", "err", err)
			p.pause(ctx, j, h)
			c.Queue(j.ref, req)
			return true, nil
		default:
			log.Error("next refused the report, run paused", "err", err)
			p.pause(ctx, j, h)
			return true, nil
		}
		req.State, req.Last = &ans.State, nil
		step := ans.Step
		log.Info("step", "kind", step.Kind, "reason", step.Reason)

		if step.Kind == "done" && (step.Session == "paused" || step.Session == "kept") {
			p.keepLead(j, h, step.Session)
			return true, nil
		}
		if failing && step.Kind != "stop" {
			log.Warn("run ended after the runner's own stop", "kind", step.Kind)
			p.keepLead(j, h, "paused")
			return true, nil
		}
		last, err := p.step(ctx, c, j, h, step, &pending)
		if ctx.Err() != nil {
			return true, nil
		}
		if err != nil {
			log.Error("run stopped by the runner", "err", err)
			stop := p.stop(ctx, j)
			stop.Error = ptr(err.Error())
			last, failing = &stop, true
		}
		req.Last = last
		if j.isTeam() && h.lead.id != "" {
			// After every step, so a restart during the sub-issues resumes the orchestrator.
			p.saveNote(j.ref, p.note(j, h, "paused"))
		}
	}
}

// step does one step and returns its facts. An error, a panic too, is the
// runner's own failure, as is a step it does not know.
func (p *Project) step(ctx context.Context, c *cinderline.Client, j *job, h *held, step cinderline.Step, pending **conflict) (last *cinderline.LastStep, err error) {
	defer func() {
		if r := recover(); r != nil {
			p.Log.Error("panic", "project", p.Config.Key, "issue", j.ref, "stack", string(debug.Stack()))
			last, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	cf := *pending
	*pending = nil
	if j.manager && step.Kind != "run" && step.Kind != "stop" {
		return nil, malformed(c, step.Kind, "a Project Manager run is never checked, published or merged")
	}
	if step.Kind == "run" || step.Kind == "resolve" || step.Kind == "check" {
		p.takeRefused(j.ref, nil) // what an earlier step left is not this one's
		defer func() {
			if last != nil {
				last.Refused = p.takeRefused(j.ref, step.AllowHosts)
			}
		}()
	}
	switch step.Kind {
	case "run":
		return p.runStep(ctx, c, j, h, step)
	case "resolve":
		if cf == nil {
			return nil, malformed(c, step.Kind, "no conflict was reported")
		}
		return p.resolveStep(ctx, c, j, h, step, cf)
	case "check":
		return p.checkStep(ctx, c, j, h, step)
	case "publish":
		return p.publishStep(ctx, j, step, pending)
	case "stop":
		last := p.stop(ctx, j)
		return &last, nil
	}
	return nil, malformed(c, step.Kind, "unknown kind or session "+step.Session)
}

// next asks the app for the next step, again after nextWaits while it does not answer.
func (p *Project) next(ctx context.Context, c *cinderline.Client, ref string, req cinderline.NextRequest) (cinderline.NextAnswer, error) {
	for i := 0; ; i++ {
		ans, err := c.Next(ctx, ref, req)
		if err == nil || i == len(nextWaits) || !cinderline.Unavailable(err) {
			return ans, err
		}
		if err := sleep(ctx, nextWaits[i]); err != nil {
			return ans, err
		}
	}
}

// note is the note of the lead's session.
func (p *Project) note(j *job, h *held, origin string) Note {
	return Note{Dir: j.dir, Session: h.lead.id, Total: h.lead.total, Client: j.team.Agents[0].Client, Origin: origin, Manager: j.manager}
}

// keepLead keeps the lead's session under origin, or drops the note when there is none.
func (p *Project) keepLead(j *job, h *held, origin string) {
	if h.lead.id == "" || j.dir == "" {
		p.dropNote(j.ref)
		return
	}
	p.saveNote(j.ref, p.note(j, h, origin))
}

// keepResumed puts back the note of a run that did not start.
func (p *Project) keepResumed(j *job, h *held) {
	if h.lead.id != "" {
		p.saveNote(j.ref, p.note(j, h, h.lead.origin))
	}
}

// pause ends a run the app does not follow: the work is pushed and the
// lead's session kept as paused.
func (p *Project) pause(ctx context.Context, j *job, h *held) {
	if s := p.stop(ctx, j); s.Outcome != "pushed" {
		p.Log.Warn("work not pushed", "project", p.Config.Key, "issue", j.ref, "err", deref(s.Error))
	}
	p.keepLead(j, h, "paused")
}

// stop commits and pushes what the clone holds. A secret the runner knows
// in its commits is the outcome secret: nothing is pushed and the clone
// stays as it is. A manager run's clone is read-only: it is neither
// committed nor pushed, and the stop reports not_pushed (CLR-194).
func (p *Project) stop(ctx context.Context, j *job) cinderline.LastStep {
	last := cinderline.LastStep{Step: "stop", Issue: j.ref}
	if j.manager {
		last.Outcome = "not_pushed" // not even what its agent changed
		return last
	}
	var found []workspace.Secret
	_, err := p.Workspace.Commit(ctx, j.dir, leadAuthor(j.team), j.ref+": "+j.title)
	if err == nil {
		found, err = p.Workspace.Push(ctx, j.dir, j.branch)
	}
	if head, herr := p.Workspace.Head(ctx, j.dir); herr == nil {
		last.Commit = &head
	}
	last.Outcome, last.Secrets = "pushed", stepSecrets(found)
	switch {
	case errors.Is(err, workspace.ErrSecret):
		last.Outcome, last.Error = "secret", ptr(err.Error())
	case err != nil:
		last.Outcome, last.Error = "not_pushed", ptr(err.Error())
	}
	return last
}

// stepSecrets is what the push scan found, for a step's report.
func stepSecrets(found []workspace.Secret) []cinderline.StepSecret {
	var out []cinderline.StepSecret
	for _, f := range found {
		s := cinderline.StepSecret{Kind: f.Kind, Commit: f.Commit, Known: f.Known}
		if f.Path != "" {
			s.Path = &f.Path
		}
		out = append(out, s)
	}
	return out
}

// subRef is what follows the parent's ref in a sub-issue's.
var subRef = regexp.MustCompile(`^(\.[0-9]+)+$`)

// inTree reports whether issue is ref or one of its sub-issues. A step's
// issue becomes an attachment's and a commit message's, so nothing else passes.
func inTree(ref, issue string) bool {
	rest, ok := strings.CutPrefix(issue, ref)
	return ok && (rest == "" || subRef.MatchString(rest))
}

// runStep does a run step: the team's named agent works on the run's issue
// or one of its sub-issues. What it left on a sub-issue is committed as
// "<ref>: <title>" and pushed before the next request, so a lost clone never
// loses it.
func (p *Project) runStep(ctx context.Context, c *cinderline.Client, j *job, h *held, step cinderline.Step) (*cinderline.LastStep, error) {
	if step.Issue == nil || !inTree(j.ref, step.Issue.Ref) {
		return nil, malformed(c, step.Kind, "its issue is not "+j.ref+" or one of its sub-issues")
	}
	issue := step.Issue.Ref
	last, _, err := p.agentTurn(ctx, c, j, h, step, issue)
	if err != nil || issue == j.ref || last.Outcome == "not_started" || j.manager {
		return last, err
	}
	if _, err := p.Workspace.Commit(ctx, j.dir, gitAuthor(j.team, step.Agent), issue+": "+step.Issue.Title); err != nil {
		return last, err
	}
	// A failed push only logs; the final push reports a lasting problem.
	if _, err := p.Workspace.Push(ctx, j.dir, j.branch); err != nil {
		p.Log.Warn("push failed", "project", p.Config.Key, "issue", issue, "err", err)
	}
	return last, nil
}

// resolveStep has the named agent resolve the conflict the last request
// reported. The runner checks the result itself: finished is a merge it
// verified; any other outcome, and not_verified, is undone, so a
// half-resolved merge is never pushed.
func (p *Project) resolveStep(ctx context.Context, c *cinderline.Client, j *job, h *held, step cinderline.Step, cf *conflict) (*cinderline.LastStep, error) {
	log := p.Log.With("project", p.Config.Key, "issue", j.ref)
	j.blocked = ""
	last, res, err := p.agentTurn(ctx, c, j, h, step, j.ref)
	if err != nil || last.Outcome == "not_started" {
		return last, err
	}
	if last.Outcome == "finished" {
		ok, verr := !reportsBlocked(res.Report), error(nil)
		if !ok {
			j.blocked, _, _ = strings.Cut(strings.TrimSpace(res.Report), "\n")
			j.blocked = strings.TrimSpace(j.blocked)
		}
		if ok {
			ok, verr = verifyMerge(ctx, p.Workspace, j.dir, j.base, cf.head, cf.files)
		}
		if ok {
			log.Info("merge resolved", "base", j.base)
			return last, nil
		}
		why := "the runner could not verify the merge with " + j.base + ", so it undid it"
		if verr != nil {
			why += " (" + verr.Error() + ")"
		}
		last.Outcome, last.Error = "not_verified", &why
	}
	if err := p.Workspace.Reset(ctx, j.dir, cf.head); err != nil {
		return last, err
	}
	log.Info("merge not resolved, undone", "base", j.base, "conflicts", cf.files)
	return last, nil
}

// agentTurn runs the turn of a run or resolve step on issue: the named agent
// with the step's prompt and token, in a new session or the one the run
// holds. A turn it cannot start is the not_started outcome; an error is the
// runner's own failure.
func (p *Project) agentTurn(ctx context.Context, c *cinderline.Client, j *job, h *held, step cinderline.Step, issue string) (*cinderline.LastStep, client.Result, error) {
	last := &cinderline.LastStep{Step: step.Kind, Issue: issue, Agent: ptr(step.Agent)}
	notStarted := func(err error) (*cinderline.LastStep, client.Result, error) {
		p.Log.Warn("turn not started", "project", p.Config.Key, "issue", issue, "err", err)
		last.Outcome, last.Error = "not_started", ptr(err.Error())
		return last, client.Result{}, nil
	}
	turn := client.Turn{Dir: j.dir, Attachments: attachmentsDir(p.State, p.Config, j.ref), Prompt: step.Prompt, MaxTurns: j.team.MaxTurns, MaxMinutes: j.team.MaxMinutes, Skills: j.skills, Idle: j.idle, Allow: p.allowHosts(issue, step.AllowHosts), Tools: j.tools}
	var prev *client.Usage // the resumed session's usage total before the turn
	switch step.Session {
	case "new":
	case "resume":
		s := h.session(issue, step.Agent)
		if turn.SessionID, prev = s.id, s.total; turn.SessionID == "" {
			return nil, client.Result{}, malformed(c, step.Kind, "it resumes a session the runner does not hold")
		}
	default:
		return nil, client.Result{}, malformed(c, step.Kind, "unknown session "+step.Session)
	}
	i := slices.IndexFunc(j.team.Agents, func(a cinderline.TeamAgent) bool { return a.Name == step.Agent })
	switch {
	case i < 0:
		return notStarted(fmt.Errorf("agent %s is not on team %s", step.Agent, j.team.Name))
	case p.Clients[j.team.Agents[i].Client] == nil:
		return notStarted(fmt.Errorf("agent %s runs on %s, which this runner does not run", step.Agent, j.team.Agents[i].Client))
	case strings.TrimSpace(step.Prompt) == "":
		return notStarted(errors.New("the step has no prompt"))
	}
	agent := j.team.Agents[i]
	turn.Model, turn.Effort, turn.Author = deref(agent.Model), deref(agent.Effort), gitAuthor(j.team, agent.Name)
	if step.Token != nil {
		redact.Add("agent token", *step.Token)
		turn.Token = *step.Token
	}
	if step.Attachments != nil {
		if err := p.stepAttachments(ctx, c, j.ref, step.Attachments); err != nil {
			return notStarted(err)
		}
	}

	// The wait for the provider is part of the step, so the poll's stop ends it too.
	tctx, end := p.stoppable(ctx, j.ref)
	res, err := func() (client.Result, error) {
		if pr, ok := p.turnProvider(agent); ok && p.Turns != nil {
			waited := false
			waiting := func() {
				p.Log.Info("turn waits for its model provider", "project", p.Config.Key, "issue", issue, "agent", agent.Name, "provider", pr.Name, "max_agents", pr.MaxAgents)
				p.setWaiting(j.ref, pr.Name)
				waited = true
			}
			err := p.Turns.take(tctx, j.slot, pr.Name, pr.MaxAgents, waiting)
			if waited {
				p.setWaiting(j.ref, "") // with a place, or ended by the stop or ctx
			}
			if err != nil {
				return client.Result{}, err
			}
			defer p.Turns.done(pr.Name)
		}
		p.Log.Info("turn started", "project", p.Config.Key, "issue", issue, "agent", agent.Name, "session", turn.SessionID)
		return p.Clients[agent.Client].Run(tctx, turn)
	}()
	stopped := end()
	if ctx.Err() != nil {
		return last, res, ctx.Err()
	}
	if errors.Is(err, client.ErrNotStarted) {
		return notStarted(err)
	}
	res.SessionID = cmp.Or(res.SessionID, turn.SessionID)
	var total *client.Usage
	if res.UsageTotal && res.Usage != nil {
		// The app adds up usage per turn: send what the turn added to the
		// session's total (Willem on CLR-232, decision 2026-11-24-codex-usage-difference).
		total = res.Usage
		if prev != nil && res.SessionID == turn.SessionID {
			u := total.Since(*prev)
			res.Usage = &u
		}
	}
	h.turned(issue, agent.Name, res.SessionID, res.Report, total)
	last.Usage = usage(issue, agent.Name, turn.Model, res.Usage, res.Tools)
	if res.Report != "" {
		last.Report = ptr(res.Report)
	}
	var oom *environment.OutOfMemoryError
	switch {
	case err == nil:
		last.Outcome = "finished"
		p.Log.Info("turn finished", "project", p.Config.Key, "issue", issue, "session", res.SessionID)
		return last, res, nil
	case stopped:
		last.Outcome = "stopped"
	case errors.Is(err, environment.ErrIdle):
		last.Outcome = "idle"
	case step.Kind == "run" && res.HitUsageLimit:
		last.Outcome = "usage_limit"
		if !res.ResetsAt.IsZero() {
			last.ResetsAt = ptr(res.ResetsAt.UTC().Format(time.RFC3339))
		}
	case res.HitTurnLimit:
		last.Outcome = "turn_limit"
	case errors.Is(err, environment.ErrTimeout):
		last.Outcome = "time_limit"
	case step.Kind == "run" && errors.As(err, &oom): // resolve does not take it (app decision 2026-11-24-out-of-memory-outcome)
		last.Outcome, last.MemoryLimit = "out_of_memory", &oom.Limit
	default:
		last.Outcome = "failed"
	}
	p.Log.Warn("turn ended", "project", p.Config.Key, "issue", issue, "outcome", last.Outcome, "err", err)
	last.Error = ptr(err.Error())
	if res.Tail != "" {
		last.Tail = ptr(res.Tail)
	}
	return last, res, nil
}

// turnProvider is the runner's provider agent's turns go to: an OpenCode
// agent's model "<provider>/<model id>", read as OpenCode reads it.
// OpenRouter and the other clients' providers are not the runner's.
func (p *Project) turnProvider(agent cinderline.TeamAgent) (config.Provider, bool) {
	n, _, ok := strings.Cut(deref(agent.Model), "/")
	if agent.Client != "opencode" || !ok {
		return config.Provider{}, false
	}
	i := slices.IndexFunc(p.Providers, func(pr config.Provider) bool { return pr.Name == n })
	if i < 0 {
		return config.Provider{}, false
	}
	return p.Providers[i], true
}

// stoppable is ctx for a step of run ref that the poll's stop may end: a
// turn, a check, a publish. The step calls end when it is done; end reports
// whether a stop came. A stop between steps is ignored. Two runs may have a
// step in flight at once, so the cancels are per run (CLR-194).
func (p *Project) stoppable(ctx context.Context, ref string) (sctx context.Context, end func() bool) {
	sctx, cancel := context.WithCancelCause(ctx)
	p.setTurn(ref, cancel)
	return sctx, func() bool {
		p.setTurn(ref, nil)
		cancel(nil)
		return stopped(sctx)
	}
}

// stopped reports whether the poll's stop ended ctx from stoppable.
func stopped(ctx context.Context) bool { return errors.Is(context.Cause(ctx), errStopped) }

// setTurn records the cancel of the step in flight of run ref; nil: none.
func (p *Project) setTurn(ref string, cancel context.CancelCauseFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cancel == nil {
		delete(p.stopRuns, ref)
		return
	}
	if p.stopRuns == nil {
		p.stopRuns = map[string]context.CancelCauseFunc{}
	}
	p.stopRuns[ref] = cancel
}

// setWaiting records that run ref's turn waits for a place on provider;
// "": it no longer does. The poll reports it as waiting (CLR-254), and the
// loop polls at once so the app hears it before poll_after. CLR-254 keeps
// that to a signal the loop already selects on, placesFreed, which every
// project's loop shares, so the other projects poll once for nothing.
func (p *Project) setWaiting(ref, provider string) {
	p.mu.Lock()
	if provider == "" {
		delete(p.waits, ref)
	} else {
		if p.waits == nil {
			p.waits = map[string]string{}
		}
		p.waits[ref] = provider
	}
	p.mu.Unlock()
	p.Turns.wake()
}

// waitingRuns are the runs whose turn waits for a place on a provider, by
// ref; nil: none.
func (p *Project) waitingRuns() []cinderline.WaitingRun {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []cinderline.WaitingRun
	for _, ref := range slices.Sorted(maps.Keys(p.waits)) {
		out = append(out, cinderline.WaitingRun{Ref: ref, Provider: p.waits[ref]})
	}
	return out
}

// stopTurn ends the step in flight of run ref, if any, as stopped.
func (p *Project) stopTurn(ref string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cancel := p.stopRuns[ref]; cancel != nil {
		p.Log.Info("step stopped by Cinderline", "project", p.Config.Key, "issue", ref)
		cancel(errStopped)
	}
}

// usage is one turn's usage on issue with its tool calls by name; nil
// without usage, which drops the tool calls too: the app counts them with
// the usage entry (decision 2026-11-24-tool-use-counts).
func usage(issue, agent, model string, u *client.Usage, tools map[string]int) *cinderline.Usage {
	if u == nil {
		return nil
	}
	e := cinderline.Usage{
		Issue: issue, Agent: agent,
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
		CostUSD: u.CostUSD,
	}
	if model != "" {
		e.Model = &model
	}
	for _, name := range slices.Sorted(maps.Keys(tools)) {
		e.Tools = append(e.Tools, cinderline.ToolCalls{Name: name, Calls: tools[name]})
	}
	return &e
}

// checkStep commits and pushes the clone, then runs the project's check
// command in it. The check passed, failed, was stopped by the poll's stop,
// or did not start. Its facts are
// the last turn's on the step's issue: the agent whose work it checks and
// its report.
func (p *Project) checkStep(ctx context.Context, c *cinderline.Client, j *job, h *held, step cinderline.Step) (*cinderline.LastStep, error) {
	if step.Issue == nil || !inTree(j.ref, step.Issue.Ref) {
		return nil, malformed(c, step.Kind, "its issue is not "+j.ref+" or one of its sub-issues")
	}
	issue, title := step.Issue.Ref, step.Issue.Title
	agent := cmp.Or(h.agents[issue], j.team.Agents[0].Name)
	last := &cinderline.LastStep{Step: "check", Issue: issue, Agent: &agent}
	if r := h.reports[issue]; r != "" {
		last.Report = ptr(r)
	}
	fail := func(err error) (*cinderline.LastStep, error) {
		last.Outcome, last.Error = "not_started", ptr(err.Error())
		return last, nil
	}
	if j.check == "" {
		return fail(errors.New("the project has no check command"))
	}
	// The work is pushed before the check can stop anything.
	if _, err := p.Workspace.Commit(ctx, j.dir, gitAuthor(j.team, agent), issue+": "+title); err != nil {
		return fail(err)
	}
	if _, err := p.Workspace.Push(ctx, j.dir, j.branch); err != nil {
		p.Log.Warn("push failed", "project", p.Config.Key, "issue", issue, "err", err)
	}
	cctx, end := p.stoppable(ctx, j.ref)
	failed, tail, err := p.runCheck(cctx, j.dir, j.check, j.team.MaxMinutes, p.allowHosts(issue, step.AllowHosts))
	var oom *environment.OutOfMemoryError
	switch {
	case ctx.Err() != nil:
		end()
		return last, ctx.Err()
	case end():
		last.Outcome = "stopped"
	case err != nil:
		return fail(err)
	case errors.As(failed, &oom):
		last.Outcome, last.MemoryLimit, last.Tail = "out_of_memory", &oom.Limit, ptr(tail)
	case failed != nil:
		last.Outcome, last.Tail = "failed", ptr(tail)
	default:
		last.Outcome = "passed"
	}
	p.Log.Info("check", "project", p.Config.Key, "issue", issue, "outcome", last.Outcome)
	return last, nil
}

// stepAttachments empties the attachments folder and downloads the step's
// files into it. A name must already be one safe file name, unique
// regardless of case, and each file must belong to ref's tree: the app's
// answer never makes a path.
func (p *Project) stepAttachments(ctx context.Context, c *cinderline.Client, ref string, files []cinderline.StepAttachment) error {
	taken := map[string]bool{}
	for _, f := range files {
		lower := strings.ToLower(f.Name) // macOS and Windows hosts ignore case
		if f.Name == "" || fileName(f.ID, f.Name) != f.Name || taken[lower] {
			return fmt.Errorf("attachment name %q is not one safe, unique file name", f.Name)
		}
		if !inTree(ref, f.Issue) {
			return fmt.Errorf("attachment %d is of %s, outside %s", f.ID, f.Issue, ref)
		}
		taken[lower] = true
	}
	if err := p.resetAttachments(ref); err != nil {
		return err
	}
	dir := attachmentsDir(p.State, p.Config, ref)
	for _, f := range files {
		if err := c.DownloadAttachment(ctx, f.Issue, f.ID, filepath.Join(dir, f.Name)); err != nil {
			return fmt.Errorf("attachment %s of %s: %w", f.Name, f.Issue, err)
		}
	}
	return nil
}

// gitAuthor is agent's git identity: the name the app gives its token,
// "<team> / <agent>" cut to 100 characters.
func gitAuthor(team cinderline.Team, agent string) string {
	r := []rune(team.Name + " / " + agent)
	return string(r[:min(len(r), 100)])
}

// leadAuthor is the lead agent's git identity, for the commits the runner makes itself.
func leadAuthor(team cinderline.Team) string {
	if len(team.Agents) == 0 {
		return ""
	}
	return gitAuthor(team, team.Agents[0].Name)
}
