// Package environment defines Environment, the container a run's agent works
// in, and Fake, its in-memory stand-in for tests.
package environment

import "context"

// Environment starts and stops the containers of runs.
type Environment interface {
	// Start starts the container for a run and returns its ID.
	Start(ctx context.Context, s Spec) (string, error)
	Stop(ctx context.Context, id string) error
	// Tail returns the end of the container's output (the last 4,000
	// characters in the real implementation).
	Tail(ctx context.Context, id string) (string, error)
}

// Spec is what a run's container is started with.
type Spec struct {
	Project string
	Issue   string
	Image   string
	Dir     string   // the issue's clone, mounted as the working directory
	Env     []string // KEY=value
}
