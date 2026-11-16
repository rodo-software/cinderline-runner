package run

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// watchEvery is how often a running agent's issue status is checked; a
// variable so tests need not wait (CSR-159). CSR-93 may let the server set it.
var watchEvery = 30 * time.Second

// defaultIdle is the idle limit while the app sends no idle_minutes.
const defaultIdle = 20 * time.Minute

// setIdle keeps proj's idle limit for the turns of the slot: Willem set it
// per project in the app, not per team (CSR-159).
func (r *runState) setIdle(proj cinderline.Project) {
	d := defaultIdle
	if m := proj.IdleMinutes; m != nil && *m > 0 {
		d = time.Duration(*m) * time.Minute
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.idle = d
}

// idleLimit is the last setIdle's limit; 0, none, on a nil runState.
func (r *runState) idleLimit() time.Duration {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.idle
}

// stopStatuses stop a running agent when a person moves its issue there.
var stopStatuses = map[string]bool{"blocked": true, "cancelled": true, "backlog": true}

// errHalted is a run the runner stopped before or during an agent turn:
// the work is pushed and the issue has its note. The caller keeps the
// paused note and ends the run as paused, as after a question.
var errHalted = errors.New("the run was halted")

// stopError is a turn the runner stopped: why says so in the note; block
// moves the issue to Blocked, as a person did not move it.
type stopError struct {
	why   string
	block bool
	err   error // the client's error
}

func (e *stopError) Error() string { return "stopped: " + e.why }
func (e *stopError) Unwrap() error { return e.err }

// turn runs t on cl and checks ref's status every watchEvery meanwhile. A
// move to a stopStatuses status stops the turn; so does the Environment's
// idle limit. Both return a *stopError, for the caller to undo what must not
// be kept and then call halt. Right after an agent's own comment, as its
// question to a person, Blocked gets one more check: the agent is ending
// its turn.
func (p Project) turn(ctx context.Context, c *cinderline.Client, cl client.Client, ref string, t client.Turn) (client.Result, error) {
	t.Idle = p.run.idleLimit()
	run, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		tick := time.NewTicker(watchEvery)
		defer tick.Stop()
		grace := false
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			issue, err := c.GetIssue(run, ref)
			if err != nil {
				continue // a failed check waits for the next one
			}
			s := issue.Status
			if !stopStatuses[s.Value] {
				grace = false
				continue
			}
			if n := len(issue.Comments); s.Value == "blocked" && !grace && n > 0 && issue.Comments[n-1].Author.Agent != nil {
				grace = true
				continue
			}
			stop(&stopError{why: "the issue was moved to " + s.Label})
			return
		}
	})
	res, err := cl.Run(run, t)
	close(done)
	wg.Wait()
	if ctx.Err() != nil || err == nil {
		return res, err
	}
	var moved *stopError
	if errors.As(context.Cause(run), &moved) {
		moved.err = err
		return res, moved
	}
	if errors.Is(err, environment.ErrIdle) {
		return res, &stopError{why: "it was " + err.Error(), block: true, err: err}
	}
	return res, err
}

// halt ends a stopped turn: it commits and pushes what the clone holds, so
// no work is lost (preserve first, report second, EMBER-143), moves ref to
// Blocked when the runner stopped it on its own, and leaves a note with the
// branch, the commit and the agent's last report. A failed push is said in
// the note; the clone keeps the commit and the next start pushes it.
func (p Project) halt(ctx context.Context, c *cinderline.Client, ws workspace.Workspace, ref string, t client.Turn, res client.Result, stop *stopError) error {
	log := p.Log.With("project", p.Config.Key, "issue", ref)
	branch := name.Name + "/" + ref
	if _, err := ws.Commit(ctx, t.Dir, t.Author, ref+": work until the runner stopped the agent"); err != nil {
		log.Warn("stopped work not committed", "err", err)
	}
	head, err := ws.Head(ctx, t.Dir)
	if err != nil {
		head = "(unknown)"
	}
	where := "is pushed to"
	if err := ws.Push(ctx, t.Dir, branch); err != nil {
		log.Warn("stopped work not pushed", "err", err)
		where = "is not pushed yet; the runner pushes it at its next start. It waits in the run's clone, on"
	}
	if stop.block {
		move(ctx, c, ref, "blocked", "in_progress")
	}
	note := fmt.Sprintf("**Review this manually**: the runner stopped the agent because %s. Its work, commit %s, %s branch `%s`.", stop.why, head, where, branch)
	if res.Report != "" {
		note += "\n\nThe agent's last report:\n\n" + res.Report
	}
	note += "\n\nMove the issue to To do to let the agent go on from there."
	p.comment(ctx, c, ref, note)
	log.Warn("agent stopped", "why", stop.why, "commit", head)
	return fmt.Errorf("%w: %s", errHalted, stop.why)
}
