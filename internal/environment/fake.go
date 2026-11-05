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

// StartReply is one scripted reply to Start.
type StartReply struct {
	ID  string
	Err error
}

// TailReply is one scripted reply to Tail.
type TailReply struct {
	Tail string
	Err  error
}

// Fake is a scripted Environment. Fill the queues before use; each call pops
// the next reply in order and is recorded. An empty queue returns the zero
// value and ErrNotScripted.
type Fake struct {
	Starts []StartReply
	Stops  []error // replies to Stop
	Tails  []TailReply

	mu    sync.Mutex
	calls []Call
}

// Calls returns the calls made so far, in order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

func (f *Fake) record(method string, args ...any) {
	f.calls = append(f.calls, Call{method, args})
}

func (f *Fake) Start(_ context.Context, s Spec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Start", s)
	if len(f.Starts) == 0 {
		return "", fmt.Errorf("%w for Start", ErrNotScripted)
	}
	r := f.Starts[0]
	f.Starts = f.Starts[1:]
	return r.ID, r.Err
}

func (f *Fake) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Stop", id)
	if len(f.Stops) == 0 {
		return fmt.Errorf("%w for Stop", ErrNotScripted)
	}
	err := f.Stops[0]
	f.Stops = f.Stops[1:]
	return err
}

func (f *Fake) Tail(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Tail", id)
	if len(f.Tails) == 0 {
		return "", fmt.Errorf("%w for Tail", ErrNotScripted)
	}
	r := f.Tails[0]
	f.Tails = f.Tails[1:]
	return r.Tail, r.Err
}
