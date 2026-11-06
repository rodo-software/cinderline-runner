// Package workspace defines Workspace, the git clones runs work in (prepare,
// commit, base merges, the resolver's ancestry check, push), and Fake, its
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
	// MergeBase merges base into the clone's branch and returns the
	// conflicting files; none means the merge succeeded.
	MergeBase(ctx context.Context, dir, base string) ([]string, error)
	// IsAncestor reports whether commit ancestor is an ancestor of descendant.
	IsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error)
	Push(ctx context.Context, dir, branch string) error
	// Head returns the commit the clone's branch is on.
	Head(ctx context.Context, dir string) (string, error)
}
