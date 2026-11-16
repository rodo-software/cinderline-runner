package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// HeadReply is one scripted reply to Head.
type HeadReply struct {
	Commit string
	Err    error
}

// StrandedReply is one scripted reply to Stranded.
type StrandedReply struct {
	Clones []Clone
	Err    error
}

// BehindReply is one scripted reply to Behind.
type BehindReply struct {
	Branches []string
	Err      error
}

// BoolReply is one scripted reply to Clean or ConflictMarkers.
type BoolReply struct {
	Yes bool
	Err error
}

// LogReply is one scripted reply to Log.
type LogReply struct {
	Log string
	Err error
}

// Fake is a scripted Workspace. Fill the queues before use; each call pops
// the next reply in order and is recorded. An empty queue returns the zero
// value and ErrNotScripted.
type Fake struct {
	Prepares     []PrepareReply
	Commits      []CommitReply
	MergeBases   []MergeBaseReply
	Ancestors    []IsAncestorReply
	Pushes       []error // replies to Push
	Heads        []HeadReply
	Cleans       []BoolReply
	Markers      []BoolReply // replies to ConflictMarkers
	Resets       []error
	Logs         []LogReply
	Strandeds    []StrandedReply
	Behinds      []BehindReply
	FastForwards []error
	Removes      []error
	Frees        []FreeReply

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

func (f *Fake) Commit(_ context.Context, dir, author, message string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Commit", dir, author, message)
	if len(f.Commits) == 0 {
		return false, fmt.Errorf("%w for Commit", ErrNotScripted)
	}
	r := f.Commits[0]
	f.Commits = f.Commits[1:]
	return r.Committed, r.Err
}

func (f *Fake) MergeBase(_ context.Context, dir, base, author string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("MergeBase", dir, base, author)
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

func (f *Fake) Head(_ context.Context, dir string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Head", dir)
	if len(f.Heads) == 0 {
		return "", fmt.Errorf("%w for Head", ErrNotScripted)
	}
	r := f.Heads[0]
	f.Heads = f.Heads[1:]
	return r.Commit, r.Err
}

func (f *Fake) Clean(_ context.Context, dir string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Clean", dir)
	if len(f.Cleans) == 0 {
		return false, fmt.Errorf("%w for Clean", ErrNotScripted)
	}
	r := f.Cleans[0]
	f.Cleans = f.Cleans[1:]
	return r.Yes, r.Err
}

func (f *Fake) ConflictMarkers(_ context.Context, dir string, files []string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ConflictMarkers", dir, files)
	if len(f.Markers) == 0 {
		return false, fmt.Errorf("%w for ConflictMarkers", ErrNotScripted)
	}
	r := f.Markers[0]
	f.Markers = f.Markers[1:]
	return r.Yes, r.Err
}

func (f *Fake) Reset(_ context.Context, dir, commit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Reset", dir, commit)
	if len(f.Resets) == 0 {
		return fmt.Errorf("%w for Reset", ErrNotScripted)
	}
	err := f.Resets[0]
	f.Resets = f.Resets[1:]
	return err
}

func (f *Fake) Log(_ context.Context, dir, base string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Log", dir, base)
	if len(f.Logs) == 0 {
		return "", fmt.Errorf("%w for Log", ErrNotScripted)
	}
	r := f.Logs[0]
	f.Logs = f.Logs[1:]
	return r.Log, r.Err
}

func (f *Fake) Stranded(_ context.Context) ([]Clone, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Stranded")
	if len(f.Strandeds) == 0 {
		return nil, fmt.Errorf("%w for Stranded", ErrNotScripted)
	}
	r := f.Strandeds[0]
	f.Strandeds = f.Strandeds[1:]
	return r.Clones, r.Err
}

func (f *Fake) Behind(_ context.Context, base string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Behind", base)
	if len(f.Behinds) == 0 {
		return nil, fmt.Errorf("%w for Behind", ErrNotScripted)
	}
	r := f.Behinds[0]
	f.Behinds = f.Behinds[1:]
	return r.Branches, r.Err
}

// FreeReply is one scripted reply to Free.
type FreeReply struct {
	Bytes int64
	Err   error
}

// Exists reports whether dir exists on the host: tests make their clones
// with t.TempDir.
func (f *Fake) Exists(_ context.Context, dir string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Exists", dir)
	_, err := os.Stat(dir)
	return err == nil, nil
}

func (f *Fake) FastForward(_ context.Context, dir, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("FastForward", dir, branch)
	if len(f.FastForwards) == 0 {
		return fmt.Errorf("%w for FastForward", ErrNotScripted)
	}
	r := f.FastForwards[0]
	f.FastForwards = f.FastForwards[1:]
	return r
}

func (f *Fake) Remove(_ context.Context, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Remove", dir)
	if len(f.Removes) == 0 {
		return fmt.Errorf("%w for Remove", ErrNotScripted)
	}
	r := f.Removes[0]
	f.Removes = f.Removes[1:]
	return r
}

func (f *Fake) Free(_ context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Free")
	if len(f.Frees) == 0 {
		return 0, fmt.Errorf("%w for Free", ErrNotScripted)
	}
	r := f.Frees[0]
	f.Frees = f.Frees[1:]
	return r.Bytes, r.Err
}
