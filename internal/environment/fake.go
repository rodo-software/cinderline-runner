package environment

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrNotScripted is returned when a method's queue is empty.
var ErrNotScripted = errors.New("fake environment: no scripted reply")

var _ Environment = (*Fake)(nil)

// Call is one recorded method call.
type Call struct {
	Method string
	Args   []any
}

// RunReply is one scripted reply to Run.
type RunReply struct {
	Output string
	Err    error
}

// Fake is a scripted Environment. Fill the queues before use; each call pops
// the next reply in order and is recorded. An empty queue returns the zero
// value and ErrNotScripted.
type Fake struct {
	Runs     []RunReply
	Cleanups []error // replies to Cleanup

	mu    sync.Mutex
	calls []Call
}

// Calls returns the calls made so far, in order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

func (f *Fake) Run(_ context.Context, s Spec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{"Run", []any{s}})
	if len(f.Runs) == 0 {
		return "", fmt.Errorf("%w for Run", ErrNotScripted)
	}
	r := f.Runs[0]
	f.Runs = f.Runs[1:]
	return r.Output, r.Err
}

func (f *Fake) Cleanup(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{"Cleanup", nil})
	if len(f.Cleanups) == 0 {
		return fmt.Errorf("%w for Cleanup", ErrNotScripted)
	}
	err := f.Cleanups[0]
	f.Cleanups = f.Cleanups[1:]
	return err
}
