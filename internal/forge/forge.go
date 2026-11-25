// Package forge defines Forge, the git host of a project's repository
// (pull requests and merges), its implementations GitHub (GitHub and
// GitHub Enterprise), GitLab (gitlab.com and self-managed) and Forgejo
// (Forgejo and Gitea), Bitbucket (Bitbucket Cloud), and Fake, its in-memory
// stand-in for tests.
package forge

import (
	"bytes"
	"cmp"
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
	// state; State "" when the branch has none. By branch: the runner keeps
	// no pull request numbers (0091).
	MergeState(ctx context.Context, branch string) (PullState, error)
	// CheckLog is the end of the log of a check MergeState named, at most
	// logBytes; "" and no error when the forge has no log of it the runner
	// can read. The runner reads it through Log.
	CheckLog(ctx context.Context, c Check) (string, error)
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

// PullState is where the newest pull request of a branch stands.
type PullState struct {
	State MergeState
	Head  string // its head commit; Bitbucket names it by a short hash
	// With MergeFailed: Running says a check of the head has not finished,
	// and Failed are the head's failed checks, without their logs.
	Running bool
	Failed  []Check
	// With MergePending and no check running: the forge's words when it has
	// no merge of the open pull request scheduled; "" when one is scheduled
	// or the forge cannot tell (Forgejo, Bitbucket).
	Unscheduled string
}

// Check is a CI check that failed on a pull request's head. Its texts are
// the forge's, untrusted.
type Check struct {
	Name, Description, URL string

	job  string // the forge's ID of the job whose log CheckLog reads; "": none
	head string // the head commit, for Forgejo, which finds the job by it
	text string // the forge's own words on the failure beyond Description, which Log sends without a log; "": none
}

// Log is what the runner sends as the log of the failed check c, never ""
// (CLR-244): the agent cannot reach the forge, so this is all it gets. It
// is the end of the check's CI log, read a second time when the first read
// fails; without one, the forge's own words on the failure (its text, else
// Description), after a line saying why there is no log. err is the read's
// failure, for the runner's own log. URLs are left in: the app client
// strips them from every text of a check.
func Log(ctx context.Context, f Forge, c Check) (string, error) {
	log, err := f.CheckLog(ctx, c)
	if err != nil && ctx.Err() == nil {
		log, err = f.CheckLog(ctx, c)
	}
	if err == nil && strings.TrimSpace(log) != "" {
		return log, nil
	}
	why := "No log of this check can be read through the forge's API; its full log, if any, is with the CI that reported it."
	if err != nil {
		why = "Reading this check's log failed: " + shortError(err)
	}
	text := cmp.Or(strings.TrimSpace(c.text), strings.TrimSpace(c.Description), "The forge says only that this check failed.")
	return why + "\n\n" + text, err
}

// shortError is err without the request's URL when it is a forge's answer.
func shortError(err error) string {
	var e *apiError
	if errors.As(err, &e) {
		return fmt.Sprintf("%s answered %d: %s", e.forge, e.status, e.message)
	}
	return err.Error()
}

// Refusal is the forge's own words when it refused a request, for the reason
// the runner sends the app (CLR-245); "" when the error is not a refusal the
// agent could act on.
func Refusal(err error) string {
	var e *apiError
	if errors.As(err, &e) && e.status < 500 {
		return shortError(err)
	}
	return ""
}

// logBytes is how much of the end of a check's log CheckLog reads: enough
// for the 4,000 characters the app takes, in any UTF-8.
const logBytes = 16 << 10

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

// logTail is the last logBytes of the plain-text log at u. It asks for
// only those with a Range request; a server that ignores it sends the whole
// log, which is read to its end and only its end kept. header is the
// forge's authentication, and its Accept when plain text is not right.
func logTail(ctx context.Context, hc *http.Client, forge, u string, header map[string]string) (string, error) {
	return logTailMessage(ctx, hc, forge, u, header, func(data []byte) string { return textStart(string(data)) })
}

// logTailMessage is logTail, reading the error answer's message with message.
func logTailMessage(ctx context.Context, hc *http.Client, forge, u string, header map[string]string, message func([]byte) string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/plain, */*")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", name.Name+"-runner")
	req.Header.Set("Range", fmt.Sprintf("bytes=-%d", logBytes))
	resp, err := send(hc, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return "", &apiError{forge, http.MethodGet, u, resp.StatusCode, message(data)}
	}
	t := &tail{n: logBytes}
	if _, err := io.Copy(t, resp.Body); err != nil {
		return "", err
	}
	// The cut can split a character.
	return strings.ToValidUTF8(string(t.end()), ""), nil
}

// tail is a writer that keeps the last n bytes written to it.
type tail struct {
	b []byte
	n int
}

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2*t.n {
		t.b = append(t.b[:0], t.b[len(t.b)-t.n:]...)
	}
	return len(p), nil
}

func (t *tail) end() []byte { return t.b[max(0, len(t.b)-t.n):] }

// textStart is the start of an error answer's text, for when it names no message.
func textStart(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// parseRepository splits a repository URL, https://host/path with or
// without .git, into its lower-case host, with a port other than 443, and
// its path without slashes around it.
func parseRepository(repository string) (host, path string, err error) {
	u, err := url.Parse(repository)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
		return "", "", fmt.Errorf("repository URL %q is not https://host/owner/repository without a user name", repository)
	}
	host = strings.ToLower(u.Hostname())
	if p := u.Port(); p != "" && p != "443" {
		host += ":" + p
	}
	return host, strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), nil
}
