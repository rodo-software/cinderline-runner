package run

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/redact"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// The merge wait of a trusted team's pull request (decision
// 2026-11-04-runner-waits-for-merge).
const (
	mergeEvery = 15 * time.Second
	mergeLimit = 15 * time.Minute
)

// Publish retries the base merge, push and pull request five times with
// backoff (decision 2026-11-07-publish-resilience).
const (
	publishAttempts = 5
	publishBackoff  = 30 * time.Second
)

// sleep waits d or until ctx is done; a variable so tests need not wait.
var sleep = forge.Sleep

// mergeStatusOf is an issue's merge_status for where its pull request stands
// (decision 2026-11-04-merge-status), as legacy MERGE_STATUS. An open pull
// request, also at the wait's limit, maps to "": null, unknown.
var mergeStatusOf = map[forge.MergeState]string{
	forge.MergeMerged: "success",
	forge.MergeClosed: "issue",
	forge.MergeFailed: "issue",
}

// BaseBranch is the branch runs start from and pull requests target: the
// project's base branch, else the remote's default branch (decision
// 2026-11-04-base-branch).
func BaseBranch(ctx context.Context, c *cinderline.Client, fg forge.Forge) (string, error) {
	proj, err := c.GetProject(ctx)
	if err != nil {
		return "", err
	}
	if proj.BaseBranch != "" {
		return proj.BaseBranch, nil
	}
	b, err := fg.DefaultBranch(ctx)
	if err == nil && b == "" {
		err = errors.New("the forge names none")
	}
	if err != nil {
		return "", fmt.Errorf("could not find the repository's default branch; set a base branch on the project in Cinderline (%w)", err)
	}
	return b, nil
}

// Pub is one issue's publish, after its run.
type Pub struct {
	Ref, Title string
	Body       string // the issue's text, untrusted; the pull request's text
	Base       string
	Dir        string
	MaxTurns   int // the Merge Resolver's
	Team       cinderline.Team
	Forge      string // the project's forge, as GET project names it, for the report
	Unread     bool   // reading a person's comments failed twice: add UnreadComments
	// Session and Token are the solo orchestrator's session it just worked in
	// and its scoped Cinderline token, for the merge attempt before the push
	// (decision 2026-11-07-solo-team-resolves-merge-conflicts); never logged.
	Session, Token string
}

// Published is what a publish opened, for FinishReport.
type Published struct {
	URL, Base string
	Conflicts []string
	Merge     forge.MergeState // a trusted team's wait; MergePending: the limit passed
}

// pullRequestBody builds the body of a pull request from the issue reference
// and body text, shared by Publish and the stranded branch recovery
// (decision 2026-11-07-publish-resilience).
func pullRequestBody(ref, body string) string {
	return redact.String(strings.TrimSpace("Resolves " + ref + " in Cinderline.\n\n" + body))
}

// pullRequestTitle is the title of the issue's pull request, redacted as the body.
func pullRequestTitle(ref, title string) string { return redact.String(ref + ": " + title) }

// Publish ends a finished run, as legacy publish() and runIssue(): commit
// what was left, merge the base in with UpdateBranch, push cinderline/<REF>
// (never the base), open or find the pull request and, for a trusted team
// without conflicts, request the merge and wait for it every 15 seconds for
// at most 15 minutes, holding the project's slot. A trusted issue's
// merge_status is set. Then the report is commented and the issue moved to
// Review. A conflict left is pushed as it is, for a person to resolve.
// Errors are the run's: the scheduler comments them and moves the issue to
// Review.
func (p Project) Publish(ctx context.Context, c *cinderline.Client, clients map[string]client.Client, ws workspace.Workspace, fg forge.Forge, pub Pub) error {
	log := p.Log.With("project", p.Config.Key, "issue", pub.Ref)
	// Built here, never passed in, so the push can never target the base.
	branch := name.Name + "/" + pub.Ref
	title := pullRequestTitle(pub.Ref, pub.Title)
	if _, err := ws.Commit(ctx, pub.Dir, title); err != nil {
		return err
	}

	var pr forge.PullRequest
	var published *Published
	var conflicts []string
	for attempt, wait := 1, publishBackoff; ; attempt, wait = attempt+1, wait*2 {
		var nothing bool
		var err error
		if conflicts, nothing, pr, err = p.attempt(ctx, c, clients, ws, fg, pub, branch, title); err == nil {
			if !nothing {
				published = &Published{URL: pr.URL, Base: pub.Base, Conflicts: conflicts}
			}
			break
		}
		if errors.Is(err, errBudget) {
			return nil // checkBudget pushed the work and blocked the issue
		}
		if ctx.Err() != nil {
			return err
		}
		if attempt == publishAttempts {
			head, herr := ws.Head(ctx, pub.Dir)
			if herr != nil {
				head = "an unpushed commit"
			}
			return fmt.Errorf("publishing %s failed %d times, last: %w; commit %s on branch %s in the run's clone is not pushed", pub.Ref, publishAttempts, err, head, branch)
		}
		log.Warn("publish failed, trying again", "attempt", attempt, "retry_in", wait, "err", err)
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}

	// Only a trusted team merges without a person (0114).
	if published != nil && pub.Team.Trusted {
		if len(conflicts) > 0 {
			p.setMergeStatus(ctx, c, pub.Ref, "issue")
		} else {
			if err := fg.RequestMerge(ctx, pr); err != nil {
				p.setMergeStatus(ctx, c, pub.Ref, "issue")
				return fmt.Errorf("pull request %s is open, but asking %s to merge it failed: %w", pr.URL, forgeName(pub.Forge), err)
			}
			// In memory only: a restart does not wait (decision 0091).
			p.Log.Info("waiting for the merge", "project", p.Config.Key, "issue", pub.Ref, "pull_request", pr.URL)
			merge, err := p.awaitMerge(ctx, fg, branch)
			if err != nil {
				return err
			}
			published.Merge = merge
			p.setMergeStatus(ctx, c, pub.Ref, mergeStatusOf[published.Merge])
		}
	}

	report := FinishReport(published, pub.Team.Trusted, pub.Forge)
	if pub.Unread {
		report += UnreadComments
	}
	if _, err := c.AddComment(ctx, pub.Ref, report); err != nil {
		return err
	}
	// Refused when the issue is no longer In progress, for example after a question (decision 0116).
	if _, err := c.UpdateIssue(ctx, pub.Ref, cinderline.UpdateIssueRequest{Status: "review", ExpectedStatus: "in_progress"}); err != nil {
		log.Warn("not moved to Review", "err", err)
	}
	return nil
}

// attempt does one attempt of the publish loop: UpdateBranch, IsAncestor,
// push and OpenPullRequest. It returns the conflicts, whether nothing was
// pushed (HEAD already in the base), the pull request, and any error.
func (p Project) attempt(ctx context.Context, c *cinderline.Client, clients map[string]client.Client, ws workspace.Workspace, fg forge.Forge, pub Pub, branch, title string) (conflicts []string, nothing bool, pr forge.PullRequest, err error) {
	// The branch protection blocks outdated pull requests (decision 0121).
	conflicts, err = p.UpdateBranch(ctx, c, clients, ws, Upkeep{
		Ref: pub.Ref, Branch: branch, Base: pub.Base, Dir: pub.Dir, MaxTurns: pub.MaxTurns, Team: pub.Team, Session: pub.Session, Token: pub.Token,
	})
	if err != nil {
		return nil, false, forge.PullRequest{}, err
	}
	nothing, err = ws.IsAncestor(ctx, pub.Dir, "HEAD", pub.Base)
	if err != nil {
		return nil, false, forge.PullRequest{}, err
	}
	if nothing {
		return conflicts, true, forge.PullRequest{}, nil
	}
	pr, err = p.open(ctx, c, ws, fg, pub, branch, title)
	if err != nil {
		return nil, false, forge.PullRequest{}, err
	}
	return conflicts, false, pr, nil
}

// open pushes the branch, posts the progress line and opens the pull request.
// It is the retried part of Publish (decision 2026-11-07-publish-resilience).
func (p Project) open(ctx context.Context, c *cinderline.Client, ws workspace.Workspace, fg forge.Forge, pub Pub, branch, title string) (forge.PullRequest, error) {
	if err := ws.Push(ctx, pub.Dir, branch); err != nil {
		return forge.PullRequest{}, err
	}
	p.progress(ctx, c, pub.Ref, "Opening the pull request")
	return fg.OpenPullRequest(ctx, forge.PullRequestInput{
		Branch: branch,
		Base:   pub.Base,
		Title:  title,
		Body:   pullRequestBody(pub.Ref, pub.Body),
	})
}

// awaitMerge polls the branch's pull request every mergeEvery until it is
// merged or closed, or mergeLimit has passed; then it returns MergePending.
// A failed poll is logged and retried. Time is counted in sleeps, so a slow
// forge stretches the limit a little, as legacy awaitMerge.
func (p Project) awaitMerge(ctx context.Context, fg forge.Forge, branch string) (forge.MergeState, error) {
	for waited := time.Duration(0); ; waited += mergeEvery {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		state, err := fg.MergeState(ctx, branch)
		switch {
		case err != nil:
			p.Log.Warn("merge not checked, trying again", "project", p.Config.Key, "branch", branch, "err", err)
		case state == forge.MergeMerged || state == forge.MergeClosed:
			return state, nil
		}
		if waited >= mergeLimit {
			return forge.MergePending, nil
		}
		if err := sleep(ctx, mergeEvery); err != nil {
			return "", err
		}
	}
}

// forgeName is the forge's name for people, as legacy forgeName.
func forgeName(f string) string {
	if n, ok := map[string]string{"github": "GitHub", "gitlab": "GitLab", "bitbucket": "Bitbucket"}[f]; ok {
		return n
	}
	return "Forgejo"
}

// FinishReport is the comment that ends a published run, the text of legacy
// lib.mjs finishReport; pub is nil when the run changed nothing.
func FinishReport(pub *Published, trusted bool, forgeKind string) string {
	switch {
	case pub == nil:
		return "The run finished without changing any files, so there is no pull request."
	case len(pub.Conflicts) > 0:
		then := "merge it"
		if trusted {
			then = "let CI merge it"
		}
		return "Opened " + pub.URL + ". It conflicts with " + pub.Base + " in " + strings.Join(pub.Conflicts, ", ") + "; resolve the conflict by hand, then " + then + "."
	case !trusted:
		return "Opened " + pub.URL + ". Review it and merge it; then move this issue to done."
	case pub.Merge == forge.MergeMerged:
		return "Merged " + pub.URL + "."
	case pub.Merge == forge.MergeClosed:
		return "Opened " + pub.URL + ", but it was closed without merging."
	}
	return "Opened " + pub.URL + ". " + forgeName(forgeKind) + " merges it when CI is green; it had not merged after 15 minutes, so the runner moved on."
}

// CheckMerges brings the merge_status of the project's trusted top-level
// Review issues up to date with their pull requests (decision
// 2026-11-04-merge-status), on the MERGE_POLL_SECONDS timer. Issues already
// at success are skipped; an open pull request or none says nothing new. An
// issue that fails is logged and the next one checked. An issue found
// without a pull request is asked again only after noPullRequestFor.
//
// shortcut: one page of Review issues, as the API returns them, like ReviewBranches; page if Review outgrows it.
func (p Project) CheckMerges(ctx context.Context, c *cinderline.Client, fg forge.Forge) error {
	issues, err := c.ListIssues(ctx, "review")
	if err != nil {
		return err
	}
	trusted := map[int]bool{}
	for _, issue := range issues {
		current := deref(issue.MergeStatus)
		if issue.Team == nil || issue.Parent != nil || current == "success" {
			continue
		}
		log := p.Log.With("project", p.Config.Key, "issue", issue.Ref)
		if _, ok := trusted[issue.Team.ID]; !ok {
			team, err := c.GetTeam(ctx, issue.Team.ID)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				log.Warn("merge status not checked", "err", err)
				continue
			}
			trusted[issue.Team.ID] = team.Trusted
		}
		if !trusted[issue.Team.ID] {
			continue
		}
		if p.run.withoutPullRequest(issue.Ref) {
			continue
		}
		state, err := fg.MergeState(ctx, name.Name+"/"+issue.Ref)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Warn("merge status not checked", "err", err)
			continue
		}
		p.run.setPullRequest(issue.Ref, state != "")
		if status := mergeStatusOf[state]; status != "" && status != current {
			p.setMergeStatus(ctx, c, issue.Ref, status)
		}
	}
	return nil
}
