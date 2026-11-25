package run

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/redact"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
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
	var found []workspace.Secret
	for attempt, wait := 1, publishBackoff; ; attempt, wait = attempt+1, wait*2 {
		var err error
		if conflicts, unchanged, pr, found, err = p.attempt(ctx, j, title); err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, workspace.ErrSecret) {
			// Not tried again: the scan would find the same commits. The
			// clone stays as it is (decision 2026-11-17-open-questions-answered).
			log.Warn("push stopped by the secret scan", "err", err)
			last.Outcome, last.Error, last.Secrets = "secret", ptr(err.Error()), stepSecrets(found)
			last.Commit = p.head(ctx, j)
			return last, nil
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
	last.Commit, last.Secrets = p.head(ctx, j), stepSecrets(found)
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
	state, err := p.awaitMerge(sctx, j.fg, j.branch, deref(last.Commit))
	if stopped(sctx) {
		log.Info("stopped during the publish, merge not waited for", "pull_request", pr.URL)
		last.Merge = ptr(string(forge.MergePending))
		return last, nil
	}
	if err != nil {
		return nil, err
	}
	last.Merge = ptr(string(state.State))
	if state.State == forge.MergeFailed {
		log.Info("CI failed", "pull_request", pr.URL, "checks", len(state.Failed))
		last.Checks = failedChecks(sctx, j.fg, log, state.Failed)
	}
	return last, nil
}

// attempt is one try of the publish: the base merge, the check whether the
// branch has anything the base lacks, the push and the pull request.
func (p *Project) attempt(ctx context.Context, j *job, title string) (conflicts []string, unchanged bool, pr forge.PullRequest, found []workspace.Secret, err error) {
	// Branch protection may block outdated pull requests.
	if conflicts, err = p.Workspace.MergeBase(ctx, j.dir, j.base, leadAuthor(j.team)); err != nil {
		return nil, false, pr, nil, err
	}
	if unchanged, err = p.Workspace.IsAncestor(ctx, j.dir, "HEAD", j.base); err != nil || unchanged {
		return conflicts, unchanged, pr, nil, err
	}
	// Built here from the ref, never passed in, so the push can never target the base.
	if found, err = p.Workspace.Push(ctx, j.dir, j.branch); err != nil {
		return nil, false, pr, found, err
	}
	pr, err = j.fg.OpenPullRequest(ctx, forge.PullRequestInput{Branch: j.branch, Base: j.base, Title: title, Body: pullRequestBody(j.ref, j.body)})
	return conflicts, false, pr, found, err
}

// awaitMerge polls the branch's pull request every mergeEvery until it is
// merged or closed, or its CI failed on head, the commit the run pushed
// ("": unknown, any head), and no other check of it still runs, so one
// report holds every failure; a failure on another head is an earlier
// push's (CLR-201). At mergeLimit it is failed when CI failed on head, else
// pending. A failed poll is logged and tried again.
func (p *Project) awaitMerge(ctx context.Context, fg forge.Forge, branch, head string) (forge.PullState, error) {
	for waited := time.Duration(0); ; waited += mergeEvery {
		state, err := fg.MergeState(ctx, branch)
		failed := err == nil && state.State == forge.MergeFailed && sameCommit(state.Head, head)
		switch {
		case ctx.Err() != nil:
			return forge.PullState{}, ctx.Err()
		case err != nil:
			p.Log.Warn("merge not checked, trying again", "project", p.Config.Key, "branch", branch, "err", err)
		case state.State == forge.MergeMerged || state.State == forge.MergeClosed:
			return state, nil
		case failed && !state.Running:
			return state, nil
		}
		if waited >= mergeLimit {
			if failed {
				return state, nil
			}
			return forge.PullState{State: forge.MergePending}, nil
		}
		if err := sleep(ctx, mergeEvery); err != nil {
			return forge.PullState{}, err
		}
	}
}

// sameCommit says whether the forge's head is the commit pushed; pushed ""
// is unknown and taken as any head. Bitbucket names a head by a short hash.
func sameCommit(head, pushed string) bool {
	return pushed == "" || (head != "" && strings.HasPrefix(pushed, head))
}

// failedChecks is the forge's failed checks as the app takes them, the
// first cinderline.MaxChecks, each with forge.Log: the end of its log, else
// the forge's words on it. A log that cannot be read is logged, never
// failing the step (CLR-201).
func failedChecks(ctx context.Context, fg forge.Forge, log *slog.Logger, failed []forge.Check) []cinderline.FailedCheck {
	out := []cinderline.FailedCheck{}
	for _, c := range failed[:min(len(failed), cinderline.MaxChecks)] {
		text, err := forge.Log(ctx, fg, c)
		if err != nil && ctx.Err() == nil {
			log.Warn("CI log not read, the check is sent with the forge's text", "check", c.Name, "err", err)
		}
		out = append(out, cinderline.FailedCheck{Name: c.Name, Description: c.Description, URL: c.URL, Log: text})
	}
	return out
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
