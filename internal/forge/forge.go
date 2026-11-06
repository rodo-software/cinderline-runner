// Package forge defines Forge, the git host of a project's repository
// (pull requests and merges), and Fake, its in-memory stand-in for tests.
package forge

import "context"

// Forge talks to the host of one repository.
type Forge interface {
	DefaultBranch(ctx context.Context) (string, error)
	// OpenPullRequest opens a pull request for in.Branch, or returns the open
	// one that already exists for it.
	OpenPullRequest(ctx context.Context, in PullRequestInput) (PullRequest, error)
	// FindPullRequest returns the open pull request of branch; found is
	// false when none is open, so a pull request a person closed is not
	// opened again.
	FindPullRequest(ctx context.Context, branch string) (pr PullRequest, found bool, err error)
	RequestMerge(ctx context.Context, pr PullRequest) error
	MergeState(ctx context.Context, pr PullRequest) (MergeState, error)
}

// PullRequestInput is what a new pull request is opened with.
type PullRequestInput struct {
	Branch string
	Base   string
	Title  string
	Body   string
}

// PullRequest identifies a pull request on the forge.
type PullRequest struct {
	Number int
	URL    string
}

// MergeState is where a requested merge stands.
type MergeState string

const (
	MergePending MergeState = "pending"
	MergeMerged  MergeState = "merged"
	MergeFailed  MergeState = "failed" // failed checks or a conflict
)
