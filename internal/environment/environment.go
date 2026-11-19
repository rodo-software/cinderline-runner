// Package environment defines Environment, where an agent turn runs, and
// Fake, its in-memory stand-in for tests.
package environment

import (
	"context"
	"errors"
	"time"
)

// Environment runs agent turns, each in a fresh container with its own
// private services, so nothing one turn started outlives it (Willem's
// decision on CSR-31: a process left behind could read a later turn's
// token).
type Environment interface {
	// Run runs s.Cmd and returns its whole output, standard output and error
	// interleaved. A command that ran and failed returns an error with an
	// ExitCode method; one stopped at s.Timeout returns ErrTimeout.
	Run(ctx context.Context, s Spec) (string, error)
	// Cleanup removes what an earlier process left, at start, while no run
	// is in progress.
	Cleanup(ctx context.Context) error
}

// ErrTimeout is a run stopped at its Timeout, the team's max_minutes.
var ErrTimeout = errors.New("stopped at the time limit")

// ErrIdle is a run stopped because it printed nothing for its Idle limit:
// stalled, as an agent that waits inside its session (CSR-159).
var ErrIdle = errors.New("stopped as idle")

// Spec is one agent turn.
type Spec struct {
	Issue  string   // the issue's ref, in names and labels
	Dir    string   // the working directory: the issue's clone
	Mounts []string // docker run flags that mount the clone, the tools and the state
	// Env is all the container gets besides the proxy settings: the
	// turn's model credential, its scoped token and the client's settings,
	// as KEY=value.
	Env     []string
	Cmd     []string
	Stdin   string
	Timeout time.Duration // 0: none
	// Idle stops the run once its output was silent this long; 0: never.
	Idle time.Duration
	// Allow is the client's provider hosts, as host:port, for the run's
	// network allowlist next to the project's (CSR-63).
	Allow []string
	// AppAllow holds app-approved host:port pairs. Unlike local config and
	// provider hosts, these may connect only to public addresses.
	AppAllow []string
}

// Tail is the end of a turn's output kept for comments: the last 40 lines,
// at most 4,000 characters, as today.
func Tail(output string) string { return tail(output, 40, 4000) }
