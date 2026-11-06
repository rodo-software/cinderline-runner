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

var _ Forge = (*Forgejo)(nil)

// Forgejo is the Forge of a repository on Forgejo or Gitea, which share
// this API. Its token stays in this process: it is sent only to the
// repository's own API host and never put in an error or a log line.
type Forgejo struct {
	HTTP *http.Client // set by NewForgejo; replace it before first use, e.g. in tests

	repo  string // REST base of the repository: https://<host>/api/v1/repos/<owner>/<repo>
	token string
	sleep func(context.Context, time.Duration) error // between merge attempts; replaced in tests
}

// NewForgejo returns the Forge of repository, given as https://host/o/r,
// ssh://git@host[:port]/o/r or git@host:o/r, with or without .git. Its API
// is https://<host>/api/v1 on any host. An https port is kept for the API,
// an ssh one dropped, as legacy parseRepository.
func NewForgejo(repository, token string) (*Forgejo, error) {
	s := repository
	if host, path, ok := strings.Cut(s, ":"); ok && strings.HasPrefix(s, "git@") && !strings.Contains(s, "://") {
		s = "ssh://" + host + "/" + path // scp form: git@host:o/r
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "ssh") || u.Hostname() == "" {
		return nil, fmt.Errorf("unsupported repository URL %q", repository)
	}
	owner, repo, ok := strings.Cut(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), "/")
	if !ok || strings.Trim(owner, ".") == "" || strings.Trim(repo, ".") == "" || strings.Contains(repo, "/") {
		return nil, fmt.Errorf("repository URL %q does not name a Forgejo owner/repository", repository)
	}
	host := strings.ToLower(u.Hostname())
	if p := u.Port(); u.Scheme == "https" && p != "" && p != "443" {
		host += ":" + p
	}
	return &Forgejo{
		HTTP:  &http.Client{Timeout: 30 * time.Second},
		repo:  "https://" + host + "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo),
		token: token,
		sleep: sleep,
	}, nil
}

// forgejoPull is the part of a Forgejo pull request the runner reads.
type forgejoPull struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

// forgejoError is a non-2xx answer.
type forgejoError struct {
	method, url string
	status      int
	message     string
}

func (e *forgejoError) Error() string {
	return fmt.Sprintf("Forgejo answered %s %s with %d: %s", e.method, e.url, e.status, e.message)
}

func (f *Forgejo) DefaultBranch(ctx context.Context) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	err := f.do(ctx, http.MethodGet, f.repo, nil, &r)
	return r.DefaultBranch, err
}

func (f *Forgejo) OpenPullRequest(ctx context.Context, in PullRequestInput) (PullRequest, error) {
	var pr forgejoPull
	err := f.do(ctx, http.MethodPost, f.repo+"/pulls", map[string]string{
		"head": in.Branch, "base": in.Base, "title": in.Title, "body": in.Body,
	}, &pr)
	// Forgejo answers 409 when the branch already has an open pull request.
	var e *forgejoError
	if errors.As(err, &e) && e.status == http.StatusConflict {
		if found, ok, ferr := f.FindPullRequest(ctx, in.Branch); ferr == nil && ok {
			return found, nil
		}
	}
	if err != nil {
		return PullRequest{}, err
	}
	return PullRequest{Number: pr.Number, URL: pr.HTMLURL}, nil
}

func (f *Forgejo) FindPullRequest(ctx context.Context, branch string) (PullRequest, bool, error) {
	// shortcut: the 50 newest open pull requests only, as legacy; page if a repository has more open at once.
	pr, err := f.find(ctx, "?state=open&limit=50", branch)
	return PullRequest{Number: pr.Number, URL: pr.HTMLURL}, pr.Head.Ref != "", err
}

// RequestMerge asks Forgejo to squash-merge the pull request and delete
// its branch once its checks succeed. Forgejo answers 405 while it is still
// checking a new pull request's mergeability, so a 405 is tried again every
// 5 seconds, 12 times in all, as legacy; other refusals are not.
func (f *Forgejo) RequestMerge(ctx context.Context, pr PullRequest) error {
	for attempt := 1; ; attempt++ {
		err := f.do(ctx, http.MethodPost, f.repo+"/pulls/"+strconv.Itoa(pr.Number)+"/merge", map[string]any{
			"Do":                        "squash",
			"merge_when_checks_succeed": true,
			"delete_branch_after_merge": true,
		}, nil)
		var e *forgejoError
		if !errors.As(err, &e) || e.status != http.StatusMethodNotAllowed || attempt == 12 {
			return err
		}
		if err := f.sleep(ctx, 5*time.Second); err != nil {
			return err
		}
	}
}

func (f *Forgejo) MergeState(ctx context.Context, branch string) (MergeState, error) {
	// shortcut: the 50 most recently updated pull requests only, as legacy; page if a repository has more at once.
	pr, err := f.find(ctx, "?state=all&sort=recentupdate&limit=50", branch)
	switch {
	case err != nil:
		return "", err
	case pr.Head.Ref == "": // none found
		return "", nil
	case pr.Merged:
		return MergeMerged, nil
	case pr.State == "closed":
		return MergeClosed, nil
	}
	var status struct {
		State string `json:"state"`
	}
	if err := f.do(ctx, http.MethodGet, f.repo+"/commits/"+url.PathEscape(pr.Head.SHA)+"/status", nil, &status); err != nil {
		return "", err
	}
	if status.State == "failure" || status.State == "error" {
		return MergeFailed, nil
	}
	return MergePending, nil
}

// find is the first pull request of branch in the list query names,
// matched by head ref as legacy, or the zero pull request (empty Head.Ref).
func (f *Forgejo) find(ctx context.Context, query, branch string) (forgejoPull, error) {
	var prs []forgejoPull
	if err := f.do(ctx, http.MethodGet, f.repo+"/pulls"+query, nil, &prs); err != nil {
		return forgejoPull{}, err
	}
	for _, pr := range prs {
		if pr.Head.Ref == branch {
			return pr, nil
		}
	}
	return forgejoPull{}, nil
}

// do sends one API request with in as its JSON body, when not nil, and
// decodes a 2xx answer into out, when not nil.
func (f *Forgejo) do(ctx context.Context, method, u string, in, out any) error {
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
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "token "+f.token)
	req.Header.Set("User-Agent", name.Name+"-runner")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &forgejoError{method, u, resp.StatusCode, githubMessage(data)} // same error shape: {"message": "..."}
	}
	if out == nil {
		return nil // the merge answers an empty 200
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("Forgejo answered %s %s with %d and unreadable JSON: %w", method, u, resp.StatusCode, err)
	}
	return nil
}

// sleep waits d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
