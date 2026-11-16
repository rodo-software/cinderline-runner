package run

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/redact"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// sleep waits d or until ctx is done; a variable so tests need not wait.
var sleep = forge.Sleep

// nextWaits are the waits between the tries of a next call the app did not answer.
var nextWaits = []time.Duration{2 * time.Second, 8 * time.Second}

// The publish step's retries of the base merge, push and pull request.
const (
	publishAttempts = 5
	publishBackoff  = 30 * time.Second
)

// The publish step's wait for a merge it asked for.
const (
	mergeEvery = 15 * time.Second
	mergeLimit = 15 * time.Minute
)

// errStopped is the cause of a turn the poll's stop ended.
var errStopped = errors.New("stopped by Cinderline")

// job is a run's fixed facts for its steps.
type job struct {
	ref, title, body string // the issue's; body is untrusted
	branch, base     string
	dir              string
	team             cinderline.Team // the lead first
	check            string          // the project's check command
	idle             time.Duration   // the project's idle limit for a turn
	fg               forge.Forge
	ws               cinderline.RunWorkspace
}

func (j *job) isTeam() bool { return len(j.team.Agents) > 1 }

// session is a client session the run can resume: an agent's on an issue.
type session struct{ issue, agent, id, origin string }

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

// session is the session the run holds for agent on issue; "": none.
func (h *held) session(issue, agent string) string {
	for _, s := range []session{h.lead, h.other} {
		if s.issue == issue && s.agent == agent {
			return s.id
		}
	}
	return ""
}

// turned records a turn of agent on issue that ended in session id.
func (h *held) turned(issue, agent, id, report string) {
	switch {
	case issue == h.lead.issue && agent == h.lead.agent:
		if id != "" {
			h.lead.id, h.lead.origin = id, "turn"
		}
		h.other = session{}
	case id != "":
		h.other = session{issue, agent, id, "turn"}
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

// run prepares and does one job, following next until done. It reports
// started false when the run did not start; err then says why, or is nil
// when the app took the job back (a 409 on the first call). Nothing is
// claimed before the first call, so a run that did not start wrote nothing.
func (p *Project) run(ctx context.Context, c *cinderline.Client, proj cinderline.Project, jb cinderline.Job) (started bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			p.Log.Error("panic", "project", p.Config.Key, "issue", jb.Issue.Ref, "stack", string(debug.Stack()))
			if !started {
				err = fmt.Errorf("panic: %v", r)
			}
		}
	}()
	j, cf, h, err := p.prepareRun(ctx, proj, jb)
	if err != nil {
		return false, err
	}
	return p.steps(ctx, c, j, cf, h)
}

// prepareRun prepares the run's environment and clone. What it could not
// prepare but the run can do without goes into the workspace's notes; an
// error means the run did not start.
func (p *Project) prepareRun(ctx context.Context, proj cinderline.Project, jb cinderline.Job) (*job, *conflict, *held, error) {
	ref, team := jb.Issue.Ref, jb.Team
	// The ref becomes a branch, a folder and a file name.
	if !workspace.IssueRef.MatchString(ref) {
		return nil, nil, nil, fmt.Errorf("%q is not an issue reference", ref)
	}
	if len(team.Agents) == 0 {
		return nil, nil, nil, errors.New("the team has no agents")
	}
	lead := team.Agents[0]
	if p.Clients[lead.Client] == nil {
		return nil, nil, nil, fmt.Errorf("agent %s runs on %s, which this runner does not run", lead.Name, lead.Client)
	}
	fg, err := p.Forge(proj.Forge)
	if err != nil {
		return nil, nil, nil, err
	}
	base, err := baseBranch(ctx, proj, fg)
	if err != nil {
		return nil, nil, nil, err
	}
	if p.prepare != nil {
		if err := p.prepare(ctx, proj, base); err != nil {
			return nil, nil, nil, fmt.Errorf("the run's environment could not be prepared: %w", err)
		}
	}
	j := &job{
		ref: ref, title: jb.Issue.Title, body: jb.Issue.Body, branch: name.Name + "/" + ref, base: base,
		team: team, check: proj.CheckCommand, idle: time.Duration(max(proj.IdleMinutes, 0)) * time.Minute, fg: fg,
	}
	extras, notes := p.extraRepositories(ctx, ref)
	// Fetched on every run, so a resume and a follow-up read fresh code too.
	links, more := p.linkedProjects(ctx, ref, proj.LinkedProjects)
	notes = append(notes, more...)
	// A run's attachments come from its steps; another run's files must not linger.
	if err := p.resetAttachments(); err != nil {
		return nil, nil, nil, err
	}
	h := &held{reports: map[string]string{}, agents: map[string]string{}}
	h.lead = session{issue: ref, agent: lead.Name}
	n, resumed, more := p.resume(ctx, j, lead.Client)
	notes = append(notes, more...)
	if resumed {
		j.dir, h.lead.id, h.lead.origin = n.Dir, n.Session, n.Origin
	} else if err := p.pushBeforePrepare(ctx, j); err != nil {
		return nil, nil, nil, err
	} else if j.dir, err = p.Workspace.Prepare(ctx, j.branch, base); err != nil {
		return nil, nil, nil, err
	}
	j.ws = runWorkspace(j.branch, base, extras, links, notes)
	// Also on a fresh clone, where the merge changes nothing, as Prepare
	// does not say whether it continued the remote branch.
	return j, p.catchUp(ctx, j), h, nil
}

// resume returns the note of the session the run continues, if it can: a
// session of this client, in a clone that is still there, of the run kind
// the team has now. A kept session's clone is first fast-forwarded to the
// remote branch. What it had to drop gets a note for the workspace. A solo
// run's note is removed, so a crash during it starts fresh; a team's stays.
func (p *Project) resume(ctx context.Context, j *job, cl string) (Note, bool, []string) {
	ref := j.ref
	if !j.isTeam() {
		defer p.dropNote(ref)
	}
	n, ok := p.readNote(ref)
	switch {
	case !ok:
		return Note{}, false, nil
	case n.Pruned != "":
		p.dropNote(ref)
		return Note{}, false, []string{"The agent's earlier session on " + ref + " was deleted because " + n.Pruned + ", so this run starts a fresh session"}
	case n.Session == "":
		return Note{}, false, nil
	case n.Client != cl:
		return Note{}, false, nil
	}
	if ok, err := p.Workspace.Exists(ctx, n.Dir); !ok {
		if err != nil {
			p.Log.Warn("paused clone not checked", "project", p.Config.Key, "issue", ref, "err", err)
		}
		return Note{}, false, nil
	}
	if n.Origin != "kept" {
		return n, true, nil
	}
	if err := p.Workspace.FastForward(ctx, n.Dir, j.branch); err != nil {
		p.Log.Warn("kept session dropped: its clone is not up to date", "project", p.Config.Key, "issue", ref, "err", err)
		p.dropNote(ref)
		p.forget(ctx, ref, n.Session)
		return Note{}, false, []string{"The agent's earlier session on " + ref + " could not be continued: its clone could not be brought up to date with the remote branch " + j.branch + "; this run starts a fresh session"}
	}
	return n, true, nil
}

// pushBeforePrepare pushes the issue's clone if it holds commits no remote
// branch has, as Prepare replaces it; an error keeps the clone.
func (p *Project) pushBeforePrepare(ctx context.Context, j *job) error {
	clones, err := p.Workspace.Stranded(ctx)
	if err != nil {
		return err // unknown, so the clone may not be replaced
	}
	for _, clone := range clones {
		if clone.Branch != j.branch {
			continue
		}
		if err := p.Workspace.Push(ctx, clone.Dir, j.branch); err != nil {
			return fmt.Errorf("the clone has unpushed commits, kept: %w", err)
		}
		p.Log.Info("stranded branch pushed", "project", p.Config.Key, "issue", j.ref, "branch", j.branch)
	}
	return nil
}

// catchUp merges the base into continued work before the first step. What
// a paused turn left is committed first. It returns a conflict for the
// run's first request; an error only logs: publish tries again.
func (p *Project) catchUp(ctx context.Context, j *job) *conflict {
	log := p.Log.With("project", p.Config.Key, "issue", j.ref)
	author := leadAuthor(j.team)
	if _, err := p.Workspace.Commit(ctx, j.dir, author, j.ref+": "+j.title); err != nil {
		log.Warn("base not merged in", "err", err)
		return nil
	}
	cf, err := updateBranch(ctx, p.Workspace, j.ref, j.dir, j.base, author)
	switch {
	case err != nil:
		log.Warn("base not merged in", "err", err)
	case cf != nil:
		log.Info("base not merged in", "base", j.base, "conflicts", cf.files)
	}
	return cf
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
		case cinderline.Unavailable(err):
			log.Warn("next step not reached, run paused and its report queued", "err", err)
			p.pause(ctx, j, h)
			c.Queue(j.ref, req)
			return true, nil
		case first && errors.As(err, &taken):
			log.Info("job taken back, dropped", "err", err)
			p.keepResumed(j, h)
			return false, nil
		case first:
			p.keepResumed(j, h)
			return false, err
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
	return Note{Dir: j.dir, Session: h.lead.id, Client: j.team.Agents[0].Client, Origin: origin}
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
	if s := p.stop(ctx, j); s.Outcome == "not_pushed" {
		p.Log.Warn("work not pushed", "project", p.Config.Key, "issue", j.ref, "err", deref(s.Error))
	}
	p.keepLead(j, h, "paused")
}

// stop commits and pushes what the clone holds.
func (p *Project) stop(ctx context.Context, j *job) cinderline.LastStep {
	last := cinderline.LastStep{Step: "stop", Issue: j.ref}
	_, err := p.Workspace.Commit(ctx, j.dir, leadAuthor(j.team), j.ref+": "+j.title)
	if err == nil {
		err = p.Workspace.Push(ctx, j.dir, j.branch)
	}
	if head, herr := p.Workspace.Head(ctx, j.dir); herr == nil {
		last.Commit = &head
	}
	last.Outcome = "pushed"
	if err != nil {
		last.Outcome, last.Error = "not_pushed", ptr(err.Error())
	}
	return last
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
	if err != nil || issue == j.ref || last.Outcome == "not_started" {
		return last, err
	}
	if _, err := p.Workspace.Commit(ctx, j.dir, gitAuthor(j.team, step.Agent), issue+": "+step.Issue.Title); err != nil {
		return last, err
	}
	// A failed push only logs; the final push reports a lasting problem.
	if err := p.Workspace.Push(ctx, j.dir, j.branch); err != nil {
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
	last, res, err := p.agentTurn(ctx, c, j, h, step, j.ref)
	if err != nil || last.Outcome == "not_started" {
		return last, err
	}
	if last.Outcome == "finished" {
		ok, verr := !reportsBlocked(res.Report), error(nil)
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
	turn := client.Turn{Dir: j.dir, Prompt: step.Prompt, MaxTurns: j.team.MaxTurns, MaxMinutes: j.team.MaxMinutes, Ponytail: j.team.Ponytail, Idle: j.idle}
	switch step.Session {
	case "new":
	case "resume":
		if turn.SessionID = h.session(issue, step.Agent); turn.SessionID == "" {
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

	p.Log.Info("turn started", "project", p.Config.Key, "issue", issue, "agent", agent.Name, "session", turn.SessionID)
	tctx, cancel := context.WithCancelCause(ctx)
	p.setTurn(j.ref, cancel)
	res, err := p.Clients[agent.Client].Run(tctx, turn)
	p.setTurn("", nil)
	cancel(nil)
	if ctx.Err() != nil {
		return last, res, ctx.Err()
	}
	res.SessionID = cmp.Or(res.SessionID, turn.SessionID)
	h.turned(issue, agent.Name, res.SessionID, res.Report)
	last.Usage = usage(issue, agent.Name, turn.Model, res.Usage)
	if res.Report != "" {
		last.Report = ptr(res.Report)
	}
	switch {
	case err == nil:
		last.Outcome = "finished"
		p.Log.Info("turn finished", "project", p.Config.Key, "issue", issue, "session", res.SessionID)
		return last, res, nil
	case errors.Is(context.Cause(tctx), errStopped):
		last.Outcome = "stopped"
	case errors.Is(err, environment.ErrIdle):
		last.Outcome = "idle"
	case res.HitTurnLimit:
		last.Outcome = "turn_limit"
	case errors.Is(err, environment.ErrTimeout):
		last.Outcome = "time_limit"
	default:
		last.Outcome = "failed"
	}
	p.Log.Warn("turn ended", "project", p.Config.Key, "issue", issue, "outcome", last.Outcome, "err", err)
	last.Error = ptr(err.Error())
	if res.Tail != "" {
		last.Tail = ptr(lastRunes(res.Tail, 4000))
	}
	return last, res, nil
}

// setTurn records the cancel of the turn in flight of run ref; nil: none.
func (p *Project) setTurn(ref string, cancel context.CancelCauseFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopRef, p.stopRun = ref, cancel
}

// stopTurn ends the turn in flight of run ref, if any, as stopped.
func (p *Project) stopTurn(ref string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopRun != nil && p.stopRef == ref {
		p.Log.Info("turn stopped by Cinderline", "project", p.Config.Key, "issue", ref)
		p.stopRun(errStopped)
	}
}

// usage is one turn's usage on issue; nil without usage.
func usage(issue, agent, model string, u *client.Usage) *cinderline.Usage {
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
	return &e
}

// checkStep commits and pushes the clone, then runs the project's check
// command in it. The check passed, failed, or did not start. Its facts are
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
	if err := p.Workspace.Push(ctx, j.dir, j.branch); err != nil {
		p.Log.Warn("push failed", "project", p.Config.Key, "issue", issue, "err", err)
	}
	failed, tail, err := p.runCheck(ctx, j.dir, j.check, j.team.MaxMinutes)
	switch {
	case ctx.Err() != nil:
		return last, ctx.Err()
	case err != nil:
		return fail(err)
	case failed:
		last.Outcome, last.Tail = "failed", ptr(lastRunes(tail, 4000))
	default:
		last.Outcome = "passed"
	}
	p.Log.Info("check", "project", p.Config.Key, "issue", issue, "outcome", last.Outcome)
	return last, nil
}

// publishStep commits, merges the base in, pushes, opens or finds the pull
// request and, when asked, asks the forge to merge it and waits. With
// resolve_conflicts a conflicting base merge is reported as a merge step
// first. The base merge, push and pull request are tried publishAttempts
// times.
func (p *Project) publishStep(ctx context.Context, j *job, step cinderline.Step, pending **conflict) (*cinderline.LastStep, error) {
	log := p.Log.With("project", p.Config.Key, "issue", j.ref)
	author := leadAuthor(j.team)
	title := pullRequestTitle(j.ref, j.title)
	if _, err := p.Workspace.Commit(ctx, j.dir, author, title); err != nil {
		return nil, err
	}
	if step.ResolveConflicts {
		cf, err := updateBranch(ctx, p.Workspace, j.ref, j.dir, j.base, author)
		if err != nil {
			log.Warn("base not merged in, publish tries again", "err", err)
		}
		if cf != nil {
			*pending = cf
			return &cf.last, nil
		}
	}
	last := &cinderline.LastStep{Step: "publish", Issue: j.ref}
	var pr forge.PullRequest
	var conflicts []string
	var unchanged bool
	for attempt, wait := 1, publishBackoff; ; attempt, wait = attempt+1, wait*2 {
		var err error
		if conflicts, unchanged, pr, err = p.attempt(ctx, j, title); err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt == publishAttempts {
			last.Outcome, last.Error = "failed", ptr(err.Error())
			last.Commit = p.head(ctx, j)
			return last, nil
		}
		log.Warn("publish failed, trying again", "attempt", attempt, "retry_in", wait, "err", err)
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	last.Commit = p.head(ctx, j)
	if unchanged {
		last.Outcome = "unchanged"
		return last, nil
	}
	last.Outcome, last.PullRequest, last.Conflicts = "published", &pr.URL, conflicts
	if !step.Merge {
		return last, nil
	}
	if len(conflicts) > 0 {
		last.Merge = ptr("failed") // a conflicted pull request cannot merge
		return last, nil
	}
	if err := j.fg.RequestMerge(ctx, pr); err != nil {
		last.Merge, last.Error = ptr("failed"), ptr("asking the forge to merge "+pr.URL+" failed: "+err.Error())
		return last, nil
	}
	log.Info("waiting for the merge", "pull_request", pr.URL)
	state, err := p.awaitMerge(ctx, j.fg, j.branch)
	if err != nil {
		return nil, err
	}
	last.Merge = ptr(string(state))
	return last, nil
}

// attempt is one try of the publish: the base merge, the check whether the
// branch has anything the base lacks, the push and the pull request.
func (p *Project) attempt(ctx context.Context, j *job, title string) (conflicts []string, unchanged bool, pr forge.PullRequest, err error) {
	// Branch protection may block outdated pull requests.
	if conflicts, err = p.Workspace.MergeBase(ctx, j.dir, j.base, leadAuthor(j.team)); err != nil {
		return nil, false, pr, err
	}
	if unchanged, err = p.Workspace.IsAncestor(ctx, j.dir, "HEAD", j.base); err != nil || unchanged {
		return conflicts, unchanged, pr, err
	}
	// Built here from the ref, never passed in, so the push can never target the base.
	if err = p.Workspace.Push(ctx, j.dir, j.branch); err != nil {
		return nil, false, pr, err
	}
	pr, err = j.fg.OpenPullRequest(ctx, forge.PullRequestInput{Branch: j.branch, Base: j.base, Title: title, Body: pullRequestBody(j.ref, j.body)})
	return conflicts, false, pr, err
}

// awaitMerge polls the branch's pull request every mergeEvery until it is
// merged or closed, or mergeLimit has passed: then it is pending, or failed
// when its checks failed. A failed poll is logged and tried again.
func (p *Project) awaitMerge(ctx context.Context, fg forge.Forge, branch string) (forge.MergeState, error) {
	for waited := time.Duration(0); ; waited += mergeEvery {
		state, err := fg.MergeState(ctx, branch)
		switch {
		case ctx.Err() != nil:
			return "", ctx.Err()
		case err != nil:
			p.Log.Warn("merge not checked, trying again", "project", p.Config.Key, "branch", branch, "err", err)
		case state == forge.MergeMerged || state == forge.MergeClosed:
			return state, nil
		}
		if waited >= mergeLimit {
			if state == forge.MergeFailed {
				return state, nil
			}
			return forge.MergePending, nil
		}
		if err := sleep(ctx, mergeEvery); err != nil {
			return "", err
		}
	}
}

// head is the clone's commit; nil when unknown.
func (p *Project) head(ctx context.Context, j *job) *string {
	if h, err := p.Workspace.Head(ctx, j.dir); err == nil {
		return &h
	}
	return nil
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
	if err := p.resetAttachments(); err != nil {
		return err
	}
	dir := attachmentsDir(p.State, p.Config)
	for _, f := range files {
		if err := c.DownloadAttachment(ctx, f.Issue, f.ID, filepath.Join(dir, f.Name)); err != nil {
			return fmt.Errorf("attachment %s of %s: %w", f.Name, f.Issue, err)
		}
	}
	return nil
}

// pullRequestTitle is the title of the issue's pull request.
func pullRequestTitle(ref, title string) string { return redact.String(ref + ": " + title) }

// pullRequestBody is the body of the issue's pull request; body is the issue's text, untrusted.
func pullRequestBody(ref, body string) string {
	return redact.String(strings.TrimSpace("Resolves " + ref + " in Cinderline.\n\n" + body))
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

// lastRunes is the end of s, at most n characters.
func lastRunes(s string, n int) string {
	r := []rune(s)
	return string(r[max(0, len(r)-n):])
}
