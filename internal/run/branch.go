package run

import (
	"context"
	"regexp"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// conflict is a base merge that conflicted and was aborted: the facts next
// gets as its last step, and what verifyMerge checks a resolution against.
type conflict struct {
	head  string   // the branch's head before the merge
	files []string // untrusted, from the repository
	last  cinderline.LastStep
}

var blocked = regexp.MustCompile(`(?i)^[\s*_>#-]*blocked\b`)

// reportsBlocked reports whether an agent's final message says it was
// blocked: its first non-empty line starts with "Blocked", maybe after
// markdown such as ** or "- ". Only the first line, as the app's resolver
// prompt asks for; a later line such as "- Blocked-by links: kept both" is
// part of a report, not a verdict.
func reportsBlocked(msg string) bool {
	first, _, _ := strings.Cut(strings.TrimSpace(msg), "\n")
	return blocked.MatchString(first)
}

// updateBranch merges base into the committed clone dir of ref's branch. A
// merge, not a rebase: the branch is pushed and continued across runs. A
// conflicted merge is aborted, so the clone is as it was, and returned with
// its files and the branch's commits for next, which decides who, if
// anyone, resolves it.
func updateBranch(ctx context.Context, ws workspace.Workspace, ref, dir, base, author string) (*conflict, error) {
	files, err := ws.MergeBase(ctx, dir, base, author)
	if err != nil || len(files) == 0 {
		return nil, err
	}
	head, err := ws.Head(ctx, dir)
	if err != nil {
		return nil, err
	}
	commits, err := ws.Log(ctx, dir, base)
	if err != nil {
		return nil, err
	}
	return &conflict{head: head, files: files, last: cinderline.LastStep{
		Step: "merge", Issue: ref, Outcome: "conflict", Conflicts: files, Commits: &commits,
	}}, nil
}

// verifyMerge checks the resolver's result in order, so a later check only
// runs on a clean clone. Every failed check, an error too, means not resolved.
func verifyMerge(ctx context.Context, ws workspace.Workspace, dir, base, head string, conflicts []string) (bool, error) {
	if ok, err := ws.Clean(ctx, dir); !ok || err != nil {
		return false, err
	}
	if ok, err := ws.IsAncestor(ctx, dir, base, "HEAD"); !ok || err != nil {
		return false, err
	}
	// A reset to the base would also contain it: the branch's work must stay.
	if ok, err := ws.IsAncestor(ctx, dir, head, "HEAD"); !ok || err != nil {
		return false, err
	}
	markers, err := ws.ConflictMarkers(ctx, dir, conflicts)
	return !markers && err == nil, err
}
