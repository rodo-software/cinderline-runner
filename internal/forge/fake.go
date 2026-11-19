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
	State   MergeState
	Head    string
	Running bool
	Failed  []Check
	Err     error
}

// CheckLogReply is one scripted reply to CheckLog.
type CheckLogReply struct {
	Log string
	Err error
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
	CheckLogs       []CheckLogReply

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

func (f *Fake) MergeState(_ context.Context, branch string) (PullState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("MergeState", branch)
	if len(f.MergeStates) == 0 {
		return PullState{}, fmt.Errorf("%w for MergeState", ErrNotScripted)
	}
	r := f.MergeStates[0]
	f.MergeStates = f.MergeStates[1:]
	return PullState{State: r.State, Head: r.Head, Running: r.Running, Failed: r.Failed}, r.Err
}

func (f *Fake) CheckLog(_ context.Context, c Check) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CheckLog", c)
	if len(f.CheckLogs) == 0 {
		return "", fmt.Errorf("%w for CheckLog", ErrNotScripted)
	}
	r := f.CheckLogs[0]
	f.CheckLogs = f.CheckLogs[1:]
	return r.Log, r.Err
}
