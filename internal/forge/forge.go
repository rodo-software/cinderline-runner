// Package forge defines Forge, the git host of a project's repository
// (pull requests and merges), its implementations GitHub (GitHub and
// GitHub Enterprise), GitLab (gitlab.com and self-managed) and Forgejo
// (Forgejo and Gitea), Bitbucket (Bitbucket Cloud), and Fake, its in-memory
// stand-in for tests.
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
	// MergeState is where the newest pull request of branch stands, in any
	// state; "" when the branch has none. By branch, as legacy
	// pullRequestOutcome: the runner keeps no pull request numbers (0091).
	MergeState(ctx context.Context, branch string) (MergeState, error)
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

// MergeState is where a pull request stands.
type MergeState string

const (
	MergePending MergeState = "pending" // open, without failed checks
	MergeMerged  MergeState = "merged"
	MergeClosed  MergeState = "closed" // closed without merging
	MergeFailed  MergeState = "failed" // open, with failed checks on its head
)
