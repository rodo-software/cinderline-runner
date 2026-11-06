package run

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/rodo-software/cinderstack-runner/internal/cinderstack"
	"github.com/rodo-software/cinderstack-runner/internal/client"
	"github.com/rodo-software/cinderstack-runner/internal/environment"
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

// UnreadComments is added to the final comment when the resume to read a
// person's comments failed twice and the finished work is published anyway.
const UnreadComments = "\n\nA person commented during the run, but resuming the agent to read those comments failed twice, so this result doesn't take them into account. Read them before you go on."

// retryProgress is the progress line of the run after a failed one, as
// legacy runner.mjs retryProgress.
func retryProgress(res client.Result, attempt int) string {
	if res.HitTurnLimit {
		return "Continuing after the turn limit"
	}
	return fmt.Sprintf("Retrying (attempt %d)", attempt)
}

// Job is one issue's agent run for Attempts.
type Job struct {
	Ref       string
	Client    string // the agent's CLI, kept in a paused note
	Container string
	Turn      client.Turn // the fresh first turn
	Resume    string      // a paused session to resume first with ResumePrompt, from TakePaused
	// NoComments leaves the comments to the caller: a team's rounds read
	// them only after the last round, as legacy rounds() does.
	NoComments bool
}

// Outcome is how Attempts ended without an error.
type Outcome struct {
	client.Result
	Paused bool // the issue waits for a person; the note is written and the clone stays
	Unread bool // reading a person's comments failed twice: publish with UnreadComments
}

// newestCommentID is the comment cursor: IDs, not timestamps, so the
// runner's clock doesn't matter (decision 2026-11-03-comments-during-a-run).
func newestCommentID(cs []cinderstack.Comment) int {
	id := 0
	for _, c := range cs {
		id = max(id, c.ID)
	}
	return id
}

// personCommentedAfter reports a person's comment after the cursor; an
// agent's comments don't count.
func personCommentedAfter(cs []cinderstack.Comment, cursor int) bool {
	for _, c := range cs {
		if c.Author.Agent == nil && c.ID > cursor {
			return true
		}
	}
	return false
}

// Attempts runs the agent for job.Ref, at most twice (plan M3, 0095). A run
// that hit the turn limit continues its own session with a fresh budget, so
// its context is kept; any other failure starts over in a fresh session.
// After a second failure the error carries the output tail the Environment
// kept, and the scheduler posts it as a comment before moving the issue to
// Review, without publishing.
//
// After every run, an issue that waits for a person (Blocked, or To do after
// a quick answer) pauses the run: it writes the note and returns Paused
// (0116). A run that ends while a person has commented since the cursor
// resumes its session to read them; that uses no attempt. A failed resume is
// retried once in the same session, then the finished work is published with
// UnreadComments (decision 2026-11-03-comments-during-a-run).
func (p Project) Attempts(ctx context.Context, c *cinderstack.Client, cl client.Client, env environment.Environment, j Job) (Outcome, error) {
	log := p.Log.With("project", p.Config.Key, "issue", j.Ref)
	t := j.Turn
	resume := func(session, prompt string) client.Turn {
		r := t
		r.Prompt, r.SessionID = prompt, session
		return r
	}
	next := t
	if j.Resume != "" {
		next = resume(j.Resume, ResumePrompt(j.Ref))
	}

	// An answer to ask_person is in already, so it is never delivered twice.
	var cursor int
	if !j.NoComments {
		cs, err := c.ListComments(ctx, j.Ref)
		if err != nil {
			return Outcome{}, err
		}
		cursor = newestCommentID(cs)
	}
	var (
		err      error
		res      client.Result
		done     *client.Result // the finished work while its session reads comments
		readFail int
	)
	for attempt := 1; attempt <= 2; {
		log.Info("run started", "attempt", attempt, "session", next.SessionID)
		res, err = cl.Run(ctx, next)
		if ctx.Err() != nil {
			// A stop is no failure: the next start's recovery puts the issue back.
			return Outcome{Result: res}, ctx.Err()
		}

		// The status, not the exit code, says it asked: a quick answer can
		// have moved it back to To do already (0116).
		issue, ierr := c.GetIssue(ctx, j.Ref)
		if ierr != nil {
			return Outcome{Result: res}, ierr
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
			if j.NoComments {
				return Outcome{Result: res}, nil
			}
			cs, err := c.ListComments(ctx, j.Ref)
			if err != nil {
				return Outcome{Result: res}, err
			}
			if !personCommentedAfter(cs, cursor) {
				return Outcome{Result: res}, nil
			}
			finished := res
			cursor, readFail, done = newestCommentID(cs), 0, &finished
			next = resume(res.SessionID, CommentsPrompt(j.Ref))
			log.Info("resuming to read a person's comments", "session", res.SessionID)
			p.progress(ctx, c, j.Ref, "Reading new comments from a person")
			continue
		}
		log.Warn("run failed", "attempt", attempt, "turn_limit", res.HitTurnLimit, "err", err)

		// The work before the comments was finished: never start it over.
		if done != nil {
			if readFail++; readFail == 2 {
				log.Warn("comments not read twice, publishing without them")
				return Outcome{Result: *done, Unread: true}, nil
			}
			prompt := CommentsPrompt(j.Ref)
			if res.HitTurnLimit {
				prompt = ContinuePrompt(j.Ref)
			}
			next = resume(next.SessionID, prompt)
			p.progress(ctx, c, j.Ref, retryProgress(res, readFail+1))
			continue
		}

		if attempt++; attempt > 2 {
			break
		}
		if res.HitTurnLimit && res.SessionID != "" {
			next = resume(res.SessionID, ContinuePrompt(j.Ref))
			log.Info("continuing after the turn limit", "session", res.SessionID)
		} else {
			next = t
			log.Info("starting over")
		}
		p.progress(ctx, c, j.Ref, retryProgress(res, attempt))
	}

	return Outcome{Result: res}, bothFailed(ctx, log, env, j.Container, err)
}

// bothFailed is the error of a second failed run, with the output tail the
// Environment kept (0095).
func bothFailed(ctx context.Context, log *slog.Logger, env environment.Environment, container string, err error) error {
	tail, terr := env.Tail(ctx, container)
	if terr != nil {
		log.Warn("output tail not read", "err", terr)
		tail = "(not available: " + terr.Error() + ")"
	}
	return fmt.Errorf("both runs failed (last: %w). Last lines of the output:\n\n```\n%s\n```", err, tail)
}
