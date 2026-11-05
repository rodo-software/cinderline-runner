package workspace

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrNotScripted is returned when a method's queue is empty.
var ErrNotScripted = errors.New("fake workspace: no scripted reply")

var _ Workspace = (*Fake)(nil)

// Call is one recorded method call.
type Call struct {
	Method string
	Args   []any
}

// PrepareReply is one scripted reply to Prepare.
type PrepareReply struct {
	Dir string
	Err error
}

// CommitReply is one scripted reply to Commit.
type CommitReply struct {
	Committed bool
	Err       error
}

// MergeBaseReply is one scripted reply to MergeBase.
type MergeBaseReply struct {
	Conflicts []string
	Err       error
}

// IsAncestorReply is one scripted reply to IsAncestor.
type IsAncestorReply struct {
	Yes bool
	Err error
}

// Fake is a scripted Workspace. Fill the queues before use; each call pops
// the next reply in order and is recorded. An empty queue returns the zero
// value and ErrNotScripted.
type Fake struct {
	Prepares   []PrepareReply
	Commits    []CommitReply
	MergeBases []MergeBaseReply
	Ancestors  []IsAncestorReply
	Pushes     []error // replies to Push

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

func (f *Fake) Prepare(_ context.Context, branch, base string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Prepare", branch, base)
	if len(f.Prepares) == 0 {
		return "", fmt.Errorf("%w for Prepare", ErrNotScripted)
	}
	r := f.Prepares[0]
	f.Prepares = f.Prepares[1:]
	return r.Dir, r.Err
}

func (f *Fake) Commit(_ context.Context, dir, message string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Commit", dir, message)
	if len(f.Commits) == 0 {
		return false, fmt.Errorf("%w for Commit", ErrNotScripted)
	}
	r := f.Commits[0]
	f.Commits = f.Commits[1:]
	return r.Committed, r.Err
}

func (f *Fake) MergeBase(_ context.Context, dir, base string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("MergeBase", dir, base)
	if len(f.MergeBases) == 0 {
		return nil, fmt.Errorf("%w for MergeBase", ErrNotScripted)
	}
	r := f.MergeBases[0]
	f.MergeBases = f.MergeBases[1:]
	return r.Conflicts, r.Err
}

func (f *Fake) IsAncestor(_ context.Context, dir, ancestor, descendant string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("IsAncestor", dir, ancestor, descendant)
	if len(f.Ancestors) == 0 {
		return false, fmt.Errorf("%w for IsAncestor", ErrNotScripted)
	}
	r := f.Ancestors[0]
	f.Ancestors = f.Ancestors[1:]
	return r.Yes, r.Err
}

func (f *Fake) Push(_ context.Context, dir, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Push", dir, branch)
	if len(f.Pushes) == 0 {
		return fmt.Errorf("%w for Push", ErrNotScripted)
	}
	err := f.Pushes[0]
	f.Pushes = f.Pushes[1:]
	return err
}
