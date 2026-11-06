// Package client defines Client, the agent CLI (Claude Code or OpenCode) that
// runs one agent turn, and Fake, its in-memory stand-in for tests. It is not
// the REST client of the issue tracker, which is its own package.
package client

import "context"

// Client runs agent turns.
type Client interface {
	// Run runs one turn. On failure it returns the parsed result it has, such
	// as the output tail, together with the error.
	Run(ctx context.Context, t Turn) (Result, error)
}

// Turn is one agent turn. An empty SessionID starts a new session; otherwise
// that session is resumed.
type Turn struct {
	Dir       string
	Prompt    string
	SessionID string
	MaxTurns  int
	// MaxMinutes is the team's time limit for the turn, at which the
	// Environment stops it; 0: none.
	MaxMinutes int
	Model      string // the agent's model; empty: the client's default
	// Token is the run's scoped CinderStack token for the MCP server; empty
	// gives the agent none, as workers and the Merge Resolver (0122). Never log it.
	Token string
}

// Result is the parsed outcome of a turn.
type Result struct {
	SessionID    string
	Report       string // the agent's final text
	HitTurnLimit bool
	Tail         string // the end of the output, for comments
}
