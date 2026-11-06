// Package workspace defines Workspace, the git clones runs work in (prepare,
// commit, base merges, the Merge Resolver's checks, push), and Fake, its
// in-memory stand-in for tests.
package workspace

import "context"

// Workspace manages the per-issue clones of one project's repository. A clone
// is identified by the dir Prepare returns.
type Workspace interface {
	// Prepare makes or reuses the clone for branch, starting from base when
	// the branch is new, and returns its dir.
	Prepare(ctx context.Context, branch, base string) (string, error)
	// Commit commits everything left in dir and reports whether there was
	// anything to commit.
	Commit(ctx context.Context, dir, message string) (bool, error)
	// MergeBase fetches and merges base into the clone's branch and returns
	// the conflicting files; none means the merge succeeded. A conflicted
	// merge is aborted, so the clone is as it was.
	MergeBase(ctx context.Context, dir, base string) ([]string, error)
	// IsAncestor reports whether commit ancestor is an ancestor of
	// descendant. A branch name other than the clone's own means the
	// remote's branch as last fetched.
	IsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error)
	// Clean reports whether the clone has nothing uncommitted or untracked
	// and no merge in progress.
	Clean(ctx context.Context, dir string) (bool, error)
	// ConflictMarkers reports whether a line of one of files starts with a
	// conflict marker ("<<<<<<< " or ">>>>>>> ").
	ConflictMarkers(ctx context.Context, dir string, files []string) (bool, error)
	// Reset aborts a merge in progress, moves the branch back to commit and
	// removes untracked files.
	Reset(ctx context.Context, dir, commit string) error
	// Log returns the one-line log of the commits on the clone's branch that
	// are not on base.
	Log(ctx context.Context, dir, base string) (string, error)
	Push(ctx context.Context, dir, branch string) error
	// Head returns the commit the clone's branch is on.
	Head(ctx context.Context, dir string) (string, error)
}
