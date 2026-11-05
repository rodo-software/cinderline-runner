package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrNotScripted is returned when a method's queue is empty.
var ErrNotScripted = errors.New("fake client: no scripted reply")

var _ Client = (*Fake)(nil)

// Call is one recorded method call.
type Call struct {
	Method string
	Args   []any
}

// RunReply is one scripted reply to Run.
type RunReply struct {
	Result Result
	Err    error
}

// Fake is a scripted Client. Fill the queues before use; each call pops the
// next reply in order and is recorded. An empty queue returns the zero value
// and ErrNotScripted.
type Fake struct {
	Runs []RunReply

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

func (f *Fake) Run(_ context.Context, t Turn) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Run", t)
	if len(f.Runs) == 0 {
		return Result{}, fmt.Errorf("%w for Run", ErrNotScripted)
	}
	r := f.Runs[0]
	f.Runs = f.Runs[1:]
	return r.Result, r.Err
}
