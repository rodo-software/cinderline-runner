package run

import (
	"context"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/redact"
)

// The publish step's retries of the base merge, push and pull request.
const (
	publishAttempts = 5
	publishBackoff  = 30 * time.Second
)

// The publish step's wait for a merge it asked for.
const (
	mergeEvery = 15 * time.Second
	mergeLimit = 15 * time.Minute
)

// publishStep commits, merges the base in, pushes, opens or finds the pull
// request and, when asked, asks the forge to merge it and waits. With
// resolve_conflicts a conflicting base merge is reported as a merge step
// first. The base merge, push and pull request are tried publishAttempts
// times. A stop from the poll at any time during the step lets the push and
// the pull request finish. Before the merge is asked for, it then is not
// asked for and merge stays null; once asked, it is not waited for and
// merge is pending: asked, not seen yet (decisions of 2026-11-17).
func (p *Project) publishStep(ctx context.Context, j *job, step cinderline.Step, pending **conflict) (*cinderline.LastStep, error) {
	log := p.Log.With("project", p.Config.Key, "issue", j.ref)
	author := leadAuthor(j.team)
	title := pullRequestTitle(j.ref, j.title)
	if _, err := p.Workspace.Commit(ctx, j.dir, author, title); err != nil {
		return nil, err
	}
	if step.ResolveConflicts {
		cf, err := updateBranch(ctx, p.Workspace, j.ref, j.dir, j.base, author)
		if err != nil {
			log.Warn("base not merged in, publish tries again", "err", err)
		}
		if cf != nil {
			*pending = cf
			return &cf.last, nil
		}
	}
	sctx, end := p.stoppable(ctx, j.ref) // only the merge and its wait use it
	defer end()
	last := &cinderline.LastStep{Step: "publish", Issue: j.ref}
	var pr forge.PullRequest
	var conflicts []string
	var unchanged bool
	for attempt, wait := 1, publishBackoff; ; attempt, wait = attempt+1, wait*2 {
		var err error
		if conflicts, unchanged, pr, err = p.attempt(ctx, j, title); err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt == publishAttempts {
			last.Outcome, last.Error = "failed", ptr(err.Error())
			last.Commit = p.head(ctx, j)
			return last, nil
		}
		log.Warn("publish failed, trying again", "attempt", attempt, "retry_in", wait, "err", err)
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	last.Commit = p.head(ctx, j)
	if unchanged {
		last.Outcome = "unchanged"
		return last, nil
	}
	last.Outcome, last.PullRequest, last.Conflicts = "published", &pr.URL, conflicts
	if !step.Merge {
		return last, nil
	}
	if stopped(sctx) {
		log.Info("stopped during the publish, merge not asked for", "pull_request", pr.URL)
		return last, nil
	}
	if len(conflicts) > 0 {
		last.Merge = ptr("failed") // a conflicted pull request cannot merge
		return last, nil
	}
	if err := j.fg.RequestMerge(ctx, pr); err != nil {
		last.Merge, last.Error = ptr("failed"), ptr("asking the forge to merge "+pr.URL+" failed: "+err.Error())
		return last, nil
	}
	log.Info("waiting for the merge", "pull_request", pr.URL)
	state, err := p.awaitMerge(sctx, j.fg, j.branch)
	if stopped(sctx) {
		log.Info("stopped during the publish, merge not waited for", "pull_request", pr.URL)
		last.Merge = ptr(string(forge.MergePending))
		return last, nil
	}
	if err != nil {
		return nil, err
	}
	last.Merge = ptr(string(state))
	return last, nil
}

// attempt is one try of the publish: the base merge, the check whether the
// branch has anything the base lacks, the push and the pull request.
func (p *Project) attempt(ctx context.Context, j *job, title string) (conflicts []string, unchanged bool, pr forge.PullRequest, err error) {
	// Branch protection may block outdated pull requests.
	if conflicts, err = p.Workspace.MergeBase(ctx, j.dir, j.base, leadAuthor(j.team)); err != nil {
		return nil, false, pr, err
	}
	if unchanged, err = p.Workspace.IsAncestor(ctx, j.dir, "HEAD", j.base); err != nil || unchanged {
		return conflicts, unchanged, pr, err
	}
	// Built here from the ref, never passed in, so the push can never target the base.
	if err = p.Workspace.Push(ctx, j.dir, j.branch); err != nil {
		return nil, false, pr, err
	}
	pr, err = j.fg.OpenPullRequest(ctx, forge.PullRequestInput{Branch: j.branch, Base: j.base, Title: title, Body: pullRequestBody(j.ref, j.body)})
	return conflicts, false, pr, err
}

// awaitMerge polls the branch's pull request every mergeEvery until it is
// merged or closed, or mergeLimit has passed: then it is pending, or failed
// when its checks failed. A failed poll is logged and tried again.
func (p *Project) awaitMerge(ctx context.Context, fg forge.Forge, branch string) (forge.MergeState, error) {
	for waited := time.Duration(0); ; waited += mergeEvery {
		state, err := fg.MergeState(ctx, branch)
		switch {
		case ctx.Err() != nil:
			return "", ctx.Err()
		case err != nil:
			p.Log.Warn("merge not checked, trying again", "project", p.Config.Key, "branch", branch, "err", err)
		case state == forge.MergeMerged || state == forge.MergeClosed:
			return state, nil
		}
		if waited >= mergeLimit {
			if state == forge.MergeFailed {
				return state, nil
			}
			return forge.MergePending, nil
		}
		if err := sleep(ctx, mergeEvery); err != nil {
			return "", err
		}
	}
}

// head is the clone's commit; nil when unknown.
func (p *Project) head(ctx context.Context, j *job) *string {
	if h, err := p.Workspace.Head(ctx, j.dir); err == nil {
		return &h
	}
	return nil
}

// pullRequestTitle is the title of the issue's pull request.
func pullRequestTitle(ref, title string) string { return redact.String(ref + ": " + title) }

// pullRequestBody is the body of the issue's pull request; body is the issue's text, untrusted.
func pullRequestBody(ref, body string) string {
	return redact.String(strings.TrimSpace("Resolves " + ref + " in Cinderline.\n\n" + body))
}
