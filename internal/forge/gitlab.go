package forge

import (
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
	// A merge request from a fork has its own source project; its branch
	// can have the same name as the runner's (CLR-237).
	SourceProjectID int `json:"source_project_id"`
	TargetProjectID int `json:"target_project_id"`
}

// own reports whether mr comes from the project itself, not from a fork.
func (mr gitlabMR) own() bool { return mr.SourceProjectID == mr.TargetProjectID }

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
		if mr.SourceBranch == branch && mr.own() {
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

func (g *GitLab) MergeState(ctx context.Context, branch string) (PullState, error) {
	var mrs []gitlabMR
	if err := g.do(ctx, http.MethodGet, g.project+"/merge_requests?source_branch="+url.QueryEscape(branch)+"&state=all", nil, &mrs); err != nil {
		return PullState{}, err
	}
	// The newest of the project's own: GitLab lists by creation, newest first.
	i := slices.IndexFunc(mrs, gitlabMR.own)
	if i < 0 {
		return PullState{}, nil
	}
	mr := mrs[i]
	switch mr.State {
	case "merged":
		return PullState{State: MergeMerged}, nil
	case "closed":
		return PullState{State: MergeClosed}, nil
	}
	// The list leaves out head_pipeline; only the single merge request carries it.
	var one struct {
		SHA          string `json:"sha"` // the head; a merged results pipeline runs on another commit
		HeadPipeline *struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"head_pipeline"`
	}
	if err := g.do(ctx, http.MethodGet, g.project+"/merge_requests/"+strconv.Itoa(mr.IID), nil, &one); err != nil {
		return PullState{}, err
	}
	s := PullState{State: MergePending, Head: one.SHA}
	p := one.HeadPipeline
	if p == nil || (p.Status != "failed" && p.Status != "canceled") {
		return s, nil
	}
	// A failed or canceled pipeline has finished, so nothing is running.
	// shortcut: this list is read on every pass of the Review upkeep while
	// the pipeline stays failed, one request; the logs are read once per head.
	s.State = MergeFailed
	var jobs []struct {
		ID            int64  `json:"id"`
		Name          string `json:"name"`
		FailureReason string `json:"failure_reason"`
		WebURL        string `json:"web_url"`
		AllowFailure  bool   `json:"allow_failure"`
	}
	// Canceled jobs too: a canceled pipeline counts as failed, as a cancelled check run does on GitHub.
	if err := g.do(ctx, http.MethodGet, g.project+"/pipelines/"+strconv.FormatInt(p.ID, 10)+"/jobs?scope%5B%5D=failed&scope%5B%5D=canceled&per_page=100", nil, &jobs); err != nil {
		return PullState{}, err
	}
	for _, j := range jobs {
		if !j.AllowFailure { // an allowed failure does not fail the pipeline
			s.Failed = append(s.Failed, Check{Name: j.Name, Description: j.FailureReason, URL: j.WebURL, job: strconv.FormatInt(j.ID, 10)})
		}
	}
	return s, nil
}

// CheckLog reads the job's trace, which the token's api scope covers.
func (g *GitLab) CheckLog(ctx context.Context, c Check) (string, error) {
	if c.job == "" {
		return "", nil
	}
	return logTail(ctx, g.HTTP, "GitLab", g.project+"/jobs/"+c.job+"/trace", map[string]string{"PRIVATE-TOKEN": g.token})
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
