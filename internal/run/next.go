package run

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// soloJob is a solo or generic run's fixed facts for its steps.
type soloJob struct {
	Ref, Title, Body string // the issue's, from the run's first read; Body is untrusted
	Branch, Base     string
	Dir              string
	Team             cinderline.Team
	Agent            cinderline.TeamAgent // the team's one agent
	MaxTurns         int
	Generic          bool
	Check            string // the project's check_command; "" for a generic run
	Forge            string
	Workspace        cinderline.RunWorkspace
	// Session is the session the run starts with, from the note TakePaused
	// took; Origin is "paused" or "kept".
	Session, Origin string
	// Token is the orchestrator token the catch-up merge already made; "": none yet.
	Token string
}

// nextWaits are the waits between tries of a later next call, as the
// outbox's (decision 2026-11-15-server-decides-steps point 8); tests set
// Client.SendWaits.
var nextWaits = []time.Duration{2 * time.Second, 8 * time.Second}

// steps runs a solo or generic run as the app decides it: it asks next at
// every step boundary and does the step it gets (decision
// 2026-11-15-server-decides-steps). It holds no policy: it sends facts and
// echoes state. handed says publish was handed to the Publish hook.
func (p *Project) steps(ctx context.Context, c *cinderline.Client, j soloJob) (handed bool, err error) {
	log := p.Log.With("project", p.Config.Key, "issue", j.Ref)
	session, origin := j.Session, j.Origin
	paused := false
	tokens := map[string]string{} // by kind, made once per run
	if j.Token != "" {
		tokens[string(cinderline.KindOrchestrator)] = j.Token
	}
	report := "" // the last turn's, for a check's facts
	author := gitAuthor(j.Team, j.Agent.Name)
	defer func() {
		if ctx.Err() != nil {
			return // a stop: the next start's recovery puts the issue back
		}
		if !paused {
			// Kept for a follow-up even when the run failed: the next run can continue it.
			p.keep(j.Ref, Paused{Dir: j.Dir, Session: session, Client: j.Agent.Client, Generic: j.Generic})
		}
		if !handed && len(tokens) > 0 {
			if derr := c.DeleteAgentTokens(ctx, j.Ref); derr != nil {
				log.Warn("agent tokens not deleted", "err", derr)
			}
		}
	}()

	req := cinderline.NextRequest{Context: "run", Workspace: j.Workspace}
	for first := true; ; first = false {
		req.Sessions = []cinderline.ResumableSession{}
		if session != "" {
			req.Sessions = append(req.Sessions, cinderline.ResumableSession{Issue: j.Ref, Agent: j.Agent.Name, Origin: origin})
		}
		resp, err := p.next(ctx, c, j.Ref, req, first)
		switch {
		case ctx.Err() != nil:
			return false, ctx.Err()
		case err != nil && first:
			// The run did not start: the note TakePaused took is kept as it was.
			paused = true
			if j.Session != "" {
				if err := p.SavePaused(j.Ref, Paused{Dir: j.Dir, Session: j.Session, Client: j.Agent.Client, Generic: j.Generic, Kept: keptAt(j.Origin)}); err != nil {
					log.Warn("paused note not written", "err", err)
				}
			}
			return false, notStarted(err)
		case err != nil:
			log.Warn("next step not reached, pausing", "err", err)
			paused = true
			p.unreachable(ctx, c, j, session, author, req.Last, err)
			return false, nil
		}
		req.State, req.Last = &resp.State, nil
		step := resp.Step
		log.Info("step", "kind", step.Kind, "reason", step.Reason, "progress", deref(step.Progress))

		switch {
		case step.Kind == "run":
			last, res, err := p.runStep(ctx, c, j, step, session, tokens)
			if err != nil {
				return false, err
			}
			if res.SessionID != "" {
				session, origin = res.SessionID, "turn"
			}
			report = cmp.Or(res.Report, report)
			req.Last = &last

		case step.Kind == "check" && !j.Generic:
			last, err := p.checkStep(ctx, j, step, author, report)
			if err != nil {
				return false, err
			}
			req.Last = &last

		case step.Kind == "publish" && !j.Generic:
			// shortcut: resolve_conflicts is not read; Publish resolves a conflict
			// with the solo agent in its session, as before, until CSR-179.
			if tokens[string(cinderline.KindOrchestrator)] == "" && len(tokens) > 0 {
				if derr := c.DeleteAgentTokens(ctx, j.Ref); derr != nil {
					log.Warn("agent tokens not deleted", "err", derr)
				}
			}
			p.setHandoff(runHandoff{
				Ref: j.Ref, Title: j.Title, Body: j.Body, Dir: j.Dir, Base: j.Base,
				Team: j.Team, MaxTurns: j.MaxTurns, Forge: j.Forge,
				Verdict: deref(step.ReportHead), Foot: deref(step.ReportFoot),
				Session: session, Token: tokens[string(cinderline.KindOrchestrator)],
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
			if err := p.SavePaused(j.Ref, Paused{Dir: j.Dir, Session: session, Client: j.Agent.Client, Generic: j.Generic}); err != nil {
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
func (p *Project) unreachable(ctx context.Context, c *cinderline.Client, j soloJob, session, author string, last *cinderline.LastStep, cause error) {
	if err := p.SavePaused(j.Ref, Paused{Dir: j.Dir, Session: session, Client: j.Agent.Client, Generic: j.Generic}); err != nil {
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
func (p *Project) preserve(ctx context.Context, j soloJob, author string) error {
	if _, err := p.Workspace.Commit(ctx, j.Dir, author, j.Ref+": "+j.Title); err != nil {
		return err
	}
	return p.Workspace.Push(ctx, j.Dir, j.Branch)
}

// lastStep is the facts of a step on ref, without an outcome yet.
func lastStep(kind, ref, agent string) cinderline.LastStep {
	return cinderline.LastStep{ID: rand.Text(), Step: kind, Issue: ref, Agent: &agent, Conflicts: []string{}}
}

// runStep does a run step: it runs the solo agent on the run's issue with
// the step's prompt, in a new session or the one the run holds, with a
// token of the step's kind. A step it cannot start is the not_started
// outcome; its error stops the run.
func (p *Project) runStep(ctx context.Context, c *cinderline.Client, j soloJob, step cinderline.Step, session string, tokens map[string]string) (cinderline.LastStep, client.Result, error) {
	last := lastStep("run", j.Ref, step.Agent)
	notStartedAs := func(err error) (cinderline.LastStep, client.Result, error) {
		p.Log.Warn("turn not started", "project", p.Config.Key, "issue", j.Ref, "err", err)
		last.Outcome, last.Error = "not_started", ptr(err.Error())
		return last, client.Result{}, nil
	}
	turn := client.Turn{
		Dir: j.Dir, Prompt: step.Prompt, MaxTurns: j.MaxTurns, MaxMinutes: j.Team.MaxMinutes,
		Model: deref(j.Agent.Model), Effort: deref(j.Agent.Effort), Author: gitAuthor(j.Team, j.Agent.Name),
		Ponytail: j.Team.Ponytail,
	}
	switch step.Session {
	case "new":
	case "resume":
		if session == "" {
			return last, client.Result{}, errors.New("the step resumes a session the runner does not hold")
		}
		turn.SessionID = session
	default:
		return last, client.Result{}, fmt.Errorf("runner %s does not know the session %q Cinderline asked for; update the runner", c.Version(), step.Session)
	}
	switch {
	case step.Issue == nil || step.Issue.Ref != j.Ref:
		return notStartedAs(errors.New("the step is not for " + j.Ref + ", and a solo team's run has no other issue"))
	case step.Agent != j.Agent.Name:
		return notStartedAs(errors.New("agent " + step.Agent + " is not the team's agent " + j.Agent.Name))
	case p.Clients[j.Agent.Client] == nil:
		return notStartedAs(fmt.Errorf("agent %s runs on %s, which this runner does not run", j.Agent.Name, j.Agent.Client))
	case strings.TrimSpace(step.Prompt) == "":
		return notStartedAs(errors.New("the step has no prompt"))
	}
	switch step.Token {
	case "none":
	case string(cinderline.KindOrchestrator), string(cinderline.KindReader):
		if tokens[step.Token] == "" {
			tok, err := c.CreateAgentToken(ctx, j.Ref, cinderline.TokenKind(step.Token), j.Agent.Name)
			if err != nil {
				return notStartedAs(err)
			}
			tokens[step.Token] = tok.Token
		}
		turn.Token = tokens[step.Token]
	default:
		return last, client.Result{}, fmt.Errorf("runner %s does not know the token kind %q Cinderline asked for; update the runner", c.Version(), step.Token)
	}
	if step.Attachments != nil {
		if err := p.stepAttachments(ctx, c, j.Ref, step.Attachments); err != nil {
			return notStartedAs(err)
		}
	}

	p.Log.Info("run started", "project", p.Config.Key, "issue", j.Ref, "session", turn.SessionID)
	res, err := p.turn(ctx, c, p.Clients[j.Agent.Client], j.Ref, turn)
	if ctx.Err() != nil {
		return last, res, ctx.Err()
	}
	res.SessionID = cmp.Or(res.SessionID, turn.SessionID)
	last.Usage = usageEntry(j.Ref, j.Agent.Name, turn.Model, res.Usage)
	if res.Report != "" {
		last.Report = ptr(res.Report)
	}
	var stop *stopError
	switch {
	case err == nil:
		last.Outcome = "finished"
		p.Log.Info("run finished", "project", p.Config.Key, "issue", j.Ref, "session", res.SessionID)
		return last, res, nil
	case errors.As(err, &stop):
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
	p.Log.Warn("run failed", "project", p.Config.Key, "issue", j.Ref, "outcome", last.Outcome, "err", err)
	last.Error = ptr(err.Error())
	if res.Tail != "" {
		last.Tail = ptr(lastRunes(res.Tail, 4000))
	}
	return last, res, nil
}

// checkStep does a check step: it commits and pushes the clone, then runs
// the project's check command in it (CSR-80). The check passed, failed, or
// did not start; its error is a stop.
func (p *Project) checkStep(ctx context.Context, j soloJob, step cinderline.Step, author, report string) (cinderline.LastStep, error) {
	last := lastStep("check", j.Ref, j.Agent.Name)
	if report != "" {
		last.Report = ptr(report)
	}
	fail := func(err error) (cinderline.LastStep, error) {
		last.Outcome, last.Error = "not_started", ptr(err.Error())
		return last, nil
	}
	switch {
	case step.Issue == nil || step.Issue.Ref != j.Ref:
		return fail(errors.New("the step is not for " + j.Ref + ", and a solo team's run has no other issue"))
	case j.Check == "":
		return fail(errors.New("the project has no check command"))
	}
	// Preserve first: the work is pushed before the check can stop anything (EMBER-143).
	if _, err := p.Workspace.Commit(ctx, j.Dir, author, j.Ref+": "+j.Title); err != nil {
		return fail(err)
	}
	if err := p.Workspace.Push(ctx, j.Dir, j.Branch); err != nil {
		p.Log.Warn("push failed", "project", p.Config.Key, "issue", j.Ref, "err", err)
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
	p.Log.Info("check", "project", p.Config.Key, "issue", j.Ref, "outcome", last.Outcome)
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
		if f.Issue != ref && !strings.HasPrefix(f.Issue, ref+".") {
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
