package forge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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
// with or without .git. Its API is https://<host>/api/v1 on any host, with
// the repository's port.
func NewForgejo(repository, token string) (*Forgejo, error) {
	host, path, err := parseRepository(repository)
	if err != nil {
		return nil, err
	}
	owner, repo, ok := strings.Cut(path, "/")
	if !ok || strings.Trim(owner, ".") == "" || strings.Trim(repo, ".") == "" || strings.Contains(repo, "/") {
		return nil, fmt.Errorf("repository URL %q does not name a Forgejo owner/repository", repository)
	}
	return &Forgejo{
		HTTP:  &http.Client{Timeout: 30 * time.Second},
		repo:  "https://" + host + "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo),
		token: token,
		sleep: Sleep,
	}, nil
}

// forgejoPull is the part of a Forgejo pull request the runner reads.
type forgejoPull struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	Head    struct {
		Ref   string `json:"ref"`
		SHA   string `json:"sha"`
		Label string `json:"label"`
	} `json:"head"`
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
	if status(err) == http.StatusConflict {
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
	pr, err := f.find(ctx, "?state=open&sort=recentupdate&limit="+strconv.Itoa(forgejoPageSize), branch)
	return PullRequest{Number: pr.Number, URL: pr.HTMLURL}, pr.Head.Label != "", err
}

// RequestMerge asks Forgejo to squash-merge the pull request and delete
// its branch once its checks succeed. Forgejo answers 405 while it is still
// checking a new pull request's mergeability, so a 405 is tried again every
// 5 seconds, 12 times in all; other refusals are not.
func (f *Forgejo) RequestMerge(ctx context.Context, pr PullRequest) error {
	for attempt := 1; ; attempt++ {
		err := f.do(ctx, http.MethodPost, f.repo+"/pulls/"+strconv.Itoa(pr.Number)+"/merge", map[string]any{
			"Do":                        "squash",
			"merge_when_checks_succeed": true,
			"delete_branch_after_merge": true,
		}, nil)
		if status(err) != http.StatusMethodNotAllowed || attempt == 12 {
			return err
		}
		if err := f.sleep(ctx, 5*time.Second); err != nil {
			return err
		}
	}
}

func (f *Forgejo) MergeState(ctx context.Context, branch string) (MergeState, error) {
	pr, err := f.find(ctx, "?state=all&sort=recentupdate&limit="+strconv.Itoa(forgejoPageSize), branch)
	switch {
	case err != nil:
		return "", err
	case pr.Head.Label == "": // none found
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

// forgejoPageSize is the page size used when listing pull requests, and
// forgejoPages the most pages find reads.
const (
	forgejoPageSize = 50
	forgejoPages    = 3
)

// find is the first pull request of branch in the list query names,
// matched by head.label, which survives branch deletion (decision
// 2026-11-07-publish-resilience), or the zero pull request (empty Head.Label).
// Forgejo's and Gitea's GET pulls/{base}/{head} returns any one of the pull
// requests of a branch, not the newest, and only Forgejo filters the list
// by head, so the list it is.
//
// shortcut: the forgejoPages pages of the most recently updated pull
// requests only, as this runs per Review issue every review_after; a pull
// request of the branch updated before 150 others is not found. Use the
// list's head filter once Gitea has one.
func (f *Forgejo) find(ctx context.Context, query, branch string) (forgejoPull, error) {
	for page := 1; page <= forgejoPages; page++ {
		var u string
		if page == 1 {
			u = f.repo + "/pulls" + query
		} else {
			u = f.repo + "/pulls" + query + "&page=" + strconv.Itoa(page)
		}
		var prs []forgejoPull
		if err := f.do(ctx, http.MethodGet, u, nil, &prs); err != nil {
			return forgejoPull{}, err
		}
		for _, pr := range prs {
			if pr.Head.Label == branch {
				return pr, nil
			}
		}
		if len(prs) < forgejoPageSize {
			break
		}
	}
	return forgejoPull{}, nil
}

// do sends one API request with in as its JSON body, when not nil, and
// decodes a 2xx answer into out, when not nil.
func (f *Forgejo) do(ctx context.Context, method, u string, in, out any) error {
	return do(ctx, f.HTTP, "Forgejo", method, u, map[string]string{
		"Accept":        "application/json",
		"Authorization": "token " + f.token,
	}, githubMessage, in, out) // same error shape as GitHub: {"message": "..."}
}
