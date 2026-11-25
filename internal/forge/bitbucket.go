package forge

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var _ Forge = (*Bitbucket)(nil)

// Bitbucket is the Forge of a repository on Bitbucket Cloud; Data Center
// is not supported (decision 2026-11-04-bitbucket-cloud). Its token stays
// in this process: it is sent only to api.bitbucket.org and never put in an
// error or a log line.
type Bitbucket struct {
	HTTP *http.Client // set by NewBitbucket; replace it before first use, e.g. in tests

	repo  string // REST base of the repository: https://api.bitbucket.org/2.0/repositories/<workspace>/<repo>
	token string
}

// NewBitbucket returns the Forge of repository, given as
// https://bitbucket.org/w/r, with or without .git; w is the workspace. Any
// other host is refused: Bitbucket Cloud is bitbucket.org only.
func NewBitbucket(repository, token string) (*Bitbucket, error) {
	host, path, err := parseRepository(repository)
	if err != nil || host != "bitbucket.org" { // also refuses an https port
		return nil, fmt.Errorf("repository URL %q is not on Bitbucket Cloud (bitbucket.org)", repository)
	}
	workspace, repo, ok := strings.Cut(path, "/")
	if !ok || strings.Trim(workspace, ".") == "" || strings.Trim(repo, ".") == "" || strings.Contains(repo, "/") {
		return nil, fmt.Errorf("repository URL %q does not name a Bitbucket workspace/repository", repository)
	}
	return &Bitbucket{
		HTTP:  &http.Client{Timeout: 30 * time.Second},
		repo:  "https://api.bitbucket.org/2.0/repositories/" + url.PathEscape(workspace) + "/" + url.PathEscape(repo),
		token: token,
	}, nil
}

// bitbucketPull is the part of a Bitbucket pull request the runner reads.
type bitbucketPull struct {
	ID     int    `json:"id"`
	State  string `json:"state"` // OPEN, MERGED, DECLINED or SUPERSEDED
	Source struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
		Commit struct {
			Hash string `json:"hash"`
		} `json:"commit"`
		Repository bitbucketRepository `json:"repository"`
	} `json:"source"`
	// A pull request from a fork has its own source repository; its branch
	// can have the same name as the runner's (CLR-237).
	Destination struct {
		Repository bitbucketRepository `json:"repository"`
	} `json:"destination"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

type bitbucketRepository struct {
	FullName string `json:"full_name"` // workspace/repository
}

func (p bitbucketPull) pullRequest() PullRequest {
	return PullRequest{Number: p.ID, URL: p.Links.HTML.Href}
}

func (b *Bitbucket) DefaultBranch(ctx context.Context) (string, error) {
	var r struct {
		MainBranch struct {
			Name string `json:"name"`
		} `json:"mainbranch"`
	}
	err := b.do(ctx, http.MethodGet, b.repo, nil, &r)
	return r.MainBranch.Name, err
}

func (b *Bitbucket) OpenPullRequest(ctx context.Context, in PullRequestInput) (PullRequest, error) {
	var pr bitbucketPull
	err := b.do(ctx, http.MethodPost, b.repo+"/pullrequests", map[string]any{
		"title":               in.Title,
		"description":         in.Body,
		"source":              map[string]any{"branch": map[string]string{"name": in.Branch}},
		"destination":         map[string]any{"branch": map[string]string{"name": in.Base}},
		"close_source_branch": true,
	}, &pr)
	// Bitbucket answers a duplicate with a plain 400, so any 400 looks for the open one before failing.
	if status(err) == http.StatusBadRequest {
		if found, ok, ferr := b.FindPullRequest(ctx, in.Branch); ferr == nil && ok {
			return found, nil
		}
	}
	if err != nil {
		return PullRequest{}, err
	}
	return pr.pullRequest(), nil
}

func (b *Bitbucket) FindPullRequest(ctx context.Context, branch string) (PullRequest, bool, error) {
	pr, err := b.find(ctx, "?state=OPEN&q="+sourceBranch(branch), branch)
	return pr.pullRequest(), pr.Source.Branch.Name != "", err
}

// RequestMerge always fails without calling the API: Bitbucket Cloud's API
// has no merge-when-builds-pass and its /merge merges at once, before CI
// (decisions 2026-11-04-bitbucket-cloud, 0110), so a person merges.
func (b *Bitbucket) RequestMerge(context.Context, PullRequest) error {
	return errors.New("Bitbucket cannot merge a pull request when its checks pass; merge it yourself")
}

func (b *Bitbucket) MergeState(ctx context.Context, branch string) (PullState, error) {
	pr, err := b.find(ctx, "?q="+sourceBranch(branch)+"&state=OPEN&state=MERGED&state=DECLINED&state=SUPERSEDED&sort=-created_on", branch)
	head := pr.Source.Commit.Hash // short: Bitbucket names the first 12 characters
	switch {
	case err != nil:
		return PullState{}, err
	case pr.Source.Branch.Name == "": // none found
		return PullState{}, nil
	case pr.State == "MERGED":
		return PullState{State: MergeMerged, Head: head}, nil
	case pr.State == "DECLINED" || pr.State == "SUPERSEDED":
		return PullState{State: MergeClosed, Head: head}, nil
	}
	type status struct {
		State       string `json:"state"` // SUCCESSFUL, FAILED, INPROGRESS or STOPPED
		Key         string `json:"key"`
		Name        string `json:"name"`
		Description string `json:"description"`
		URL         string `json:"url"`
	}
	type page struct {
		Values []status `json:"values"`
		Next   string   `json:"next"`
	}
	// shortcut: 100 build statuses to a page, at most 10 pages.
	u := b.repo + "/commit/" + url.PathEscape(head) + "/statuses?pagelen=100"
	s := PullState{State: MergePending, Head: head}
	for n := 1; u != "" && n <= 10; n++ {
		var p page
		if err := b.do(ctx, http.MethodGet, u, nil, &p); err != nil {
			return PullState{}, err
		}
		for _, st := range p.Values {
			switch st.State {
			case "INPROGRESS":
				s.Running = true
			case "FAILED", "STOPPED":
				s.State = MergeFailed
				s.Failed = append(s.Failed, Check{Name: cmp.Or(st.Name, st.Key), Description: st.Description, URL: st.URL, job: buildNumber(st.URL)})
			}
		}
		// A next link that leaves the repository's API is not followed: the token never goes elsewhere.
		u = ""
		if n := p.Next; strings.HasPrefix(n, b.repo+"/") {
			u = n
		}
	}
	return s, nil
}

// pipelinesResults is a Bitbucket Pipelines results page, its build number
// captured.
var pipelinesResults = regexp.MustCompile(`^https://bitbucket\.org/[^/]+/[^/]+/(?:pipelines/results|addon/pipelines/home#!/results)/(\d+)(?:[/?#]|$)`)

// buildNumber is the number of a Bitbucket Pipelines results page, else "".
func buildNumber(u string) string {
	if m := pipelinesResults.FindStringSubmatch(u); m != nil {
		return m[1]
	}
	return ""
}

// CheckLog is the end of the log of the first failed step of the
// Bitbucket Pipelines build whose number the check's job is; a build of
// another CI, or one without a failed step, has none.
func (b *Bitbucket) CheckLog(ctx context.Context, c Check) (string, error) {
	if c.job == "" {
		return "", nil
	}
	var pipeline struct {
		UUID string `json:"uuid"`
	}
	if err := b.do(ctx, http.MethodGet, b.repo+"/pipelines/"+c.job, nil, &pipeline); err != nil {
		return "", err
	}
	base := b.repo + "/pipelines/" + url.PathEscape(pipeline.UUID)
	var steps struct {
		Values []struct {
			UUID  string `json:"uuid"`
			State struct {
				Result struct {
					Name string `json:"name"`
				} `json:"result"`
			} `json:"state"`
		} `json:"values"`
	}
	if err := b.do(ctx, http.MethodGet, base+"/steps?pagelen=100", nil, &steps); err != nil {
		return "", err
	}
	for _, st := range steps.Values {
		if st.State.Result.Name == "FAILED" || st.State.Result.Name == "ERROR" {
			return logTailMessage(ctx, b.HTTP, "Bitbucket", base+"/steps/"+url.PathEscape(st.UUID)+"/log", map[string]string{"Authorization": "Bearer " + b.token}, bitbucketMessage)
		}
	}
	return "", nil // no failed step: the check's text is sent instead
}

// sourceBranch is the escaped query that filters pull requests by source branch.
func sourceBranch(branch string) string {
	return url.QueryEscape(`source.branch.name="` + branch + `"`)
}

// find is the first pull request of branch on the first page of the list
// query names, matched by source branch and coming from the repository
// itself, not a fork, or the zero pull request (empty Source.Branch.Name).
func (b *Bitbucket) find(ctx context.Context, query, branch string) (bitbucketPull, error) {
	var page struct {
		Values []bitbucketPull `json:"values"`
	}
	if err := b.do(ctx, http.MethodGet, b.repo+"/pullrequests"+query, nil, &page); err != nil {
		return bitbucketPull{}, err
	}
	for _, pr := range page.Values {
		if pr.Source.Branch.Name == branch && pr.Source.Repository.FullName == pr.Destination.Repository.FullName {
			return pr, nil
		}
	}
	return bitbucketPull{}, nil
}

// do sends one API request with in as its JSON body, when not nil, and
// decodes a 2xx answer into out.
func (b *Bitbucket) do(ctx context.Context, method, u string, in, out any) error {
	return do(ctx, b.HTTP, "Bitbucket", method, u, map[string]string{
		"Accept":        "application/json",
		"Authorization": "Bearer " + b.token,
	}, bitbucketMessage, in, out)
}

// bitbucketMessage is the message of an error answer: its JSON
// error.message, else the start of its text.
func bitbucketMessage(data []byte) string {
	var r struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &r) == nil && r.Error.Message != "" {
		return r.Error.Message
	}
	return textStart(string(data))
}
