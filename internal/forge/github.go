package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rodo-software/cinderstack-runner/internal/name"
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
// ssh://git@host[:port]/o/r or git@host:o/r, with or without .git. On
// github.com the API is api.github.com; on any other host it is GitHub
// Enterprise Server's https://<host>/api/v3, with GraphQL beside it at
// /api/graphql. An https port is kept for the API, an ssh one dropped, as
// legacy parseRepository.
func NewGitHub(repository, token string) (*GitHub, error) {
	s := repository
	if host, path, ok := strings.Cut(s, ":"); ok && strings.HasPrefix(s, "git@") && !strings.Contains(s, "://") {
		s = "ssh://" + host + "/" + path // scp form: git@host:o/r
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "ssh") || u.Hostname() == "" {
		return nil, fmt.Errorf("unsupported repository URL %q", repository)
	}
	owner, repo, ok := strings.Cut(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return nil, fmt.Errorf("repository URL %q does not name a GitHub owner/repository", repository)
	}
	host := strings.ToLower(u.Hostname())
	if p := u.Port(); u.Scheme == "https" && p != "" && p != "443" {
		host += ":" + p
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

// githubError is a non-2xx answer, or GraphQL errors in a 200.
type githubError struct {
	method, url string
	status      int
	message     string
}

func (e *githubError) Error() string {
	return fmt.Sprintf("GitHub answered %s %s with %d: %s", e.method, e.url, e.status, e.message)
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
	var e *githubError
	if errors.As(err, &e) && e.status == http.StatusUnprocessableEntity {
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
		return &githubError{http.MethodPost, g.graphql, http.StatusOK, strings.Join(msgs, "; ")}
	}
	return nil
}

func (g *GitHub) MergeState(ctx context.Context, branch string) (MergeState, error) {
	var prs []githubPull
	if err := g.do(ctx, http.MethodGet, g.pulls("all", branch), nil, &prs); err != nil {
		return "", err
	}
	if len(prs) == 0 {
		return "", nil
	}
	pr := prs[0] // the newest: GitHub lists by creation, newest first
	switch {
	case pr.MergedAt != "":
		return MergeMerged, nil
	case pr.State == "closed":
		return MergeClosed, nil
	}
	// shortcut: the first 30 check runs only, GitHub's default page; page if a repository runs more on one commit.
	var checks struct {
		CheckRuns []struct {
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := g.do(ctx, http.MethodGet, g.repo+"/commits/"+url.PathEscape(pr.Head.SHA)+"/check-runs", nil, &checks); err != nil {
		return "", err
	}
	for _, c := range checks.CheckRuns {
		if c.Conclusion == "failure" || c.Conclusion == "timed_out" || c.Conclusion == "cancelled" {
			return MergeFailed, nil
		}
	}
	return MergePending, nil
}

// pulls is the list of the branch's pull requests in state, by head
// owner:branch, which only matches branches in the repository itself.
func (g *GitHub) pulls(state, branch string) string {
	return g.repo + "/pulls?state=" + state + "&head=" + url.QueryEscape(g.owner+":"+branch)
}

// do sends one API request with in as its JSON body, when not nil, and
// decodes a 2xx answer into out.
func (g *GitHub) do(ctx context.Context, method, u string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("User-Agent", name.Name+"-runner")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &githubError{method, u, resp.StatusCode, githubMessage(data)}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("GitHub answered %s %s with %d and unreadable JSON: %w", method, u, resp.StatusCode, err)
	}
	return nil
}

// githubMessage is the message of an error answer: its JSON message, else
// the start of its text.
func githubMessage(data []byte) string {
	var r struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &r) == nil && r.Message != "" {
		return r.Message
	}
	s := strings.TrimSpace(string(data))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
