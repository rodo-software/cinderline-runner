// Package forge defines Forge, the git host of a project's repository
// (pull requests and merges), its implementations GitHub (GitHub and
// GitHub Enterprise), GitLab (gitlab.com and self-managed) and Forgejo
// (Forgejo and Gitea), Bitbucket (Bitbucket Cloud), and Fake, its in-memory
// stand-in for tests.
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

	"github.com/rodo-software/cinderline-runner/internal/name"
)

// Forge talks to the host of one repository.
type Forge interface {
	DefaultBranch(ctx context.Context) (string, error)
	// OpenPullRequest opens a pull request for in.Branch, or returns the open
	// one that already exists for it.
	OpenPullRequest(ctx context.Context, in PullRequestInput) (PullRequest, error)
	// FindPullRequest returns the open pull request of branch; found is
	// false when none is open, so a pull request a person closed is not
	// opened again.
	FindPullRequest(ctx context.Context, branch string) (pr PullRequest, found bool, err error)
	RequestMerge(ctx context.Context, pr PullRequest) error
	// MergeState is where the newest pull request of branch stands, in any
	// state; "" when the branch has none. By branch: the runner keeps no
	// pull request numbers (0091).
	MergeState(ctx context.Context, branch string) (MergeState, error)
}

// PullRequestInput is what a new pull request is opened with.
type PullRequestInput struct {
	Branch string
	Base   string
	Title  string
	Body   string
}

// PullRequest identifies a pull request on the forge.
type PullRequest struct {
	Number int
	URL    string
}

// MergeState is where a pull request stands.
type MergeState string

const (
	MergePending MergeState = "pending" // open, without failed checks
	MergeMerged  MergeState = "merged"
	MergeClosed  MergeState = "closed" // closed without merging
	MergeFailed  MergeState = "failed" // open, with failed checks on its head
)

// New returns the Forge of repository on a forge of kind, as a project names
// it; "" tells the kind from the repository's host, Forgejo for a host of
// its own.
func New(kind, repository, token string) (Forge, error) {
	if kind == "" {
		host, _, err := parseRepository(repository)
		if err != nil {
			return nil, err
		}
		kind = map[string]string{"github.com": "github", "gitlab.com": "gitlab", "bitbucket.org": "bitbucket"}[host]
		if kind == "" {
			kind = "forgejo"
		}
	}
	switch kind {
	case "github":
		return NewGitHub(repository, token)
	case "gitlab":
		return NewGitLab(repository, token)
	case "forgejo":
		return NewForgejo(repository, token)
	case "bitbucket":
		return NewBitbucket(repository, token)
	default:
		return nil, fmt.Errorf("unsupported forge kind %q", kind)
	}
}

// readWaits are the waits between the tries of a forge read; a variable so tests need not wait.
var readWaits = []time.Duration{2 * time.Second, 8 * time.Second}

// send sends req. A GET that got no answer, a 5xx or a 429 is tried again
// after readWaits (CSR-181); any other request is sent once: Publish
// retries the writes itself.
func send(hc *http.Client, req *http.Request) (*http.Response, error) {
	for i := 0; ; i++ {
		resp, err := hc.Do(req)
		retry := err != nil || resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		if !retry || req.Method != http.MethodGet || i == len(readWaits) || req.Context().Err() != nil {
			return resp, err
		}
		if resp != nil {
			resp.Body.Close()
		}
		if err := Sleep(req.Context(), readWaits[i]); err != nil {
			return nil, err
		}
	}
}

// Sleep waits d, or until ctx ends.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// apiError is a non-2xx answer of a forge's API, or GitHub's GraphQL errors in a 200.
type apiError struct {
	forge, method, url string
	status             int
	message            string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s answered %s %s with %d: %s", e.forge, e.method, e.url, e.status, e.message)
}

// status is the HTTP status of err when it is a forge's non-2xx answer, else 0.
func status(err error) int {
	var e *apiError
	if errors.As(err, &e) {
		return e.status
	}
	return 0
}

// do sends one request to the API of the forge named forge, with header
// (its authentication and Accept) and in as its JSON body, when not nil,
// and decodes a 2xx answer into out, when not nil. message reads the
// message of an error answer.
func do(ctx context.Context, hc *http.Client, forge, method, u string, header map[string]string, message func([]byte) string, in, out any) error {
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
	for k, v := range header {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", name.Name+"-runner")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := send(hc, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &apiError{forge, method, u, resp.StatusCode, message(data)}
	}
	if out == nil {
		return nil // Forgejo's merge answers an empty 200
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s answered %s %s with %d and unreadable JSON: %w", forge, method, u, resp.StatusCode, err)
	}
	return nil
}

// textStart is the start of an error answer's text, for when it names no message.
func textStart(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// parseRepository splits a repository URL, given as https://host/path,
// ssh://git@host[:port]/path or git@host:path, with or without .git, into
// its lower-case host and its path without slashes around it. An https
// port is kept in host; an ssh one is dropped, as the forge's API is on https.
func parseRepository(repository string) (host, path string, err error) {
	s := repository
	if h, p, ok := strings.Cut(s, ":"); ok && strings.HasPrefix(s, "git@") && !strings.Contains(s, "://") {
		s = "ssh://" + h + "/" + p // scp form: git@host:o/r
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "ssh") || u.Hostname() == "" {
		return "", "", fmt.Errorf("unsupported repository URL %q", repository)
	}
	host = strings.ToLower(u.Hostname())
	if p := u.Port(); u.Scheme == "https" && p != "" && p != "443" {
		host += ":" + p
	}
	return host, strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), nil
}
