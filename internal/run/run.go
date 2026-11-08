// Package run holds the steps of a run that the scheduler calls as hooks:
// recovery at start, the repository pin and the check before a claim. It
// keeps no workflow state (decision 0091): paused notes are a cache it may
// lose (decision 2026-11-05-runner-state-location).
package run

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

// PausedDir is the folder of a project's paused notes, one <REF>.json per
// paused issue, in the host state folder. The URL is part of the name because
// two instances may have projects with the same key.
func PausedDir(state string, p config.Project) string {
	return filepath.Join(state, "paused", url.QueryEscape(p.URL+" "+p.Key))
}

// Recover puts back what a previous process left: a project has one runner
// and it works on one issue at a time, so at start none of its In progress
// issues is really being worked on. First it removes the containers and
// networks a previous process left (review item 9), then it drops the paused
// notes of issues a person has since closed (0114).
func (p Project) Recover(ctx context.Context, c *cinderline.Client) error {
	if err := p.Env.Cleanup(ctx); err != nil {
		// Only hygiene: a failure must not keep the project from working.
		p.Log.Warn("stale containers and networks not removed", "project", p.Config.Key, "err", err)
	}
	dir := PausedDir(p.State, p.Config)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.Log.Warn("paused notes not read", "project", p.Config.Key, "err", err)
	}
	for _, e := range entries {
		ref, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		issue, err := c.GetIssue(ctx, ref)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A note the runner cannot check must not keep it from working.
			p.Log.Warn("paused note not checked", "project", p.Config.Key, "issue", ref, "err", err)
			continue
		}
		// A team's note on an issue left In progress resumes it after the restart (decision 0122).
		switch issue.Status.Value {
		case "blocked", "todo", "in_progress":
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			p.Log.Warn("paused note not dropped", "project", p.Config.Key, "issue", ref, "err", err)
			continue
		}
		p.Log.Info("paused note dropped", "project", p.Config.Key, "issue", ref, "status", issue.Status.Value)
	}

	issues, err := c.ListIssues(ctx, "in_progress")
	if err != nil {
		return err
	}
	for _, issue := range issues {
		var conflict *cinderline.ConflictError
		_, err := c.UpdateIssue(ctx, issue.Ref, cinderline.UpdateIssueRequest{Status: "todo", ExpectedStatus: "in_progress"})
		if errors.As(err, &conflict) {
			continue // someone moved it meanwhile; nothing to put back
		}
		if err != nil {
			return err
		}
		p.Log.Info("back to To do after a restart", "project", p.Config.Key, "issue", issue.Ref)
	}

	// Stranded branch recovery (decision 2026-11-07-publish-resilience point 3).
	return p.pushStranded(ctx, c)
}

// pushStranded pushes any stranded branches and opens pull requests for
// Review issues (decision 2026-11-07-publish-resilience point 3).
func (p Project) pushStranded(ctx context.Context, c *cinderline.Client) error {
	if p.Workspace == nil || p.Forge == nil {
		return nil
	}
	proj, err := c.GetProject(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.Log.Warn("stranded branches not checked", "project", p.Config.Key, "err", err)
		return nil
	}
	fg, err := p.Forge(proj.Forge)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.Log.Warn("stranded branches not checked", "project", p.Config.Key, "err", err)
		return nil
	}
	base, err := BaseBranch(ctx, c, fg)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.Log.Warn("stranded branches not checked", "project", p.Config.Key, "err", err)
		return nil
	}
	clones, err := p.Workspace.Stranded(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.Log.Warn("stranded listing failed", "project", p.Config.Key, "err", err)
		return nil
	}
	for _, clone := range clones {
		ref, ok := strings.CutPrefix(clone.Branch, name.Name+"/")
		if !ok {
			p.Log.Warn("stranded branch not checked", "project", p.Config.Key, "branch", clone.Branch, "err", "not a "+name.Name+" branch")
			continue
		}
		log := p.Log.With("project", p.Config.Key, "issue", ref)
		issue, err := c.GetIssue(ctx, ref)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Warn("stranded branch not checked", "err", err)
			continue
		}
		switch issue.Status.Value {
		case "done", "cancelled":
			log.Info("stranded branch of a closed issue left alone")
			continue
		}
		state, err := fg.MergeState(ctx, clone.Branch)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Warn("stranded branch not checked", "err", err)
			continue
		}
		switch state {
		case forge.MergeMerged, forge.MergeClosed:
			log.Info("stranded branch left alone", "pull_request_state", state)
			continue
		}
		err = p.Workspace.Push(ctx, clone.Dir, clone.Branch)
		if err != nil {
			log.Warn("stranded branch not pushed", "err", err)
			continue
		}
		log.Info("stranded branch pushed", "branch", clone.Branch)
		if issue.Status.Value != "review" {
			continue
		}
		pr, err := fg.OpenPullRequest(ctx, forge.PullRequestInput{
			Branch: clone.Branch,
			Base:   base,
			Title:  ref + ": " + issue.Title,
			Body:   pullRequestBody(ref, deref(issue.Body)),
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Warn("pull request not opened for a stranded branch", "err", err)
			continue
		}
		log.Info("pull request opened for a stranded branch", "pull_request", pr.URL)
	}
	return nil
}

// Pin refuses a repository from Cinderline other than the one configured on
// this machine, so the git token never follows a changed URL (0114).
func (p Project) Pin(ctx context.Context, c *cinderline.Client) error {
	proj, err := c.GetProject(ctx)
	if err != nil {
		return err
	}
	if proj.RepositoryURL == "" {
		return fmt.Errorf("the project in Cinderline names no repository for %s; this runner is configured for %s", p.Config.Key, p.Config.Repository)
	}
	if !config.SameRepository(proj.RepositoryURL, p.Config.Repository) {
		return fmt.Errorf("the project in Cinderline names the repository %s, but this runner is configured for %s", proj.RepositoryURL, p.Config.Repository)
	}
	return nil
}

// Check refuses, before the claim, an issue whose team has an agent this
// runner cannot run: an unsupported client, or a client without its key, so
// such a team does not drain To do (decision 0122).
func (p Project) Check(ctx context.Context, c *cinderline.Client, issue cinderline.Issue) error {
	if issue.Team == nil {
		return errors.New("the issue has no team")
	}
	team, err := c.GetTeam(ctx, issue.Team.ID)
	if err != nil {
		return err
	}
	for _, a := range team.Agents {
		kind, ok := keys[a.Client]
		if !ok {
			return fmt.Errorf("agent %s runs on %s; this runner supports only claude and opencode", a.Name, a.Client)
		}
		if _, _, err := p.Secrets.Get(p.Config, kind); err != nil {
			return fmt.Errorf("agent %s runs on %s, which needs this project's %s key: %w", a.Name, a.Client, kind, err)
		}
	}
	return nil
}
