package run

import (
	"context"
	"errors"
	"fmt"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// ContinuePrompt is what a run that hit the team's turn limit gets when it
// continues, the text of legacy lib.mjs continuePrompt.
func ContinuePrompt(ref string) string {
	return "You ran out of turns on " + ref + " and have a fresh budget. Finish the task under the same rules, starting from the work already in this worktree."
}

// CommentsPrompt is what a finished session gets when a person commented
// while it worked (decision 2026-11-03-comments-during-a-run), the text of
// legacy lib.mjs commentsPrompt.
func CommentsPrompt(ref string) string {
	return "While you worked, a person commented on " + ref + ". Read the latest comments with list_comments on " + ref + " and take them into account, under the same rules."
}

// UnreadComments is added below a team's final comment when the resume to
// read a person's comments failed twice and the finished work is published
// anyway.
const UnreadComments = "A person commented during the run, but resuming the agent to read those comments failed twice, so this result doesn't take them into account. Read them before you go on."

// retryProgress is the progress line of the run after a failed one, as
// legacy runner.mjs retryProgress.
func retryProgress(res client.Result, attempt int) string {
	if res.HitTurnLimit {
		return "Continuing after the turn limit"
	}
	return fmt.Sprintf("Retrying (attempt %d)", attempt)
}

// Job is one turn of a team's orchestrator for Attempts.
type Job struct {
	Ref    string
	Agent  string      // the team's agent, for its usage entries
	Client string      // the agent's CLI, kept in a paused note
	Turn   client.Turn // the first try
	// Workspace commits and pushes the clone when the budget or a halt stops
	// the run.
	Workspace workspace.Workspace
}

// Outcome is how Attempts or Rounds ended without an error.
type Outcome struct {
	client.Result
	Dir    string // the issue's clone dir
	Paused bool   // the issue waits for a person; the note is written and the clone stays
	Unread bool   // Rounds: reading a person's comments failed twice, publish with UnreadComments
}

// newestCommentID is the comment cursor: IDs, not timestamps, so the
// runner's clock doesn't matter (decision 2026-11-03-comments-during-a-run).
func newestCommentID(cs []cinderline.Comment) int {
	id := 0
	for _, c := range cs {
		id = max(id, c.ID)
	}
	return id
}

// personCommentedAfter reports a person's comment after the cursor; an
// agent's comments don't count.
func personCommentedAfter(cs []cinderline.Comment, cursor int) bool {
	for _, c := range cs {
		if c.Author.Agent == nil && c.ID > cursor {
			return true
		}
	}
	return false
}

// Attempts runs a team orchestrator's turn for j.Ref, at most twice (plan
// M3, 0095). A run that hit the turn limit continues its own session with a
// fresh budget, so its context is kept; any other failure starts over in a
// fresh session. After a second failure the error carries the tail of its
// output, and the scheduler posts it as a comment before moving the issue to
// Blocked, without publishing. Rounds reads the comments itself.
//
// After every run, an issue that waits for a person (Blocked, or To do after
// a quick answer) pauses the run: it writes the note and returns Paused
// (0116). A solo team's run follows the app's steps instead (decision
// 2026-11-15-server-decides-steps).
func (p Project) Attempts(ctx context.Context, c *cinderline.Client, cl client.Client, j Job) (Outcome, error) {
	log := p.Log.With("project", p.Config.Key, "issue", j.Ref)
	t := j.Turn
	next := t
	var (
		err error
		res client.Result
	)
	for attempt := 1; attempt <= 2; {
		if berr := p.checkBudget(ctx, c, j.Workspace, j.Ref, t.Dir, t.Author); berr != nil {
			if !errors.Is(berr, errHalted) {
				return Outcome{Result: res}, berr
			}
			if err := p.SavePaused(j.Ref, Paused{Dir: t.Dir, Session: next.SessionID, Client: j.Client}); err != nil {
				log.Warn("paused note not written", "err", err)
			}
			return Outcome{Paused: true}, nil
		}
		log.Info("run started", "attempt", attempt, "session", next.SessionID)
		res, err = p.turn(ctx, c, cl, j.Ref, next)
		p.run.addUsage(j.Ref, j.Ref, j.Agent, t.Model, res.Usage)
		if ctx.Err() != nil {
			// A stop is no failure: the next start's recovery puts the issue back.
			return Outcome{Result: res}, ctx.Err()
		}
		if stop := (*stopError)(nil); errors.As(err, &stop) {
			_ = p.halt(ctx, c, j.Workspace, j.Ref, t, res, stop)
			if err := p.SavePaused(j.Ref, Paused{Dir: t.Dir, Session: res.SessionID, Client: j.Client}); err != nil {
				log.Warn("paused note not written", "err", err)
			}
			return Outcome{Result: res, Paused: true}, nil
		}

		// The status, not the exit code, says it asked: a quick answer can
		// have moved it back to To do already (0116).
		issue, ierr := c.GetIssue(ctx, j.Ref)
		if ierr != nil && err != nil {
			return Outcome{Result: res}, ierr
		}
		if ierr != nil {
			// Finished work goes on while Cinderline does not answer; the
			// outbox carries the report and the move (CSR-159).
			log.Warn("status not checked after the run, going on", "err", ierr)
		}
		if s := issue.Status.Value; s == "blocked" || s == "todo" {
			if err := p.SavePaused(j.Ref, Paused{Dir: t.Dir, Session: res.SessionID, Client: j.Client}); err != nil {
				log.Warn("paused note not written", "err", err)
			}
			log.Info("paused for a person's answer", "status", s, "session", res.SessionID)
			return Outcome{Result: res, Paused: true}, nil
		}
		if err == nil {
			log.Info("run finished", "attempt", attempt, "session", res.SessionID)
			return Outcome{Result: res}, nil
		}
		log.Warn("run failed", "attempt", attempt, "turn_limit", res.HitTurnLimit, "err", err)

		if attempt++; attempt > 2 {
			break
		}
		if res.HitTurnLimit && res.SessionID != "" {
			next = t
			next.Prompt, next.SessionID = ContinuePrompt(j.Ref), res.SessionID
			log.Info("continuing after the turn limit", "session", res.SessionID)
		} else {
			next = t
			log.Info("starting over")
		}
		p.progress(ctx, c, j.Ref, retryProgress(res, attempt))
	}

	return Outcome{Result: res}, bothFailed(res, err)
}

// bothFailed is the error of a second failed run, with the end of its output
// (0095).
func bothFailed(res client.Result, err error) error {
	return fmt.Errorf("both runs failed (last: %w). Last lines of the output:\n\n```\n%s\n```", err, res.Tail)
}
