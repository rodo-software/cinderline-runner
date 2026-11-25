// Package cinderline is the runner's client for the app's runner API
// (docs/runner-api/openapi.yaml, decision 2026-11-16-runner-protocol-v2):
// Poll, Next and DownloadAttachment, and the outbox of Next requests that
// could not be delivered.
//
// The runner token and agent tokens never appear in its logs or errors: it
// logs only method, path and status, and errors carry only the operation,
// the status code and the server's message.
package cinderline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/redact"
)

// Header names.
var (
	runnerVersionHeader = name.Name + "-Runner-Version"
	apiVersionHeader    = name.Name + "-Api-Version"
)

// maxRetryAfter caps a Retry-After, so a bad value cannot stall the runner for good.
const maxRetryAfter = time.Hour

// UpgradeRequiredError is a 426: the app needs a newer runner.
type UpgradeRequiredError struct {
	MinimumRunnerVersion string
}

func (e *UpgradeRequiredError) Error() string {
	return "upgrade required: runner must be at least " + e.MinimumRunnerVersion
}

// ConflictError is a 409: the issue is not where the request expects it.
type ConflictError struct {
	Message string
	Status  string // the issue's status
}

func (e *ConflictError) Error() string {
	return "conflict: " + e.Message + " (status " + e.Status + ")"
}

// APIError is any other unexpected status.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string { return fmt.Sprintf("status %d: %s", e.StatusCode, e.Message) }

// Unavailable reports whether err means the app did not answer for now: no
// answer, a timeout, a 5xx, 408 or 429, as during a deploy or under load.
// Waiting may help; any other answer will not change by waiting.
func Unavailable(err error) bool {
	var api *APIError
	if errors.As(err, &api) {
		return api.StatusCode >= 500 || api.StatusCode == http.StatusRequestTimeout || api.StatusCode == http.StatusTooManyRequests
	}
	var u *url.Error
	return errors.As(err, &u)
}

// Client talks to one instance with one runner token. Set its fields before
// first use; after that it is safe for concurrent use.
type Client struct {
	HTTP    *http.Client
	Log     *slog.Logger  // one debug line per request
	Timeout time.Duration // per request, except a download
	// Outbox is the file of the Next requests that could not be delivered;
	// empty keeps them in memory only.
	Outbox string

	base, token, version string

	mu      sync.Mutex
	retryAt time.Time // no request before this: the latest Retry-After

	outMu  sync.Mutex
	loaded bool
	queued []Queued
}

// New returns a client for the instance at baseURL, e.g. https://cinderline.example.com.
func New(baseURL, token, version string) *Client {
	return &Client{
		HTTP:    &http.Client{},
		Log:     slog.Default(),
		Timeout: 30 * time.Second,
		base:    strings.TrimRight(baseURL, "/") + "/api/runner/v2",
		token:   token,
		version: version,
	}
}

// Version is the runner's version, as it sends it on every request.
func (c *Client) Version() string { return c.version }

// Poll sends the runner's report and what it holds, and returns what to do.
// Report error messages and the texts of failed checks are redacted, and
// everything is cut to the app's limits: the first entries of a list are sent.
func (c *Client) Poll(ctx context.Context, r PollRequest) (PollAnswer, error) {
	errs := make([]ReportError, 0, len(r.Report.Errors)) // a copy: the caller's slice stays
	for _, e := range first(r.Report.Errors, maxErrors) {
		e.Message, e.Issue = clip(redact.String(e.Message), MaxErrorMessage), clip(e.Issue, maxRef)
		errs = append(errs, e)
	}
	r.Report.Errors = errs
	r.Report.RunnerVersion = clip(r.Report.RunnerVersion, maxName)
	clients := make([]ReportClient, 0, len(r.Report.Clients))
	for _, cl := range first(r.Report.Clients, maxClients) {
		cl.Name = clip(cl.Name, maxName)
		if cl.Version != nil {
			v := clip(*cl.Version, maxName)
			cl.Version = &v
		}
		cl.Models = modelIDs(cl.Models, maxClientModel)
		clients = append(clients, cl)
	}
	r.Report.Clients = clients
	r.Report.Environments = first(r.Report.Environments, maxEnvironments)
	r.Report.ModelProviders = fitting(r.Report.ModelProviders, maxProviders, maxProviderName)
	var free []string // nil when none, so the field is left out
	for _, n := range r.Report.FreeProviders {
		if len(free) < maxProviders && n != "openrouter" && slices.Contains(r.Report.ModelProviders, n) {
			free = append(free, n)
		}
	}
	r.Report.FreeProviders = free
	models := []ProviderModels{}
	for _, pm := range r.Report.ProviderModels {
		if len(models) < maxProviders && utf8.RuneCountInString(pm.Provider) <= maxProviderName {
			if pm.Models = modelIDs(pm.Models, maxProviderModel); pm.Models == nil {
				pm.Models = []string{} // the app needs the key
			}
			models = append(models, pm)
		}
	}
	r.Report.ProviderModels = models
	r.Running = refs(r.Running, maxRunning)
	r.Held = refs(r.Held, maxListed)
	if r.Review != nil {
		review := []ReviewFact{}
		for _, f := range r.Review {
			if len(review) < maxListed && utf8.RuneCountInString(f.Ref) <= maxRef {
				f.Conflicts = clipAll(f.Conflicts, maxConflicts, maxText)
				f.Checks = checks(f.Checks)
				f.Reason = reason(f.Reason)
				review = append(review, f)
			}
		}
		r.Review = review
	}
	var out PollAnswer
	err := c.call(ctx, "poll", "POST", "/poll", r, &out)
	return out, err
}

// Project asks which project the token belongs to, with a poll that carries
// no report: the app then stores, blocks and offers nothing. For add and key.
func (c *Client) Project(ctx context.Context) (Project, error) {
	var out PollAnswer
	err := c.call(ctx, "poll", "POST", "/poll", struct{}{}, &out)
	return out.Project, err
}

// Next reports the step the runner just did in the run of the top-level
// issue ref and returns the next one. One try: the caller decides on
// retries. The texts it sends are redacted.
func (c *Client) Next(ctx context.Context, ref string, r NextRequest) (NextAnswer, error) {
	var out NextAnswer
	err := c.call(ctx, "next", "POST", "/runs/"+url.PathEscape(ref)+"/next", redacted(r), &out)
	return out, err
}

// redacted is r with the texts that can hold an agent's output or an error
// redacted, the error's URLs replaced, and every text and list cut to the
// app's limits: a tail keeps its end, anything else its start. The caller's
// r is not changed.
func redacted(r NextRequest) NextRequest {
	text := func(s *string, n int) *string {
		if s == nil {
			return nil
		}
		v := clip(redact.String(*s), n)
		return &v
	}
	if r.Last != nil {
		l := *r.Last
		if l.Error != nil {
			// The app quotes it to agents, which get no forge URL (CL
			// decision 2026-11-25-forge-off-limits-for-agents).
			e := noURLs(*l.Error)
			l.Error = &e
		}
		l.Error, l.Report, l.Commits = text(l.Error, maxStepError), text(l.Report, maxLongText), text(l.Commits, maxLongText)
		l.PullRequest, l.Commit = text(l.PullRequest, maxURL), text(l.Commit, maxCommit)
		if l.Tail != nil {
			v := clipTail(redact.String(*l.Tail), maxTail)
			l.Tail = &v
		}
		l.Conflicts = clipAll(l.Conflicts, maxConflicts, maxText)
		l.Checks = checks(l.Checks)
		l.Reason = reason(l.Reason)
		if l.Secrets != nil {
			found := make([]StepSecret, 0, min(len(l.Secrets), maxSecrets))
			for _, f := range first(l.Secrets, maxSecrets) {
				// A file's name is the agent's text, so it may hold a value too.
				f.Kind, f.Commit, f.Path = clip(f.Kind, maxName), clip(f.Commit, maxCommit), text(f.Path, maxText)
				found = append(found, f)
			}
			l.Secrets = found
		}
		if l.RefusedHosts != nil {
			hosts := []string{}
			for _, h := range l.RefusedHosts {
				// Left out rather than redacted or clipped: each one must stay a host:port.
				if len(hosts) < maxRefusedHosts && utf8.RuneCountInString(h) <= maxHostPort && redact.String(h) == h {
					hosts = append(hosts, h)
				}
			}
			l.RefusedHosts = hosts
		}
		l.Issue, l.Agent = clip(l.Issue, maxIssue), text(l.Agent, maxName)
		if u := l.Usage; u != nil {
			v := *u
			v.Issue, v.Agent, v.Model = clip(v.Issue, maxIssue), clip(v.Agent, maxName), text(v.Model, maxName)
			v.Tools = nil
			for _, t := range u.Tools {
				// Left out rather than redacted or clipped: either would count another tool.
				if len(v.Tools) < maxTools && t.Name != "" && utf8.RuneCountInString(t.Name) <= maxName && redact.String(t.Name) == t.Name && t.Calls > 0 {
					v.Tools = append(v.Tools, ToolCalls{t.Name, min(t.Calls, maxToolCalls)})
				}
			}
			l.Usage = &v
		}
		r.Last = &l
	}
	sessions := make([]Session, 0, len(r.Sessions))
	for _, s := range first(r.Sessions, maxSessions) {
		s.Issue, s.Agent = clip(s.Issue, maxIssue), clip(s.Agent, maxName)
		sessions = append(sessions, s)
	}
	r.Sessions = sessions
	w := r.Workspace
	w.Notes = make([]string, 0, len(w.Notes))
	for _, n := range first(r.Workspace.Notes, maxNotes) {
		w.Notes = append(w.Notes, clip(redact.String(n), maxText))
	}
	w.Branch, w.Base = clip(w.Branch, maxPath), clip(w.Base, maxPath)
	w.ExtraRepositories = make([]WorkspaceRepository, 0, len(r.Workspace.ExtraRepositories))
	for _, e := range first(r.Workspace.ExtraRepositories, maxWorkspaceList) {
		w.ExtraRepositories = append(w.ExtraRepositories, WorkspaceRepository{Path: clip(e.Path, maxPath), URL: clip(e.URL, maxURL)})
	}
	w.LinkedProjects = make([]WorkspaceLink, 0, len(r.Workspace.LinkedProjects))
	for _, l := range first(r.Workspace.LinkedProjects, maxWorkspaceList) {
		w.LinkedProjects = append(w.LinkedProjects, WorkspaceLink{Key: clip(l.Key, maxKey), Paths: clipAll(l.Paths, maxWorkspaceList, maxPath)})
	}
	r.Workspace = w
	return r
}

// MaxAttachmentBytes caps one attachment's download: the app allows 10 MB per issue.
const MaxAttachmentBytes = 11 << 20

var errTooLarge = errors.New("attachment larger than " + strconv.Itoa(MaxAttachmentBytes>>20) + " MB")

// downloadTimeout is per attachment: 10 MB on a slow line takes longer than Timeout.
const downloadTimeout = 5 * time.Minute

// DownloadAttachment streams attachment id of issue ref to a new file at
// path, refusing more than MaxAttachmentBytes; on an error the file is
// removed. The URL is built from ref and id, so the runner token only ever
// goes to the instance's own API.
func (c *Client) DownloadAttachment(ctx context.Context, ref string, id int, path string) error {
	if err := c.pause(ctx); err != nil {
		return fmt.Errorf("downloadAttachment: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := c.request(ctx, "downloadAttachment", "GET", "/issues/"+url.PathEscape(ref)+"/attachments/"+strconv.Itoa(id), nil)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) // read by the container's agent user
	if err != nil {
		return err
	}
	err = c.send(req, "downloadAttachment", &capped{w: f, n: MaxAttachmentBytes})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

// capped writes to w and refuses to write more than n bytes in total.
type capped struct {
	w io.Writer
	n int64
}

func (c *capped) Write(p []byte) (int, error) {
	if int64(len(p)) > c.n {
		return 0, errTooLarge
	}
	c.n -= int64(len(p))
	return c.w.Write(p)
}

// call does one JSON request with the default timeout, after any Retry-After.
func (c *Client) call(ctx context.Context, op, method, path string, in, out any) error {
	if err := c.pause(ctx); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	req, err := c.request(ctx, op, method, path, in)
	if err != nil {
		return err
	}
	return c.send(req, op, out)
}

// pause waits until the latest Retry-After has passed, or ctx is done. It
// holds every request, not only the one refused: the app limits per token,
// and a 503 during a deploy means the whole app.
func (c *Client) pause(ctx context.Context) error {
	c.mu.Lock()
	d := time.Until(c.retryAt)
	c.mu.Unlock()
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// retryAfter parses a Retry-After value, whole seconds or an HTTP date, capped.
func retryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		// Clamped in seconds first, so a huge value cannot overflow the Duration.
		return time.Duration(min(max(n, 0), int64(maxRetryAfter/time.Second))) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(t), 0), maxRetryAfter), true
	}
	return 0, false
}

func (c *Client) request(ctx context.Context, op, method, path string, in any) (*http.Request, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set(runnerVersionHeader, c.version)
	return req, nil
}

// send does req and decodes a 200 into out: JSON, or the bytes when out is
// an io.Writer. Any other status becomes a typed error. Bodies are never put
// in errors or logs, only the server's message.
func (c *Client) send(req *http.Request, op string, out any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer resp.Body.Close()
	c.Log.Debug("runner api request", "method", req.Method, "path", req.URL.Path, "status", resp.StatusCode, "api_version", resp.Header.Get(apiVersionHeader))

	if resp.StatusCode == http.StatusOK {
		if w, ok := out.(io.Writer); ok {
			_, err = io.Copy(w, resp.Body)
		} else if err = json.NewDecoder(resp.Body).Decode(out); err != nil {
			err = fmt.Errorf("decode response: %w", err)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		return nil
	}

	if d, ok := retryAfter(resp.Header.Get("Retry-After")); ok {
		c.mu.Lock()
		if t := time.Now().Add(d); t.After(c.retryAt) {
			c.retryAt = t
		}
		c.mu.Unlock()
	}
	var e struct {
		Message              string `json:"message"`
		Status               string `json:"status"`
		MinimumRunnerVersion string `json:"minimum_runner_version"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e) // a body that is not JSON leaves them empty
	switch resp.StatusCode {
	case http.StatusUpgradeRequired:
		err = &UpgradeRequiredError{MinimumRunnerVersion: e.MinimumRunnerVersion}
	case http.StatusConflict:
		err = &ConflictError{Message: e.Message, Status: e.Status}
	default:
		err = &APIError{StatusCode: resp.StatusCode, Message: e.Message}
	}
	return fmt.Errorf("%s: %w", op, err)
}
