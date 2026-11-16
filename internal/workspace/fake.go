package workspace

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
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
	Clones       []string // dirs List returns and Exists reports, besides those on disk

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

// pop records the call and takes the next reply from q; an empty queue
// gives the zero reply and ErrNotScripted.
func pop[T any](f *Fake, q *[]T, method string, args ...any) (T, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(method, args...)
	var r T
	if len(*q) == 0 {
		return r, fmt.Errorf("%w for %s", ErrNotScripted, method)
	}
	r, *q = (*q)[0], (*q)[1:]
	return r, nil
}

func (f *Fake) Prepare(_ context.Context, branch, base string) (string, error) {
	r, err := pop(f, &f.Prepares, "Prepare", branch, base)
	return r.Dir, cmp.Or(err, r.Err)
}

func (f *Fake) Commit(_ context.Context, dir, author, message string) (bool, error) {
	r, err := pop(f, &f.Commits, "Commit", dir, author, message)
	return r.Committed, cmp.Or(err, r.Err)
}

func (f *Fake) MergeBase(_ context.Context, dir, base, author string) ([]string, error) {
	r, err := pop(f, &f.MergeBases, "MergeBase", dir, base, author)
	return r.Conflicts, cmp.Or(err, r.Err)
}

func (f *Fake) IsAncestor(_ context.Context, dir, ancestor, descendant string) (bool, error) {
	r, err := pop(f, &f.Ancestors, "IsAncestor", dir, ancestor, descendant)
	return r.Yes, cmp.Or(err, r.Err)
}

func (f *Fake) Push(_ context.Context, dir, branch string) error {
	r, err := pop(f, &f.Pushes, "Push", dir, branch)
	return cmp.Or(err, r)
}

func (f *Fake) Head(_ context.Context, dir string) (string, error) {
	r, err := pop(f, &f.Heads, "Head", dir)
	return r.Commit, cmp.Or(err, r.Err)
}

func (f *Fake) Clean(_ context.Context, dir string) (bool, error) {
	r, err := pop(f, &f.Cleans, "Clean", dir)
	return r.Yes, cmp.Or(err, r.Err)
}

func (f *Fake) ConflictMarkers(_ context.Context, dir string, files []string) (bool, error) {
	r, err := pop(f, &f.Markers, "ConflictMarkers", dir, files)
	return r.Yes, cmp.Or(err, r.Err)
}

func (f *Fake) Reset(_ context.Context, dir, commit string) error {
	r, err := pop(f, &f.Resets, "Reset", dir, commit)
	return cmp.Or(err, r)
}

func (f *Fake) Log(_ context.Context, dir, base string) (string, error) {
	r, err := pop(f, &f.Logs, "Log", dir, base)
	return r.Log, cmp.Or(err, r.Err)
}

func (f *Fake) Stranded(_ context.Context) ([]Clone, error) {
	r, err := pop(f, &f.Strandeds, "Stranded")
	return r.Clones, cmp.Or(err, r.Err)
}

func (f *Fake) Behind(_ context.Context, base string) ([]string, error) {
	r, err := pop(f, &f.Behinds, "Behind", base)
	return r.Branches, cmp.Or(err, r.Err)
}

// FreeReply is one scripted reply to Free.
type FreeReply struct {
	Bytes int64
	Err   error
}

// List returns Clones.
func (f *Fake) List(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("List")
	return slices.Clone(f.Clones), nil
}

// Exists reports whether dir exists on the host: tests make their clones
// with t.TempDir.
func (f *Fake) Exists(_ context.Context, dir string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Exists", dir)
	_, err := os.Stat(dir)
	return err == nil || slices.Contains(f.Clones, dir), nil
}

func (f *Fake) FastForward(_ context.Context, dir, branch string) error {
	r, err := pop(f, &f.FastForwards, "FastForward", dir, branch)
	return cmp.Or(err, r)
}

func (f *Fake) Remove(_ context.Context, dir string) error {
	r, err := pop(f, &f.Removes, "Remove", dir)
	return cmp.Or(err, r)
}

func (f *Fake) Free(_ context.Context) (int64, error) {
	r, err := pop(f, &f.Frees, "Free")
	return r.Bytes, cmp.Or(err, r.Err)
}
