// Package forge defines Forge, the git host of a project's repository
// (pull requests and merges), its implementations GitHub (GitHub and
// GitHub Enterprise), GitLab (gitlab.com and self-managed) and Forgejo
// (Forgejo and Gitea), Bitbucket (Bitbucket Cloud), and Fake, its in-memory
// stand-in for tests.
package forge

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

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
	// CISecrets checks whether the repository's CI can read secrets from a
	// pushed branch. nil, nil = no CI secret reachable from an unprotected
	// branch; non-empty = the secrets reachable from any branch, each
	// described like "repository secret DEPLOY_KEY"; error = the check could
	// not be made (forge API refused or failed). On any HTTP failure the
	// error is returned at once with no names, except a partial check: then
	// both the names found so far and an error saying what was not checked.
	CISecrets(ctx context.Context) (exposed []string, err error)
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

// New returns the Forge of repository on a forge of kind, as a project names it.
func New(kind, repository, token string) (Forge, error) {
	switch kind {
	case "github":
		return NewGitHub(repository, token)
	case "gitlab":
		return NewGitLab(repository, token)
	case "forgejo":
		return NewForgejo(repository, token)
	case "bitbucket":
		return NewBitbucket(repository, token)
	default:
		return nil, fmt.Errorf("unsupported forge kind %q", kind)
	}
}

// readWaits are the waits between the tries of a forge read; a variable so tests need not wait.
var readWaits = []time.Duration{2 * time.Second, 8 * time.Second}

// send sends req. A GET that got no answer, a 5xx or a 429 is tried again
// after readWaits (CSR-181); any other request is sent once: Publish
// retries the writes itself.
func send(hc *http.Client, req *http.Request) (*http.Response, error) {
	for i := 0; ; i++ {
		resp, err := hc.Do(req)
		retry := err != nil || resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		if !retry || req.Method != http.MethodGet || i == len(readWaits) || req.Context().Err() != nil {
			return resp, err
		}
		if resp != nil {
			resp.Body.Close()
		}
		if err := Sleep(req.Context(), readWaits[i]); err != nil {
			return nil, err
		}
	}
}
