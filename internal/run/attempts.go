package run

import (
	"context"
	"fmt"

	"github.com/rodo-software/cinderstack-runner/internal/client"
	"github.com/rodo-software/cinderstack-runner/internal/environment"
)

// ContinuePrompt is what a run that hit the team's turn limit gets when it
// continues, the text of legacy lib.mjs continuePrompt.
func ContinuePrompt(ref string) string {
	return "You ran out of turns on " + ref + " and have a fresh budget. Finish the task under the same rules, starting from the work already in this worktree."
}

// Attempts runs the agent's turn t for issue ref in container, at most twice
// (plan M3, 0095). A run that hit the turn limit continues its own session
// with a fresh budget, so its context is kept; any other failure starts over
// in a fresh session. After a second failure the error carries the output
// tail the Environment kept, and the scheduler posts it as a comment before
// moving the issue to Review, without publishing.
func (p Project) Attempts(ctx context.Context, cl client.Client, env environment.Environment, container, ref string, t client.Turn) (client.Result, error) {
	log := p.Log.With("project", p.Config.Key, "issue", ref)
	next := t
	var (
		res client.Result
		err error
	)
	for attempt := 1; attempt <= 2; attempt++ {
		log.Info("run started", "attempt", attempt, "session", next.SessionID)
		res, err = cl.Run(ctx, next)
		if err == nil {
			log.Info("run finished", "attempt", attempt, "session", res.SessionID)
			return res, nil
		}
		if ctx.Err() != nil {
			// A stop is no failure: the next start's recovery puts the issue back.
			return res, ctx.Err()
		}
		log.Warn("run failed", "attempt", attempt, "turn_limit", res.HitTurnLimit, "err", err)
		if attempt == 2 {
			break
		}
		if res.HitTurnLimit && res.SessionID != "" {
			next = client.Turn{Dir: t.Dir, Prompt: ContinuePrompt(ref), SessionID: res.SessionID, MaxTurns: t.MaxTurns}
			log.Info("continuing after the turn limit", "session", res.SessionID)
		} else {
			next = t
			log.Info("starting over")
		}
	}

	tail, terr := env.Tail(ctx, container)
	if terr != nil {
		log.Warn("output tail not read", "err", terr)
		tail = "(not available: " + terr.Error() + ")"
	}
	return res, fmt.Errorf("both runs failed (last: %w). Last lines of the output:\n\n```\n%s\n```", err, tail)
}
