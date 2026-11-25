package forge

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

var _ Forge = (*GitHub)(nil)

// GitHub is the Forge of a repository on github.com or on a GitHub
// Enterprise Server host (decisions 0117, 2026-11-04-github-enterprise).
// Its token stays in this process: it is sent only to the repository's own
// API host and never put in an error or a log line.
type GitHub struct {
	HTTP *http.Client // set by NewGitHub; replace it before first use, e.g. in tests

	repo    string // REST base of the repository: <api>/repos/<owner>/<repo>
	graphql string
	owner   string
	token   string
}

// NewGitHub returns the Forge of repository, given as https://host/o/r,
// with or without .git. On github.com the API is api.github.com; on any
// other host it is GitHub Enterprise Server's https://<host>/api/v3, with
// GraphQL beside it at /api/graphql, with the repository's port.
func NewGitHub(repository, token string) (*GitHub, error) {
	host, path, err := parseRepository(repository)
	if err != nil {
		return nil, err
	}
	owner, repo, ok := strings.Cut(path, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return nil, fmt.Errorf("repository URL %q does not name a GitHub owner/repository", repository)
	}
	api, graphql := "https://"+host+"/api/v3", "https://"+host+"/api/graphql"
	if host == "github.com" {
		api, graphql = "https://api.github.com", "https://api.github.com/graphql"
	}
	return &GitHub{
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		repo:    api + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo),
		graphql: graphql,
		owner:   owner,
		token:   token,
	}, nil
}

// githubPull is the part of a GitHub pull request the runner reads.
type githubPull struct {
	Number   int    `json:"number"`
	HTMLURL  string `json:"html_url"`
	NodeID   string `json:"node_id"`
	State    string `json:"state"`
	MergedAt string `json:"merged_at"`
	Head     struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

func (g *GitHub) DefaultBranch(ctx context.Context) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	err := g.do(ctx, http.MethodGet, g.repo, nil, &r)
	return r.DefaultBranch, err
}

func (g *GitHub) OpenPullRequest(ctx context.Context, in PullRequestInput) (PullRequest, error) {
	var pr githubPull
	err := g.do(ctx, http.MethodPost, g.repo+"/pulls", map[string]string{
		"head": in.Branch, "base": in.Base, "title": in.Title, "body": in.Body,
	}, &pr)
	// GitHub answers 422 when the branch already has an open pull request; other 422s, such as no commits, find none.
	if status(err) == http.StatusUnprocessableEntity {
		if found, ok, ferr := g.FindPullRequest(ctx, in.Branch); ferr == nil && ok {
			return found, nil
		}
	}
	if err != nil {
		return PullRequest{}, err
	}
	return PullRequest{Number: pr.Number, URL: pr.HTMLURL}, nil
}

func (g *GitHub) FindPullRequest(ctx context.Context, branch string) (PullRequest, bool, error) {
	var prs []githubPull
	if err := g.do(ctx, http.MethodGet, g.pulls("open", branch), nil, &prs); err != nil {
		return PullRequest{}, false, err
	}
	for _, pr := range prs {
		if pr.Head.Ref == branch {
			return PullRequest{Number: pr.Number, URL: pr.HTMLURL}, true, nil
		}
	}
	return PullRequest{}, false, nil
}

// RequestMerge enables auto-merge with squash over GraphQL, the only GitHub
// way to merge once checks pass. It needs "Allow auto-merge" and a protected
// base branch with required checks; deleting the branch is the repository's
// own setting (0117). A 405 is not retried, unlike on Forgejo.
func (g *GitHub) RequestMerge(ctx context.Context, pr PullRequest) error {
	// The mutation takes the node ID, which PullRequest does not carry, so it is read first.
	var p githubPull
	if err := g.do(ctx, http.MethodGet, g.repo+"/pulls/"+strconv.Itoa(pr.Number), nil, &p); err != nil {
		return err
	}
	if p.NodeID == "" {
		return fmt.Errorf("GitHub named no node ID for pull request %d", pr.Number)
	}
	var r struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	err := g.do(ctx, http.MethodPost, g.graphql, map[string]any{
		"query":     "mutation($id: ID!) { enablePullRequestAutoMerge(input: { pullRequestId: $id, mergeMethod: SQUASH }) { clientMutationId } }",
		"variables": map[string]string{"id": p.NodeID},
	}, &r)
	if err != nil {
		return err
	}
	if len(r.Errors) > 0 {
		msgs := make([]string, len(r.Errors))
		for i, e := range r.Errors {
			msgs[i] = e.Message
		}
		return &apiError{"GitHub", http.MethodPost, g.graphql, http.StatusOK, strings.Join(msgs, "; ")}
	}
	return nil
}

func (g *GitHub) MergeState(ctx context.Context, branch string) (PullState, error) {
	var prs []githubPull
	if err := g.do(ctx, http.MethodGet, g.pulls("all", branch), nil, &prs); err != nil {
		return PullState{}, err
	}
	if len(prs) == 0 {
		return PullState{}, nil
	}
	pr := prs[0] // the newest: GitHub lists by creation, newest first
	switch {
	case pr.MergedAt != "":
		return PullState{State: MergeMerged, Head: pr.Head.SHA}, nil
	case pr.State == "closed":
		return PullState{State: MergeClosed, Head: pr.Head.SHA}, nil
	}
	// shortcut: 100 check runs to a page, at most 10 pages.
	type checkRun struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"` // queued, in_progress, completed, ...
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
		Output     struct {
			Title   string `json:"title"`
			Summary string `json:"summary"`
			Text    string `json:"text"`
		} `json:"output"`
		App struct {
			Slug string `json:"slug"`
		} `json:"app"`
	}
	var runs []checkRun
	commit := g.repo + "/commits/" + url.PathEscape(pr.Head.SHA)
	for page := 1; page <= 10; page++ {
		var checks struct {
			CheckRuns []checkRun `json:"check_runs"`
		}
		u := commit + "/check-runs?per_page=100&page=" + strconv.Itoa(page)
		if err := g.do(ctx, http.MethodGet, u, nil, &checks); err != nil {
			return PullState{}, err
		}
		runs = append(runs, checks.CheckRuns...)
		if len(checks.CheckRuns) < 100 {
			break
		}
	}
	s := PullState{State: MergePending, Head: pr.Head.SHA}
	for _, c := range runs {
		if c.Status != "completed" {
			s.Running = true
		}
		if c.Conclusion == "failure" || c.Conclusion == "timed_out" || c.Conclusion == "cancelled" {
			s.State = MergeFailed
			check := Check{Name: c.Name, Description: cmp.Or(c.Output.Title, c.Output.Summary), URL: c.HTMLURL}
			if c.App.Slug == "github-actions" { // an Actions check run's ID is its job's
				check.job = strconv.FormatInt(c.ID, 10)
			}
			output := []string{strings.TrimSpace(c.Output.Title), strings.TrimSpace(c.Output.Summary), strings.TrimSpace(c.Output.Text)}
			check.text = strings.Join(slices.DeleteFunc(output, func(s string) bool { return s == "" }), "\n\n")
			s.Failed = append(s.Failed, check)
		}
	}
	var st struct {
		Statuses []struct {
			State       string `json:"state"`
			Context     string `json:"context"`
			Description string `json:"description"` // may be null
			TargetURL   string `json:"target_url"`  // may be null
		} `json:"statuses"`
	}
	if err := g.do(ctx, http.MethodGet, commit+"/status?per_page=100", nil, &st); err != nil {
		return PullState{}, err
	}
	for _, c := range st.Statuses { // the top-level state is ignored: pending when the commit has no statuses
		switch c.State {
		case "pending":
			s.Running = true
		case "failure", "error":
			s.State = MergeFailed
			s.Failed = append(s.Failed, Check{Name: c.Context, Description: c.Description, URL: c.TargetURL})
		}
	}
	return s, nil
}

// CheckLog reads the log of a GitHub Actions job. GitHub answers with a
// redirect to the text elsewhere, which the client follows without the
// token. It needs "Actions: read".
func (g *GitHub) CheckLog(ctx context.Context, c Check) (string, error) {
	if c.job == "" {
		return "", nil
	}
	h := g.auth()
	h["Accept"] = "application/vnd.github+json" // as GitHub's docs ask; the text it redirects to is plain
	return logTail(ctx, g.HTTP, "GitHub", g.repo+"/actions/jobs/"+c.job+"/logs", h)
}

// pulls is the list of the branch's pull requests in state, by head
// owner:branch, which only matches branches in the repository itself.
func (g *GitHub) pulls(state, branch string) string {
	return g.repo + "/pulls?state=" + state + "&head=" + url.QueryEscape(g.owner+":"+branch)
}

// do sends one API request with in as its JSON body, when not nil, and
// decodes a 2xx answer into out.
func (g *GitHub) do(ctx context.Context, method, u string, in, out any) error {
	h := g.auth()
	h["Accept"] = "application/vnd.github+json"
	return do(ctx, g.HTTP, "GitHub", method, u, h, githubMessage, in, out)
}

// auth is the headers that authenticate a request.
func (g *GitHub) auth() map[string]string {
	return map[string]string{"Authorization": "Bearer " + g.token, "X-GitHub-Api-Version": "2022-11-28"}
}

// githubMessage is the message of an error answer, GitHub's or Forgejo's: its JSON message, else
// the start of its text.
func githubMessage(data []byte) string {
	var r struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &r) == nil && r.Message != "" {
		return r.Message
	}
	return textStart(string(data))
}
