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
	"strings"
	"time"

	"github.com/rodo-software/cinderstack-runner/internal/name"
)

var _ Forge = (*Bitbucket)(nil)

// Bitbucket is the Forge of a repository on Bitbucket Cloud; Data Center
// is not supported (decision 2026-11-04-bitbucket-cloud). Its token stays
// in this process: it is sent only to api.bitbucket.org and never put in an
// error or a log line.
type Bitbucket struct {
	HTTP *http.Client // set by NewBitbucket; replace it before first use, e.g. in tests

	repo      string // REST base of the repository: https://api.bitbucket.org/2.0/repositories/<workspace>/<repo>
	workspace string // workspace slug
	token     string
}

// NewBitbucket returns the Forge of repository, given as
// https://bitbucket.org/w/r, ssh://git@bitbucket.org/w/r or
// git@bitbucket.org:w/r, with or without .git; w is the workspace. Any
// other host is refused: Bitbucket Cloud is bitbucket.org only.
func NewBitbucket(repository, token string) (*Bitbucket, error) {
	s := repository
	if host, path, ok := strings.Cut(s, ":"); ok && strings.HasPrefix(s, "git@") && !strings.Contains(s, "://") {
		s = "ssh://" + host + "/" + path // scp form: git@host:w/r
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "ssh") || !strings.EqualFold(u.Hostname(), "bitbucket.org") ||
		(u.Scheme == "https" && u.Port() != "" && u.Port() != "443") {
		return nil, fmt.Errorf("repository URL %q is not on Bitbucket Cloud (bitbucket.org)", repository)
	}
	workspace, repo, ok := strings.Cut(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), "/")
	if !ok || strings.Trim(workspace, ".") == "" || strings.Trim(repo, ".") == "" || strings.Contains(repo, "/") {
		return nil, fmt.Errorf("repository URL %q does not name a Bitbucket workspace/repository", repository)
	}
	return &Bitbucket{
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		repo:      "https://api.bitbucket.org/2.0/repositories/" + url.PathEscape(workspace) + "/" + url.PathEscape(repo),
		workspace: workspace,
		token:     token,
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
	} `json:"source"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

func (p bitbucketPull) pullRequest() PullRequest {
	return PullRequest{Number: p.ID, URL: p.Links.HTML.Href}
}

// bitbucketError is a non-2xx answer.
type bitbucketError struct {
	method, url string
	status      int
	message     string
}

func (e *bitbucketError) Error() string {
	return fmt.Sprintf("Bitbucket answered %s %s with %d: %s", e.method, e.url, e.status, e.message)
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
	var e *bitbucketError
	if errors.As(err, &e) && e.status == http.StatusBadRequest {
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

func (b *Bitbucket) MergeState(ctx context.Context, branch string) (MergeState, error) {
	pr, err := b.find(ctx, "?q="+sourceBranch(branch)+"&state=OPEN&state=MERGED&state=DECLINED&state=SUPERSEDED&sort=-created_on", branch)
	switch {
	case err != nil:
		return "", err
	case pr.Source.Branch.Name == "": // none found
		return "", nil
	case pr.State == "MERGED":
		return MergeMerged, nil
	case pr.State == "DECLINED" || pr.State == "SUPERSEDED":
		return MergeClosed, nil
	}
	var statuses struct {
		Values []struct {
			State string `json:"state"` // SUCCESSFUL, FAILED, INPROGRESS or STOPPED
		} `json:"values"`
	}
	// shortcut: the first page (10) of build statuses only; page if a head commit gets more builds.
	if err := b.do(ctx, http.MethodGet, b.repo+"/commit/"+url.PathEscape(pr.Source.Commit.Hash)+"/statuses", nil, &statuses); err != nil {
		return "", err
	}
	for _, s := range statuses.Values {
		if s.State == "FAILED" || s.State == "STOPPED" {
			return MergeFailed, nil
		}
	}
	return MergePending, nil
}

// sourceBranch is the escaped query that filters pull requests by source branch.
func sourceBranch(branch string) string {
	return url.QueryEscape(`source.branch.name="` + branch + `"`)
}

// find is the first pull request of branch on the first page of the list
// query names, matched by source branch as legacy, or the zero pull request
// (empty Source.Branch.Name).
func (b *Bitbucket) find(ctx context.Context, query, branch string) (bitbucketPull, error) {
	var page struct {
		Values []bitbucketPull `json:"values"`
	}
	if err := b.do(ctx, http.MethodGet, b.repo+"/pullrequests"+query, nil, &page); err != nil {
		return bitbucketPull{}, err
	}
	for _, pr := range page.Values {
		if pr.Source.Branch.Name == branch {
			return pr, nil
		}
	}
	return bitbucketPull{}, nil
}

func (b *Bitbucket) CISecrets(ctx context.Context) ([]string, error) {
	var exposed []string
	var deploymentEnvs []string

	var repoVars struct {
		Values []struct {
			Key string `json:"key"`
		} `json:"values"`
	}
	err := b.do(ctx, http.MethodGet, b.repo+"/pipelines_config/variables?pagelen=100", nil, &repoVars)
	if err != nil {
		return nil, err
	}
	for _, v := range repoVars.Values {
		exposed = append(exposed, "repository variable "+v.Key)
	}

	var wsVars struct {
		Values []struct {
			Key string `json:"key"`
		} `json:"values"`
	}
	err = b.do(ctx, http.MethodGet, "https://api.bitbucket.org/2.0/workspaces/"+url.PathEscape(b.workspace)+"/pipelines-config/variables?pagelen=100", nil, &wsVars)
	if err != nil {
		return nil, err
	}
	for _, v := range wsVars.Values {
		exposed = append(exposed, "workspace variable "+v.Key)
	}

	var envs struct {
		Values []struct {
			UUID string `json:"uuid"`
			Name string `json:"name"`
		} `json:"values"`
	}
	err = b.do(ctx, http.MethodGet, b.repo+"/environments?pagelen=100", nil, &envs)
	if err != nil {
		return nil, err
	}
	for _, env := range envs.Values {
		var envVars struct {
			Values []struct {
				Key string `json:"key"`
			} `json:"values"`
		}
		err = b.do(ctx, http.MethodGet, b.repo+"/deployments_config/environments/"+url.PathEscape(env.UUID)+"/variables?pagelen=100", nil, &envVars)
		if err != nil {
			return nil, err
		}
		if len(envVars.Values) > 0 {
			deploymentEnvs = append(deploymentEnvs, env.Name)
		}
	}

	if len(exposed) > 0 {
		return exposed, nil
	}
	if len(deploymentEnvs) > 0 {
		return nil, fmt.Errorf("deployment environment %s has variables; whether only protected branches can deploy to it cannot be read through the API",
			strings.Join(deploymentEnvs, ", "))
	}
	// shortcut: first page only (100 entries), upgrade when a repository has more
	return nil, nil
}

// do sends one API request with in as its JSON body, when not nil, and
// decodes a 2xx answer into out.
func (b *Bitbucket) do(ctx context.Context, method, u string, in, out any) error {
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
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("User-Agent", name.Name+"-runner")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &bitbucketError{method, u, resp.StatusCode, bitbucketMessage(data)}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("Bitbucket answered %s %s with %d and unreadable JSON: %w", method, u, resp.StatusCode, err)
	}
	return nil
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
	s := strings.TrimSpace(string(data))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
