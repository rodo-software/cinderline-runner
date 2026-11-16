package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var _ Forge = (*GitLab)(nil)

// GitLab is the Forge of a repository on gitlab.com or a self-managed
// GitLab (decision 2026-11-04-gitlab-and-forge-per-project). Its token
// stays in this process: it is sent only to the repository's own API host
// and never put in an error or a log line.
type GitLab struct {
	HTTP *http.Client // set by NewGitLab; replace it before first use, e.g. in tests

	project string // REST base of the project: <host>/api/v4/projects/<group%2F...%2Frepo>
	token   string
}

// NewGitLab returns the Forge of repository, given as https://host/g/r,
// with or without .git, in any depth of subgroups. Its API is
// https://<host>/api/v4 on any host, with the repository's port: the
// project's forge, not the host, says it is GitLab.
func NewGitLab(repository, token string) (*GitLab, error) {
	host, path, err := parseRepository(repository)
	if err != nil {
		return nil, err
	}
	segments := strings.Split(path, "/")
	if len(segments) < 2 {
		return nil, fmt.Errorf("repository URL %q does not name a GitLab group/project", repository)
	}
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return nil, fmt.Errorf("repository URL %q does not name a GitLab group/project", repository)
		}
	}
	return &GitLab{
		HTTP: &http.Client{Timeout: 30 * time.Second},
		// PathEscape also escapes the slashes: GitLab takes the project's full path as one segment.
		project: "https://" + host + "/api/v4/projects/" + url.PathEscape(path),
		token:   token,
	}, nil
}

// gitlabMR is the part of a GitLab merge request the runner reads.
type gitlabMR struct {
	IID          int    `json:"iid"`
	WebURL       string `json:"web_url"`
	State        string `json:"state"`
	SourceBranch string `json:"source_branch"`
}

func (g *GitLab) DefaultBranch(ctx context.Context) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	err := g.do(ctx, http.MethodGet, g.project, nil, &r)
	return r.DefaultBranch, err
}

func (g *GitLab) OpenPullRequest(ctx context.Context, in PullRequestInput) (PullRequest, error) {
	var mr gitlabMR
	err := g.do(ctx, http.MethodPost, g.project+"/merge_requests", map[string]string{
		"source_branch": in.Branch, "target_branch": in.Base, "title": in.Title, "description": in.Body,
	}, &mr)
	// GitLab answers 409 when the source branch already has an open merge request.
	if status(err) == http.StatusConflict {
		if found, ok, ferr := g.FindPullRequest(ctx, in.Branch); ferr == nil && ok {
			return found, nil
		}
	}
	if err != nil {
		return PullRequest{}, err
	}
	return PullRequest{Number: mr.IID, URL: mr.WebURL}, nil
}

func (g *GitLab) FindPullRequest(ctx context.Context, branch string) (PullRequest, bool, error) {
	var mrs []gitlabMR
	if err := g.do(ctx, http.MethodGet, g.project+"/merge_requests?state=opened&source_branch="+url.QueryEscape(branch), nil, &mrs); err != nil {
		return PullRequest{}, false, err
	}
	for _, mr := range mrs {
		if mr.SourceBranch == branch {
			return PullRequest{Number: mr.IID, URL: mr.WebURL}, true, nil
		}
	}
	return PullRequest{}, false, nil
}

// RequestMerge asks GitLab to squash-merge the merge request and delete its
// branch once the pipeline succeeds. A refusal, such as a 405 when it
// cannot be merged, is not retried.
func (g *GitLab) RequestMerge(ctx context.Context, pr PullRequest) error {
	var r json.RawMessage
	return g.do(ctx, http.MethodPut, g.project+"/merge_requests/"+strconv.Itoa(pr.Number)+"/merge", map[string]bool{
		// auto_merge replaced merge_when_pipeline_succeeds in GitLab 17.11; both are sent so an older GitLab does not merge at once.
		"auto_merge":                   true,
		"merge_when_pipeline_succeeds": true,
		"squash":                       true,
		"should_remove_source_branch":  true,
	}, &r)
}

func (g *GitLab) MergeState(ctx context.Context, branch string) (MergeState, error) {
	var mrs []gitlabMR
	if err := g.do(ctx, http.MethodGet, g.project+"/merge_requests?source_branch="+url.QueryEscape(branch)+"&state=all", nil, &mrs); err != nil {
		return "", err
	}
	if len(mrs) == 0 {
		return "", nil
	}
	mr := mrs[0] // the newest: GitLab lists by creation, newest first
	switch mr.State {
	case "merged":
		return MergeMerged, nil
	case "closed":
		return MergeClosed, nil
	}
	// The list leaves out head_pipeline; only the single merge request carries it.
	var one struct {
		HeadPipeline *struct {
			Status string `json:"status"`
		} `json:"head_pipeline"`
	}
	if err := g.do(ctx, http.MethodGet, g.project+"/merge_requests/"+strconv.Itoa(mr.IID), nil, &one); err != nil {
		return "", err
	}
	if p := one.HeadPipeline; p != nil && (p.Status == "failed" || p.Status == "canceled") {
		return MergeFailed, nil
	}
	return MergePending, nil
}

// do sends one API request with in as its JSON body, when not nil, and
// decodes a 2xx answer into out.
func (g *GitLab) do(ctx context.Context, method, u string, in, out any) error {
	return do(ctx, g.HTTP, "GitLab", method, u, map[string]string{
		"Accept":        "application/json",
		"PRIVATE-TOKEN": g.token,
	}, gitlabMessage, in, out)
}

// gitlabMessage is the message of an error answer: its JSON message, which
// GitLab sends as a string, a list or an object, else its OAuth error, else
// the start of its text.
func gitlabMessage(data []byte) string {
	var r struct {
		Message json.RawMessage `json:"message"`
		Error   string          `json:"error"`
	}
	s := string(data)
	if json.Unmarshal(data, &r) == nil {
		var m string
		switch {
		case json.Unmarshal(r.Message, &m) == nil && m != "":
			return m
		case len(r.Message) > 0 && string(r.Message) != "null":
			s = string(r.Message)
		case r.Error != "":
			return r.Error
		}
	}
	return textStart(s)
}
