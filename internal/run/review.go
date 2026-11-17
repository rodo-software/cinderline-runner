package run

import (
	"context"
	"fmt"
	"runtime/debug"
	"slices"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// pullRequestState is a forge's merge state as the poll's review facts name it.
var pullRequestState = map[forge.MergeState]string{
	"":                 "none",
	forge.MergePending: "open",
	forge.MergeFailed:  "failed",
	forge.MergeMerged:  "merged",
	forge.MergeClosed:  "closed",
}

// upkeep keeps the Review branches the app listed current, one pass, and
// returns what it found for the next poll. Per item it pushes a stranded
// clone first, and opens the pull request when there is none; merges the
// base into a branch that is behind and pushes it, or records the files
// when that merge conflicts (aborted, nothing pushed); asks the forge to
// merge again when the item allows it and the branch was updated, as a
// forge drops a scheduled merge when the head gets new commits; and records
// where the pull request stands. An item that fails is logged and left
// out; a failure of the whole pass only logs.
func (p *Project) upkeep(ctx context.Context, proj cinderline.Project, items []cinderline.ReviewItem) (facts []cinderline.ReviewFact) {
	facts = []cinderline.ReviewFact{}
	defer func() {
		if r := recover(); r != nil {
			p.Log.Error("panic in the review upkeep", "project", p.Config.Key, "err", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	if len(items) == 0 || p.Workspace == nil {
		return facts
	}
	fail := func(err error) []cinderline.ReviewFact {
		if ctx.Err() == nil {
			p.Log.Warn("review branches not kept current", "project", p.Config.Key, "err", err)
		}
		return facts
	}
	fg, err := p.Forge(proj.Forge)
	if err != nil {
		return fail(err)
	}
	base, err := baseBranch(ctx, proj, fg)
	if err != nil {
		return fail(err)
	}
	stranded, err := p.Workspace.Stranded(ctx)
	if err != nil {
		return fail(err) // unknown, so no clone may be replaced
	}
	// One fetch says which branches are behind; only those get a clone.
	behind, err := p.Workspace.Behind(ctx, base)
	if err != nil {
		return fail(err)
	}
	for _, it := range items {
		// The ref becomes a branch and a folder.
		if !workspace.IssueRef.MatchString(it.Ref) {
			continue
		}
		if f, ok := p.keepCurrent(ctx, fg, base, it, stranded, behind); ok {
			facts = append(facts, f)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return facts
}

// keepCurrent does one item of upkeep; ok is false when it could not tell
// where the pull request stands.
func (p *Project) keepCurrent(ctx context.Context, fg forge.Forge, base string, it cinderline.ReviewItem, stranded []workspace.Clone, behind []string) (cinderline.ReviewFact, bool) {
	log := p.Log.With("project", p.Config.Key, "issue", it.Ref)
	branch := name.Name + "/" + it.Ref
	fact := cinderline.ReviewFact{Ref: it.Ref, Conflicts: []string{}}
	// shortcut: one forge request per Review issue and pass, no cache of
	// pull requests that stay the same; keep one if a forge's rate limit bites.
	state, err := fg.MergeState(ctx, branch)
	if err != nil {
		log.Warn("pull request not checked", "err", err)
		return fact, false
	}
	open := state == forge.MergePending || state == forge.MergeFailed
	if i := slices.IndexFunc(stranded, func(c workspace.Clone) bool { return c.Branch == branch }); i >= 0 && (open || state == "") {
		if _, err := p.Workspace.Push(ctx, stranded[i].Dir, branch); err != nil {
			// The unpushed work must not be replaced by a fresh clone.
			log.Warn("stranded branch not pushed", "err", err)
			fact.PullRequest = pullRequestState[state]
			return fact, true
		}
		log.Info("stranded branch pushed", "branch", branch)
		if state == "" {
			pr, err := fg.OpenPullRequest(ctx, forge.PullRequestInput{Branch: branch, Base: base, Title: pullRequestTitle(it.Ref, it.Title), Body: pullRequestBody(it.Ref, "")})
			if err != nil {
				log.Warn("pull request not opened", "err", err)
			} else {
				log.Info("pull request opened", "pull_request", pr.URL)
				state, open = forge.MergePending, true
			}
		}
	}
	if open && slices.Contains(behind, branch) {
		updated, conflicts, err := p.updateReviewBranch(ctx, it, branch, base)
		switch {
		case err != nil:
			log.Warn("branch not updated", "err", err)
		case len(conflicts) > 0:
			log.Info("behind the base with conflicts", "base", base, "conflicts", conflicts)
			fact.Conflicts = conflicts
		case updated && it.Merge:
			if pr, found, err := fg.FindPullRequest(ctx, branch); err != nil {
				log.Warn("merge not asked for", "err", err)
			} else if found {
				if err := fg.RequestMerge(ctx, pr); err != nil {
					log.Warn("merge not asked for", "pull_request", pr.URL, "err", err)
				}
			}
		}
	}
	fact.PullRequest = pullRequestState[state]
	return fact, true
}

// updateReviewBranch merges base into the branch in a fresh clone and pushes
// it, unless it is merged or current already; a conflicted merge is aborted
// and its files returned. The work of a clone it replaces is kept first.
func (p *Project) updateReviewBranch(ctx context.Context, it cinderline.ReviewItem, branch, base string) (updated bool, conflicts []string, err error) {
	ws, ref := p.Workspace, it.Ref
	if err := p.keepWork(ctx, ref, "", ref+": "+it.Title); err != nil {
		return false, nil, err
	}
	dir, err := ws.Prepare(ctx, branch, base)
	if err != nil {
		return false, nil, err
	}
	if merged, err := ws.IsAncestor(ctx, dir, "HEAD", base); merged || err != nil {
		return false, nil, err
	}
	if current, err := ws.IsAncestor(ctx, dir, base, "HEAD"); current || err != nil {
		return false, nil, err
	}
	cf, err := updateBranch(ctx, ws, ref, dir, base, "")
	if err != nil || cf != nil {
		var files []string
		if cf != nil {
			files = cf.files
		}
		return false, files, err
	}
	// Never the base branch: the target is always the issue's own.
	if _, err := ws.Push(ctx, dir, branch); err != nil {
		return false, nil, err
	}
	p.Log.Info("base merged in", "project", p.Config.Key, "issue", ref, "base", base, "branch", branch)
	return true, nil, nil
}
