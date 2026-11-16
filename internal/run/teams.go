package run

import (
	"context"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
)

// gitAuthor is agent's git identity: the name Cinderline gives its token,
// "<team> / <agent>" cut to 100 characters (app decision 0045, CSR-169).
func gitAuthor(team cinderline.Team, agent string) string {
	r := []rune(team.Name + " / " + agent)
	return string(r[:min(len(r), 100)])
}

// leadAuthor is the run's lead agent's (the solo agent or the orchestrator)
// git identity, for the commits the runner makes itself; a team without
// agents leaves the runner's own.
func leadAuthor(team cinderline.Team) string {
	if len(team.Agents) == 0 {
		return ""
	}
	return gitAuthor(team, team.Agents[0].Name)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// progress reports a step on the issue a person watches (decision
// 2026-11-04-run-progress-updates). A failure only logs: progress never stops
// a run. Cut to Cinderline's 200 characters, as a sub-issue title alone may
// be that long.
func (p Project) progress(ctx context.Context, c *cinderline.Client, ref, msg string) {
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200])
	}
	if err := c.AddProgress(ctx, ref, msg); err != nil {
		p.Log.Warn("progress not reported", "project", p.Config.Key, "issue", ref, "err", err)
	}
}
