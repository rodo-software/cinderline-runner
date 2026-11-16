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
	"slices"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// runJob is a run's fixed facts for its steps.
type runJob struct {
	Ref, Title, Body string // the issue's, from the run's first read; Body is untrusted
	Branch, Base     string
	Dir              string
	Team             cinderline.Team // the API lists the lead first: the solo agent or the orchestrator
	MaxTurns         int
	Generic          bool
	Check            string // the project's check_command; "" for a generic run
	Forge            string
	Workspace        cinderline.RunWorkspace
	// Session is the lead's session the run starts with, from the note it
	// took; Origin is "paused" or "kept".
	Session, Origin string
	// Conflict is the catch-up merge's, which the first request reports; nil: none.
	Conflict *conflict
}

// session is a client session the run can resume: an agent's on an issue.
type session struct{ Issue, Agent, ID, Origin string }

// runHeld is what a run holds between its steps, in memory only: the lead's
// session on the run's issue, the last turn's when that is another's (one
// session per agent and issue, 0122), the agent tokens by issue and kind,
// each made once, and each issue's last report and agent, for a check.
type runHeld struct {
	lead, other session
	tokens      map[[2]string]string
	reports     map[string]string
	agents      map[string]string
}

// sessions are the sessions for next, the lead's first.
func (h *runHeld) sessions() []cinderline.ResumableSession {
	out := []cinderline.ResumableSession{}
	for _, s := range []session{h.lead, h.other} {
		if s.ID != "" {
			out = append(out, cinderline.ResumableSession{Issue: s.Issue, Agent: s.Agent, Origin: s.Origin})
		}
	}
	return out
}

// session is the session the run holds for agent on issue; "": none.
func (h *runHeld) session(issue, agent string) string {
	for _, s := range []session{h.lead, h.other} {
		if s.Issue == issue && s.Agent == agent {
			return s.ID
		}
	}
	return ""
}

// turned records a turn of agent on issue that ended in session id.
func (h *runHeld) turned(issue, agent, id, report string) {
	switch {
	case issue == h.lead.Issue && agent == h.lead.Agent:
		if id != "" {
			h.lead.ID, h.lead.Origin = id, "turn"
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

// nextWaits are the waits between tries of a later next call, as the
// outbox's (decision 2026-11-15-server-decides-steps point 8); tests set
// Client.SendWaits.
var nextWaits = []time.Duration{2 * time.Second, 8 * time.Second}

// steps runs a run as the app decides it: it asks next at every step
// boundary and does the step it gets (decision
// 2026-11-15-server-decides-steps). A solo, generic and team run differ only
// in the steps the app sends. It holds no policy: it sends facts and echoes
// state. handed says publish was handed to the Publish hook.
func (p *Project) steps(ctx context.Context, c *cinderline.Client, j runJob) (handed bool, err error) {
	log := p.Log.With("project", p.Config.Key, "issue", j.Ref)
	lead := j.Team.Agents[0]
	team := len(j.Team.Agents) > 1
	h := &runHeld{
		lead:    session{Issue: j.Ref, Agent: lead.Name, ID: j.Session, Origin: j.Origin},
		tokens:  map[[2]string]string{},
		reports: map[string]string{},
		agents:  map[string]string{},
	}
	note := func() Paused { return Paused{Dir: j.Dir, Session: h.lead.ID, Client: lead.Client, Generic: j.Generic} }
	paused := false
	author := gitAuthor(j.Team, lead.Name)
	defer func() {
		if ctx.Err() != nil {
			return // a stop: the next start's recovery moves the issue to Blocked
		}
		if !paused {
			// Kept for a follow-up even when the run failed: the next run can continue it.
			p.keep(j.Ref, note())
		}
		p.dropTokens(ctx, c, h, func(string) bool { return false })
	}()

	req := cinderline.NextRequest{Context: "run", Workspace: j.Workspace}
	pending := j.Conflict // the conflict the request reports, for a resolve step
	if pending != nil {
		req.Last = &pending.last
	}
	for first := true; ; first = false {
		req.Sessions = h.sessions()
		resp, err := p.next(ctx, c, j.Ref, req, first)
		switch {
		case ctx.Err() != nil:
			return false, ctx.Err()
		case err != nil && first:
			// The run did not start: the note it took is kept as it was; a team's stayed.
			paused = true
			if j.Session != "" && !team {
				if err := p.SavePaused(j.Ref, Paused{Dir: j.Dir, Session: j.Session, Client: lead.Client, Generic: j.Generic, Kept: keptAt(j.Origin)}); err != nil {
					log.Warn("paused note not written", "err", err)
				}
			}
			return false, notStarted(err)
		case err != nil:
			log.Warn("next step not reached, pausing", "err", err)
			paused = true
			p.unreachable(ctx, c, j, note(), author, req.Last, err)
			return false, nil
		}
		req.State, req.Last = &resp.State, nil
		step := resp.Step
		conflict := pending
		pending = nil
		log.Info("step", "kind", step.Kind, "reason", step.Reason, "progress", deref(step.Progress))

		switch {
		case step.Kind == "run":
			last, err := p.runStep(ctx, c, j, step, h)
			if err != nil {
				return false, err
			}
			req.Last = &last

		case step.Kind == "resolve" && conflict != nil:
			last, err := p.resolveStep(ctx, c, j, step, conflict, h)
			if err != nil {
				return false, err
			}
			req.Last = &last

		case step.Kind == "check" && !j.Generic:
			last, err := p.checkStep(ctx, j, step, h)
			if err != nil {
				return false, err
			}
			req.Last = &last

		case step.Kind == "publish" && !j.Generic:
			if step.ResolveConflicts {
				// The base merge Publish does next, here, so its conflict can go to next.
				if _, err := p.Workspace.Commit(ctx, j.Dir, author, pullRequestTitle(j.Ref, j.Title)); err != nil {
					return false, err
				}
				cf, err := UpdateBranch(ctx, p.Workspace, j.Ref, j.Dir, j.Base, author)
				if err != nil {
					log.Warn("base not merged in, publish tries again", "err", err)
				}
				if cf != nil {
					pending, req.Last = cf, &cf.last
					continue
				}
			}
			p.dropTokens(ctx, c, h, func(string) bool { return false })
			p.setHandoff(runHandoff{
				Ref: j.Ref, Title: j.Title, Body: j.Body, Dir: j.Dir, Base: j.Base, Team: j.Team, Forge: j.Forge,
				Verdict: deref(step.ReportHead), Foot: deref(step.ReportFoot),
			})
			return true, nil

		case step.Kind == "finish":
			if !j.Generic {
				if err := p.preserve(ctx, j, author); err != nil {
					return false, err
				}
			}
			if step.Comment != nil {
				p.comment(ctx, c, j.Ref, *step.Comment)
			}
			// Refused when the issue is no longer In progress, as in Publish (decision 0116).
			move(ctx, c, j.Ref, "review", "in_progress")
			return false, nil

		case step.Kind == "pause":
			paused = true
			if err := p.SavePaused(j.Ref, note()); err != nil {
				log.Warn("paused note not written", "err", err)
			}
			if step.Block {
				// Preserve first, report second (EMBER-143); a generic run publishes nothing (CSR-75).
				if !j.Generic {
					if err := p.preserve(ctx, j, author); err != nil {
						return false, err
					}
				}
				move(ctx, c, j.Ref, "blocked", "in_progress")
			}
			if step.Comment != nil {
				// Blocked already: a lost comment must not drop the paused note.
				p.comment(ctx, c, j.Ref, *step.Comment)
			}
			return false, nil

		case step.Kind == "fail":
			// scheduler.finish posts it and moves the issue to Blocked, as for any run error.
			return false, errors.New(cmp.Or(deref(step.Comment), "Cinderline ended the run: "+step.Reason))

		default:
			// Never worse than a stop with an error (decision 2026-11-15-server-decides-steps point 3).
			if !j.Generic {
				if err := p.preserve(ctx, j, author); err != nil {
					log.Warn("work not pushed before the unknown step's stop", "err", err)
				}
			}
			return false, fmt.Errorf("runner %s cannot do the %q step Cinderline asked for in this run; update the runner", c.Version(), step.Kind)
		}
		if team && h.lead.ID != "" {
			// After every step, so a restart during the sub-issues resumes the orchestrator (0122).
			if err := p.SavePaused(j.Ref, note()); err != nil {
				log.Warn("paused note not written", "err", err)
			}
		}
	}
}

// dropTokens deletes the agent tokens of each issue keep says no to.
func (p *Project) dropTokens(ctx context.Context, c *cinderline.Client, h *runHeld, keep func(issue string) bool) {
	for k := range h.tokens {
		if keep(k[0]) {
			continue
		}
		if err := c.DeleteAgentTokens(ctx, k[0]); err != nil {
			p.Log.Warn("agent tokens not deleted", "project", p.Config.Key, "issue", k[0], "err", err)
		}
		maps.DeleteFunc(h.tokens, func(o [2]string, _ string) bool { return o[0] == k[0] })
	}
}

// keptAt is the Kept time of a note TakePaused took, so putting it back
// keeps its kind: now for a kept session, zero for a paused one.
func keptAt(origin string) time.Time {
	if origin == "kept" {
		return time.Now()
	}
	return time.Time{}
}

// next asks the app for the next step. The first call of a run is tried
// once; a later one that Cinderline did not answer is tried again after
// nextWaits.
func (p *Project) next(ctx context.Context, c *cinderline.Client, ref string, req cinderline.NextRequest, first bool) (cinderline.NextResponse, error) {
	waits := nextWaits
	if c.SendWaits != nil {
		waits = c.SendWaits
	}
	if first {
		waits = nil
	}
	for i := 0; ; i++ {
		resp, err := c.Next(ctx, ref, req)
		if err == nil || i == len(waits) || !cinderline.Unavailable(err) {
			return resp, err
		}
		select {
		case <-ctx.Done():
			return resp, ctx.Err()
		case <-time.After(waits[i]):
		}
	}
}

// unreachable ends a run whose next call failed after its tries as a pause
// with block: the work is pushed, the session kept in the paused note, and
// through the outbox the issue goes to Blocked with the runner's own
// comment. The last turn's usage, which next did not deliver, goes with
// reportUsage at the run's end (decision 2026-11-15-server-decides-steps
// point 8).
func (p *Project) unreachable(ctx context.Context, c *cinderline.Client, j runJob, note Paused, author string, last *cinderline.LastStep, cause error) {
	if err := p.SavePaused(j.Ref, note); err != nil {
		p.Log.Warn("paused note not written", "project", p.Config.Key, "issue", j.Ref, "err", err)
	}
	where := ""
	if !j.Generic {
		where = " The work is pushed to branch `" + j.Branch + "`."
		if err := p.preserve(ctx, j, author); err != nil {
			p.Log.Warn("work not pushed", "project", p.Config.Key, "issue", j.Ref, "err", err)
			where = " The work is not pushed yet; the runner pushes it at its next start."
		}
	}
	if last != nil && last.Usage != nil {
		p.run.addEntry(j.Ref, *last.Usage)
	}
	move(ctx, c, j.Ref, "blocked", "in_progress")
	p.comment(ctx, c, j.Ref, "**Cinderline did not answer** the runner's request for the run's next step ("+cause.Error()+"), so the runner stopped the run."+where+" Move the issue to To do to continue.")
}

// preserve commits what the clone holds and pushes the branch.
func (p *Project) preserve(ctx context.Context, j runJob, author string) error {
	if _, err := p.Workspace.Commit(ctx, j.Dir, author, j.Ref+": "+j.Title); err != nil {
		return err
	}
	return p.Workspace.Push(ctx, j.Dir, j.Branch)
}

// lastStep is the facts of a step on issue, without an outcome yet.
func lastStep(kind, issue, agent string) cinderline.LastStep {
	return cinderline.LastStep{ID: rand.Text(), Step: kind, Issue: issue, Agent: &agent, Conflicts: []string{}}
}

// subRef is what follows the parent's ref in a sub-issue's, as the API's
// ref pattern has it.
var subRef = regexp.MustCompile(`^(\.[0-9]+)+$`)

// inTree reports whether issue is ref or one of its sub-issues. A step's
// issue becomes a token's, an attachment's and a commit message's, so
// nothing else passes.
func inTree(ref, issue string) bool {
	rest, ok := strings.CutPrefix(issue, ref)
	return ok && (rest == "" || subRef.MatchString(rest))
}

// runStep does a run step: the team's named agent works on the run's issue
// or one of its sub-issues. What it left on a sub-issue is committed as
// "<ref>: <title>" and pushed before the next request, so a lost clone never
// loses it (0122). A step it cannot start is the not_started outcome; its
// error stops the run.
func (p *Project) runStep(ctx context.Context, c *cinderline.Client, j runJob, step cinderline.Step, h *runHeld) (cinderline.LastStep, error) {
	if step.Issue == nil || !inTree(j.Ref, step.Issue.Ref) {
		last := lastStep("run", j.Ref, step.Agent)
		last.Outcome, last.Error = "not_started", ptr("the step is not for "+j.Ref+" or one of its sub-issues")
		return last, nil
	}
	issue := step.Issue.Ref
	last, _, err := p.agentTurn(ctx, c, j, step, issue, h, nil)
	if err != nil || issue == j.Ref || j.Generic || last.Outcome == "not_started" || last.Outcome == "stopped" {
		return last, err // halt pushed a stopped turn's work
	}
	if _, err := p.Workspace.Commit(ctx, j.Dir, gitAuthor(j.Team, step.Agent), issue+": "+step.Issue.Title); err != nil {
		return last, err
	}
	// A failed push only logs; the final push reports a lasting problem.
	if err := p.Workspace.Push(ctx, j.Dir, j.Branch); err != nil {
		p.Log.Warn("push failed", "project", p.Config.Key, "issue", issue, "err", err)
	}
	return last, nil
}

// resolveStep does a resolve step: the named agent resolves the conflict
// the last request reported, in the run's clone. The runner checks the
// result itself and undoes it unless the report does not say Blocked and
// verifyMerge holds; finished is a verified merge, not_verified one it
// undid. Never a retry of its own (decision
// 2026-11-04-agent-resolves-merge-conflicts).
func (p *Project) resolveStep(ctx context.Context, c *cinderline.Client, j runJob, step cinderline.Step, cf *conflict, h *runHeld) (cinderline.LastStep, error) {
	log := p.Log.With("project", p.Config.Key, "issue", j.Ref)
	// A half-resolved merge must never be pushed: back to the head before it.
	undo := func() error { return p.Workspace.Reset(ctx, j.Dir, cf.head) }
	last, res, err := p.agentTurn(ctx, c, j, step, j.Ref, h, undo)
	if err != nil {
		return last, err
	}
	switch last.Outcome {
	case "not_started", "stopped":
		return last, nil // the merge was aborted, or agentTurn undid it
	case "finished":
		ok, verr := !reportsBlocked(res.Report), error(nil)
		if ok {
			ok, verr = verifyMerge(ctx, p.Workspace, j.Dir, j.Base, cf.head, cf.files)
		}
		if ok {
			log.Info("merge resolved", "base", j.Base)
			return last, nil
		}
		why := "the runner could not verify the merge with " + j.Base + ", so it undid it"
		if verr != nil {
			why += " (" + verr.Error() + ")"
		}
		last.Outcome, last.Error = "not_verified", &why
	}
	if err := undo(); err != nil {
		return last, err
	}
	log.Info("merge not resolved, undone", "base", j.Base, "conflicts", cf.files)
	return last, nil
}

// agentTurn runs the turn of a run or resolve step on issue: the named agent
// with the step's prompt, in a new session or the one the run holds, with a
// token of the step's kind made for issue. A stopped turn is undone with
// undo, if any, before halt pushes the work. A step it cannot start is the
// not_started outcome; its error stops the run.
func (p *Project) agentTurn(ctx context.Context, c *cinderline.Client, j runJob, step cinderline.Step, issue string, h *runHeld, undo func() error) (cinderline.LastStep, client.Result, error) {
	last := lastStep(step.Kind, issue, step.Agent)
	notStartedAs := func(err error) (cinderline.LastStep, client.Result, error) {
		p.Log.Warn("turn not started", "project", p.Config.Key, "issue", issue, "err", err)
		last.Outcome, last.Error = "not_started", ptr(err.Error())
		return last, client.Result{}, nil
	}
	turn := client.Turn{Dir: j.Dir, Prompt: step.Prompt, MaxTurns: j.MaxTurns, MaxMinutes: j.Team.MaxMinutes, Ponytail: j.Team.Ponytail}
	switch step.Session {
	case "new":
	case "resume":
		if turn.SessionID = h.session(issue, step.Agent); turn.SessionID == "" {
			return last, client.Result{}, errors.New("the step resumes a session the runner does not hold")
		}
	default:
		return last, client.Result{}, fmt.Errorf("runner %s does not know the session %q Cinderline asked for; update the runner", c.Version(), step.Session)
	}
	i := slices.IndexFunc(j.Team.Agents, func(a cinderline.TeamAgent) bool { return a.Name == step.Agent })
	switch {
	case i < 0:
		return notStartedAs(fmt.Errorf("agent %s is not on team %s", step.Agent, j.Team.Name))
	case p.Clients[j.Team.Agents[i].Client] == nil:
		return notStartedAs(fmt.Errorf("agent %s runs on %s, which this runner does not run", step.Agent, j.Team.Agents[i].Client))
	case strings.TrimSpace(step.Prompt) == "":
		return notStartedAs(errors.New("the step has no prompt"))
	}
	agent := j.Team.Agents[i]
	turn.Model, turn.Effort, turn.Author = deref(agent.Model), deref(agent.Effort), gitAuthor(j.Team, agent.Name)

	// A sub-issue's token lives until a step on another issue.
	p.dropTokens(ctx, c, h, func(ref string) bool { return ref == j.Ref || ref == issue })
	switch step.Token {
	case "none":
	case string(cinderline.KindOrchestrator), string(cinderline.KindReader):
		key := [2]string{issue, step.Token}
		if h.tokens[key] == "" {
			tok, err := c.CreateAgentToken(ctx, issue, cinderline.TokenKind(step.Token), agent.Name)
			if err != nil {
				return notStartedAs(err)
			}
			h.tokens[key] = tok.Token
		}
		turn.Token = h.tokens[key]
	default:
		return last, client.Result{}, fmt.Errorf("runner %s does not know the token kind %q Cinderline asked for; update the runner", c.Version(), step.Token)
	}
	if step.Attachments != nil {
		if err := p.stepAttachments(ctx, c, j.Ref, step.Attachments); err != nil {
			return notStartedAs(err)
		}
	}

	p.Log.Info("run started", "project", p.Config.Key, "issue", issue, "agent", agent.Name, "session", turn.SessionID)
	res, err := p.turn(ctx, c, p.Clients[agent.Client], j.Ref, turn)
	if ctx.Err() != nil {
		return last, res, ctx.Err()
	}
	res.SessionID = cmp.Or(res.SessionID, turn.SessionID)
	h.turned(issue, agent.Name, res.SessionID, res.Report)
	last.Usage = usageEntry(issue, agent.Name, turn.Model, res.Usage)
	if res.Report != "" {
		last.Report = ptr(res.Report)
	}
	var stop *stopError
	switch {
	case err == nil:
		last.Outcome = "finished"
		p.Log.Info("run finished", "project", p.Config.Key, "issue", issue, "session", res.SessionID)
		return last, res, nil
	case errors.As(err, &stop):
		if undo != nil {
			if err := undo(); err != nil {
				return last, res, err
			}
		}
		// halt pushed the work (not a generic run's) and posted its note (CSR-159).
		var ws workspace.Workspace
		if !j.Generic {
			ws = p.Workspace
		}
		_ = p.halt(ctx, c, ws, j.Ref, turn, res, stop)
		last.Outcome = "stopped"
	case res.HitTurnLimit:
		last.Outcome = "turn_limit"
	case errors.Is(err, environment.ErrTimeout):
		last.Outcome = "time_limit"
	default:
		last.Outcome = "failed"
	}
	p.Log.Warn("run failed", "project", p.Config.Key, "issue", issue, "outcome", last.Outcome, "err", err)
	last.Error = ptr(err.Error())
	if res.Tail != "" {
		last.Tail = ptr(lastRunes(res.Tail, 4000))
	}
	return last, res, nil
}

// checkStep does a check step: it commits and pushes the clone, then runs
// the project's check command in it (CSR-80). The check passed, failed, or
// did not start; its error is a stop. Its facts are the last turn's on the
// step's issue: the agent whose work it checks and its report.
func (p *Project) checkStep(ctx context.Context, j runJob, step cinderline.Step, h *runHeld) (cinderline.LastStep, error) {
	if step.Issue == nil || !inTree(j.Ref, step.Issue.Ref) {
		last := lastStep("check", j.Ref, j.Team.Agents[0].Name)
		last.Outcome, last.Error = "not_started", ptr("the step is not for "+j.Ref+" or one of its sub-issues")
		return last, nil
	}
	issue, title := step.Issue.Ref, step.Issue.Title
	if issue == j.Ref {
		title = j.Title
	}
	agent := cmp.Or(h.agents[issue], j.Team.Agents[0].Name)
	last := lastStep("check", issue, agent)
	if r := h.reports[issue]; r != "" {
		last.Report = ptr(r)
	}
	fail := func(err error) (cinderline.LastStep, error) {
		last.Outcome, last.Error = "not_started", ptr(err.Error())
		return last, nil
	}
	if j.Check == "" {
		return fail(errors.New("the project has no check command"))
	}
	// Preserve first: the work is pushed before the check can stop anything (EMBER-143).
	if _, err := p.Workspace.Commit(ctx, j.Dir, gitAuthor(j.Team, agent), issue+": "+title); err != nil {
		return fail(err)
	}
	if err := p.Workspace.Push(ctx, j.Dir, j.Branch); err != nil {
		p.Log.Warn("push failed", "project", p.Config.Key, "issue", issue, "err", err)
	}
	failed, tail, err := p.runCheck(ctx, j.Dir, j.Check, j.Team.MaxMinutes)
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

// stepAttachments empties the attachments folder and downloads the step's
// files into it. A name must already be one safe file name (fileName),
// unique regardless of case, and each file must belong to ref's tree: the
// app's answer never makes a path.
func (p *Project) stepAttachments(ctx context.Context, c *cinderline.Client, ref string, files []cinderline.StepAttachment) error {
	taken := map[string]bool{}
	for _, f := range files {
		lower := strings.ToLower(f.Name) // macOS and Windows hosts ignore case
		if f.Name == "" || fileName(cinderline.Attachment{ID: f.ID, Name: f.Name}) != f.Name || taken[lower] {
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

// runWorkspace is the facts of the run's clone and mounts for next: the
// extra repositories it fetched and the linked projects' folders.
func runWorkspace(branch, base string, extras []config.ExtraRepository, links []Linked) cinderline.RunWorkspace {
	w := cinderline.RunWorkspace{Branch: branch, Base: base, ExtraRepositories: []cinderline.WorkspaceRepository{}, LinkedProjects: []cinderline.WorkspaceLink{}}
	for _, r := range extras {
		w.ExtraRepositories = append(w.ExtraRepositories, cinderline.WorkspaceRepository{Path: workspace.ExtraPath + "/" + r.Name(), URL: stripURL(r.URL)})
	}
	for _, l := range links {
		w.LinkedProjects = append(w.LinkedProjects, cinderline.WorkspaceLink{Key: l.Key, Paths: append([]string{}, l.Paths...)})
	}
	return w
}

// lastRunes is the end of s, at most n characters.
func lastRunes(s string, n int) string {
	r := []rune(s)
	return string(r[max(0, len(r)-n):])
}

func ptr(s string) *string { return &s }
