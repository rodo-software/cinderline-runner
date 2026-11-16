package run

import (
	"context"
	"crypto/rand"
	"regexp"
	"slices"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// conflict is a base merge that conflicted and was aborted: the facts next
// gets as its last step, and what verifyMerge checks a resolution against.
type conflict struct {
	head  string   // the branch's head before the merge
	files []string // untrusted, from the repository
	last  cinderline.LastStep
}

var blocked = regexp.MustCompile(`(?im)^[\s*_>#-]*blocked\b`)

// reportsBlocked reports whether an agent's final message says it was
// blocked: a line that starts with "Blocked", maybe after markdown such as
// ** or "- ", as legacy lib.mjs reportsBlocked.
func reportsBlocked(msg string) bool { return blocked.MatchString(msg) }

// UpdateBranch merges base into the committed clone dir of ref's branch. A
// merge, not a rebase: the branch is pushed and continued across runs. A
// conflicted merge is aborted, so the clone is as it was, and returned with
// its files and the branch's commits for next, which decides who, if
// anyone, resolves it (decision 2026-11-15-server-decides-steps point 7).
func UpdateBranch(ctx context.Context, ws workspace.Workspace, ref, dir, base, author string) (*conflict, error) {
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
		ID: rand.Text(), Step: "merge", Issue: ref, Outcome: "conflict", Conflicts: files, Commits: &commits,
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

// CatchUp merges the base into continued work before its first step
// (decision 2026-11-04-agent-resolves-merge-conflicts). What a paused turn
// left is committed first as "<REF>: <title>". It returns a conflict for the
// run's first request; an error only logs: the run goes on, and publish
// tries again.
func (p Project) CatchUp(ctx context.Context, ws workspace.Workspace, ref, dir, base, title string, team cinderline.Team) *conflict {
	log := p.Log.With("project", p.Config.Key, "issue", ref)
	if _, err := ws.Commit(ctx, dir, leadAuthor(team), ref+": "+title); err != nil {
		log.Warn("base not merged in", "err", err)
		return nil
	}
	cf, err := UpdateBranch(ctx, ws, ref, dir, base, leadAuthor(team))
	switch {
	case err != nil:
		log.Warn("base not merged in", "err", err)
	case cf != nil:
		log.Info("base not merged in", "base", base, "conflicts", cf.files)
	}
	return cf
}

// ReviewBranches keeps the pull requests of Review issues current: it
// merges base into the branch of every top-level Review issue with a team
// that is behind it, and pushes it. A trusted team's merge is asked for
// again, as Forgejo drops a scheduled merge when the head gets new commits;
// only an open pull request, so one a person closed is never opened again.
// A conflict goes to next, which today answers none and sets a trusted
// issue's merge_status itself; no agent runs here (decisions
// 2026-11-04-agent-resolves-merge-conflicts,
// 2026-11-04-runner-updates-branch-before-push,
// 2026-11-15-server-decides-steps point 7). An issue that fails is logged
// and the next one checked.
//
// A fresh clone replaces the issue's old one, so a clone with commits the
// remote lacks (a failed run or publish) is pushed first, and while that
// fails its issue is skipped: the work is never removed unpushed. One fetch
// then says which branches are behind; only those get a clone (decision
// 2026-11-11-audit-fixes).
//
// shortcut: one page of Review issues, as the API returns them, like the claim's To do list; page if Review outgrows it.
func (p Project) ReviewBranches(ctx context.Context, c *cinderline.Client, ws workspace.Workspace, fg forge.Forge, base string) error {
	issues, err := c.ListReview(ctx, cinderline.PollReview)
	if err != nil {
		return err
	}
	clones, err := ws.Stranded(ctx)
	if err != nil {
		return err // unknown, so no clone may be replaced
	}
	stranded := map[string]workspace.Clone{}
	for _, clone := range clones {
		stranded[clone.Branch] = clone
	}
	keep := map[string]bool{}
	for _, issue := range issues {
		branch := name.Name + "/" + issue.Ref
		clone, ok := stranded[branch]
		if !ok || issue.Team == nil || issue.Parent != nil {
			continue
		}
		pushed, err := p.pushClone(ctx, ws, fg, base, clone, issue)
		if err != nil {
			return err
		}
		keep[branch] = !pushed
	}
	behind, err := ws.Behind(ctx, base)
	if err != nil {
		return err
	}
	teams := map[int]cinderline.Team{}
	for _, issue := range issues {
		branch := name.Name + "/" + issue.Ref
		if issue.Team == nil || issue.Parent != nil { // a sub-issue's branch is its parent's
			continue
		}
		if keep[branch] || !slices.Contains(behind, branch) {
			continue
		}
		if _, ok := teams[issue.Team.ID]; !ok {
			team, err := c.GetTeam(ctx, issue.Team.ID)
			if err != nil {
				p.Log.Warn("branch not updated", "project", p.Config.Key, "issue", issue.Ref, "err", err)
				continue
			}
			teams[issue.Team.ID] = team
		}
		if err := p.reviewBranch(ctx, c, ws, fg, issue, base, teams[issue.Team.ID]); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p.Log.Warn("branch not updated", "project", p.Config.Key, "issue", issue.Ref, "err", err)
		}
	}
	return nil
}

// reviewBranch brings one Review issue's branch up to date with base.
func (p Project) reviewBranch(ctx context.Context, c *cinderline.Client, ws workspace.Workspace, fg forge.Forge, issue cinderline.Issue, base string, team cinderline.Team) error {
	log := p.Log.With("project", p.Config.Key, "issue", issue.Ref)
	branch := name.Name + "/" + issue.Ref
	// A branch the remote lacks starts from base, so the first check skips it.
	dir, err := ws.Prepare(ctx, branch, base)
	if err != nil {
		return err
	}
	if merged, err := ws.IsAncestor(ctx, dir, "HEAD", base); merged || err != nil {
		return err
	}
	if current, err := ws.IsAncestor(ctx, dir, base, "HEAD"); current || err != nil {
		return err
	}
	cf, err := UpdateBranch(ctx, ws, issue.Ref, dir, base, leadAuthor(team))
	if err != nil {
		return err
	}
	if cf != nil {
		log.Info("behind the base with conflicts", "base", base, "conflicts", cf.files)
		return p.reviewConflict(ctx, c, issue.Ref, branch, base, cf)
	}
	// Never the base branch: the target is always the issue's own.
	if err := ws.Push(ctx, dir, branch); err != nil {
		return err
	}
	log.Info("base merged in", "base", base, "branch", branch)

	// Only a trusted team merges without a person (0114).
	if !team.Trusted {
		return nil
	}
	pr, open, err := fg.FindPullRequest(ctx, branch)
	if err != nil || !open {
		return err
	}
	if err := fg.RequestMerge(ctx, pr); err != nil {
		log.Warn("merge not requested", "pull_request", pr.URL, "err", err)
	}
	return nil
}

// reviewConflict reports a Review branch's conflict to next, once per
// branch head and conflicting files: a cache it may lose, so a conflicted
// branch costs no request per poll. In Review the runner does only none;
// another step is logged (decision 2026-11-15-server-decides-steps point
// 7). A failed call is the caller's to log, and the next tick asks again.
func (p Project) reviewConflict(ctx context.Context, c *cinderline.Client, ref, branch, base string, cf *conflict) error {
	// shortcut: the branch's head and files, not the base's commit, as the workspace names no remote commit; add one if a moved base must ask again.
	key := cf.head + "\x00" + strings.Join(cf.files, "\x00")
	if p.run.askedAbout(ref) == key {
		return nil
	}
	resp, err := c.Next(ctx, ref, cinderline.NextRequest{
		Context: "review", Last: &cf.last, Sessions: []cinderline.ResumableSession{},
		Workspace: cinderline.RunWorkspace{Branch: branch, Base: base, ExtraRepositories: []cinderline.WorkspaceRepository{}, LinkedProjects: []cinderline.WorkspaceLink{}},
	})
	if err != nil {
		return err
	}
	p.run.setAsked(ref, key)
	if k := resp.Step.Kind; k != "none" {
		p.Log.Warn("step not done in Review", "project", p.Config.Key, "issue", ref, "kind", k, "runner", c.Version())
	}
	return nil
}

// setMergeStatus writes a trusted issue's merge_status (decision
// 2026-11-04-merge-status); "" writes null. A failed write only logs.
func (p Project) setMergeStatus(ctx context.Context, c *cinderline.Client, ref, status string) {
	req := cinderline.UpdateIssueRequest{MergeStatus: status, ClearMergeStatus: status == ""}
	if _, err := c.UpdateIssue(ctx, ref, req); err != nil {
		p.Log.Warn("merge status not set", "project", p.Config.Key, "issue", ref, "err", err)
	}
}
