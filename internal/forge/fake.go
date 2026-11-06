package forge

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrNotScripted is returned when a method's queue is empty.
var ErrNotScripted = errors.New("fake forge: no scripted reply")

var _ Forge = (*Fake)(nil)

// Call is one recorded method call.
type Call struct {
	Method string
	Args   []any
}

// DefaultBranchReply is one scripted reply to DefaultBranch.
type DefaultBranchReply struct {
	Branch string
	Err    error
}

// PullRequestReply is one scripted reply to OpenPullRequest.
type PullRequestReply struct {
	PullRequest PullRequest
	Err         error
}

// FindReply is one scripted reply to FindPullRequest.
type FindReply struct {
	PullRequest PullRequest
	Found       bool
	Err         error
}

// MergeStateReply is one scripted reply to MergeState.
type MergeStateReply struct {
	State MergeState
	Err   error
}

// Fake is a scripted Forge. Fill the queues before use; each call pops the
// next reply in order and is recorded. An empty queue returns the zero value
// and ErrNotScripted.
type Fake struct {
	DefaultBranches []DefaultBranchReply
	PullRequests    []PullRequestReply
	Finds           []FindReply
	Merges          []error // replies to RequestMerge
	MergeStates     []MergeStateReply

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

func (f *Fake) DefaultBranch(_ context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DefaultBranch")
	if len(f.DefaultBranches) == 0 {
		return "", fmt.Errorf("%w for DefaultBranch", ErrNotScripted)
	}
	r := f.DefaultBranches[0]
	f.DefaultBranches = f.DefaultBranches[1:]
	return r.Branch, r.Err
}

func (f *Fake) OpenPullRequest(_ context.Context, in PullRequestInput) (PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("OpenPullRequest", in)
	if len(f.PullRequests) == 0 {
		return PullRequest{}, fmt.Errorf("%w for OpenPullRequest", ErrNotScripted)
	}
	r := f.PullRequests[0]
	f.PullRequests = f.PullRequests[1:]
	return r.PullRequest, r.Err
}

func (f *Fake) FindPullRequest(_ context.Context, branch string) (PullRequest, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("FindPullRequest", branch)
	if len(f.Finds) == 0 {
		return PullRequest{}, false, fmt.Errorf("%w for FindPullRequest", ErrNotScripted)
	}
	r := f.Finds[0]
	f.Finds = f.Finds[1:]
	return r.PullRequest, r.Found, r.Err
}

func (f *Fake) RequestMerge(_ context.Context, pr PullRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("RequestMerge", pr)
	if len(f.Merges) == 0 {
		return fmt.Errorf("%w for RequestMerge", ErrNotScripted)
	}
	err := f.Merges[0]
	f.Merges = f.Merges[1:]
	return err
}

func (f *Fake) MergeState(_ context.Context, pr PullRequest) (MergeState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("MergeState", pr)
	if len(f.MergeStates) == 0 {
		return "", fmt.Errorf("%w for MergeState", ErrNotScripted)
	}
	r := f.MergeStates[0]
	f.MergeStates = f.MergeStates[1:]
	return r.State, r.Err
}
