// Package cinderline is the runner's REST client for the issue tracker's runner
// API, as specified in docs/runner-api/openapi.yaml. It sends the runner version
// on every request, records the API version of every response, long-polls for
// work and sends heartbeats.
//
// The runner token and issued agent tokens never appear in its logs or errors:
// it logs only method, path and status, and errors carry only the operation,
// the status code and the server's message.
package cinderline

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/redact"
)

// WorkWait is how many seconds the app holds a Work long-poll open.
const WorkWait = 25

// HeartbeatInterval is how often the runner sends a heartbeat.
const HeartbeatInterval = 60 * time.Second

// maxRows caps GET /issues and GET /issues/{ref}/comments (decision 2026-11-08-comment-cursor).
const maxRows = 200

// Header names.
var (
	runnerVersionHeader = name.Name + "-Runner-Version"
	apiVersionHeader    = name.Name + "-Api-Version"
)

// ErrNoChange is returned by Work when the answer is unchanged since the previous call (304).
var ErrNoChange = errors.New("no change")

// UpgradeRequiredError is a 426: the app needs a newer runner.
type UpgradeRequiredError struct {
	MinimumRunnerVersion string
}

func (e *UpgradeRequiredError) Error() string {
	return "upgrade required: runner must be at least " + e.MinimumRunnerVersion
}

// ConflictError is a 409: the issue's status was not the expected one; nothing changed.
type ConflictError struct {
	Status string // the issue's actual status
}

func (e *ConflictError) Error() string { return "conflict: issue status is " + e.Status }

// APIError is any other unexpected status.
type APIError struct {
	StatusCode int
	Message    string
	Errors     map[string][]string // per-field messages of a 422
}

func (e *APIError) Error() string { return fmt.Sprintf("status %d: %s", e.StatusCode, e.Message) }

// Client talks to one instance with one runner token. Set its fields before
// first use; after that it is safe for concurrent use.
type Client struct {
	HTTP        *http.Client  // must have no Timeout, or it cuts the long-poll short
	Log         *slog.Logger  // one debug line per request
	Timeout     time.Duration // per request, except Work
	WorkTimeout time.Duration // per Work request; above WorkWait seconds

	base, token, version string

	mu         sync.Mutex
	etag       string
	apiVersion string

	errors     map[string]ReportError // key: issue ref for issue errors, kind for runner-wide errors
	lastReport []byte                 // JSON of the last report the server accepted; nil at start

	// heldClients replaces base.Clients in SyncReport when set by
	// SetClients (CSR-137). Nil means use base.Clients.
	heldClients []ReportClient

	refused int // connections the egress proxy refused since start, for the report
}

// New returns a client for the instance at baseURL, e.g. https://cinderline.example.com.
func New(baseURL, token, version string) *Client {
	return &Client{
		HTTP:        &http.Client{},
		Log:         slog.Default(),
		Timeout:     30 * time.Second,
		WorkTimeout: (WorkWait + 15) * time.Second,
		base:        strings.TrimRight(baseURL, "/") + "/api/runner/v1",
		token:       token,
		version:     version,
	}
}

// APIVersion is the API version of the last response, or "" before the first.
func (c *Client) APIVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.apiVersion
}

// GetProject returns the project the token belongs to.
func (c *Client) GetProject(ctx context.Context) (Project, error) {
	var p Project
	err := c.call(ctx, "getProject", "GET", "/project", nil, 200, &p)
	return p, err
}

// Work long-polls for the first To do issue with a team; nil when there is none.
// It returns ErrNoChange when the answer equals the previous one.
func (c *Client) Work(ctx context.Context) (*Issue, error) {
	ctx, cancel := context.WithTimeout(ctx, c.WorkTimeout)
	defer cancel()
	req, err := c.request(ctx, "getWork", "GET", "/work?wait="+strconv.Itoa(WorkWait), nil)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.etag != "" {
		req.Header.Set("If-None-Match", c.etag)
	}
	c.mu.Unlock()
	var out struct {
		Issue *Issue `json:"issue"`
	}
	resp, err := c.send(req, "getWork", 200, &out)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotModified {
		return nil, ErrNoChange
	}
	c.mu.Lock()
	c.etag = resp.Header.Get("ETag")
	c.mu.Unlock()
	return out.Issue, nil
}

// ListIssues returns the issues with a team in any of the statuses, in board order, without comments.
func (c *Client) ListIssues(ctx context.Context, status ...string) ([]Issue, error) {
	var out struct {
		Issues []Issue `json:"issues"`
	}
	q := url.Values{"status": {strings.Join(status, ",")}}
	err := c.call(ctx, "listIssues", "GET", "/issues?"+q.Encode(), nil, 200, &out)
	if err == nil && len(out.Issues) >= maxRows {
		c.Log.Warn(fmt.Sprintf("more than %d %s issues; only the first %d are handled", maxRows, strings.Join(status, ","), maxRows))
	}
	return out.Issues, err
}

// GetIssue returns an issue with its comments.
func (c *Client) GetIssue(ctx context.Context, ref string) (IssueWithComments, error) {
	var out IssueWithComments
	err := c.call(ctx, "getIssue", "GET", issuePath(ref), nil, 200, &out)
	return out, err
}

// UpdateIssue changes an issue's status or merge status; a *ConflictError when
// the status is not ExpectedStatus.
func (c *Client) UpdateIssue(ctx context.Context, ref string, r UpdateIssueRequest) (IssueWithComments, error) {
	var out IssueWithComments
	err := c.call(ctx, "updateIssue", "PATCH", issuePath(ref), r, 200, &out)
	return out, err
}

// ClaimIssue moves an issue from expectedStatus to in_progress; a *ConflictError
// when its status is no longer expectedStatus.
func (c *Client) ClaimIssue(ctx context.Context, ref, expectedStatus string) (IssueWithComments, error) {
	var out IssueWithComments
	in := map[string]string{"expected_status": expectedStatus}
	err := c.call(ctx, "claimIssue", "POST", issuePath(ref)+"/claim", in, 200, &out)
	return out, err
}

// ListComments returns an issue's comments, oldest first, at most 200: with after > 0 the oldest ones newer than that comment ID, else the newest.
func (c *Client) ListComments(ctx context.Context, ref string, after int) ([]Comment, error) {
	var out struct {
		Comments []Comment `json:"comments"`
	}
	path := issuePath(ref) + "/comments"
	if after > 0 {
		path += "?after=" + strconv.Itoa(after)
	}
	err := c.call(ctx, "listComments", "GET", path, nil, 200, &out)
	return out.Comments, err
}

// AddComment adds a comment as the runner user.
// The body is redacted (CSR-157): it can hold an agent's report or output.
func (c *Client) AddComment(ctx context.Context, ref, body string) (Comment, error) {
	var out Comment
	err := c.call(ctx, "addComment", "POST", issuePath(ref)+"/comments", map[string]string{"body": redact.String(body)}, 201, &out)
	return out, err
}

// AddProgress reports what a run is doing now, redacted as AddComment.
func (c *Client) AddProgress(ctx context.Context, ref, body string) error {
	return c.call(ctx, "addProgress", "POST", issuePath(ref)+"/progress", map[string]string{"body": redact.String(body)}, 204, nil)
}

// GetTeam returns a team of the project with its agents.
func (c *Client) GetTeam(ctx context.Context, id int) (Team, error) {
	var out Team
	err := c.call(ctx, "getTeam", "GET", "/teams/"+strconv.Itoa(id), nil, 200, &out)
	return out, err
}

// PutReport replaces the runner's report. The Errors field is always sent as
// a list: nil becomes an empty slice so it is never null in JSON.
func (c *Client) PutReport(ctx context.Context, r Report) error {
	// Redacted in a copy, leaving the caller's slice alone: messages can hold error texts.
	errs := make([]ReportError, len(r.Errors))
	for i, e := range r.Errors {
		e.Message = redact.String(e.Message)
		errs[i] = e
	}
	r.Errors = errs
	return c.call(ctx, "putReport", "PUT", "/report", r, 204, nil)
}

// Heartbeat tells the app the runner is alive.
func (c *Client) Heartbeat(ctx context.Context) error {
	return c.call(ctx, "heartbeat", "POST", "/heartbeat", nil, 204, nil)
}

// SetError stores e under e.Issue (when non-empty) or e.Kind. It returns true
// when the key was absent or the stored error differs from e.
func (c *Client) SetError(e ReportError) bool {
	key := e.Kind
	if e.Issue != "" {
		key = e.Issue
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.errors == nil {
		c.errors = map[string]ReportError{}
	}
	prev, ok := c.errors[key]
	if ok && prev == e {
		return false
	}
	c.errors[key] = e
	return true
}

// ClearError removes the error stored under key (an issue ref or a kind).
func (c *Client) ClearError(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.errors, key)
}

// SetClients stores the detection result in memory so the next SyncReport
// sends it (CSR-137). The caller must keep the version strings alive.
func (c *Client) SetClients(cs []ReportClient) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heldClients = cs
}

// AddRefused counts one connection the egress proxy refused, sent with the
// next SyncReport.
func (c *Client) AddRefused() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refused++
}

// KeepIssueErrors drops every issue error whose ref is not in refs.
// Runner-wide errors (empty Issue) stay.
func (c *Client) KeepIssueErrors(refs map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, e := range c.errors {
		if e.Issue != "" && !refs[e.Issue] {
			delete(c.errors, key)
		}
	}
}

// SyncReport sends the report with the held errors when it differs from the
// last report the server accepted. On success it records the report as sent;
// on failure the next call tries again.
func (c *Client) SyncReport(ctx context.Context, base Report) error {
	c.mu.Lock()
	errs := make([]ReportError, 0, len(c.errors))
	for _, e := range c.errors {
		errs = append(errs, e)
	}
	slices.SortFunc(errs, func(a, b ReportError) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Issue, b.Issue))
	})
	base.Errors = errs
	// When SetClients was called, use held clients instead of base.Clients
	// so the detection result replaces the default empty list.
	clients := base.Clients
	if c.heldClients != nil {
		clients = c.heldClients
	}
	base.Clients = clients
	base.RefusedConnections = c.refused
	encoded, err := json.Marshal(base)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if c.lastReport != nil && bytes.Equal(c.lastReport, encoded) {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	if err := c.PutReport(ctx, base); err != nil {
		return err
	}

	c.mu.Lock()
	c.lastReport = encoded
	c.mu.Unlock()
	return nil
}

// RunHeartbeats sends one heartbeat per tick and syncs the report afterwards,
// until ctx is done. A failed heartbeat is logged as a warn. A sync failure
// logs one Warn "report failed" unless ctx is done.
func (c *Client) RunHeartbeats(ctx context.Context, ticks <-chan time.Time, base Report) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if err := c.Heartbeat(ctx); err != nil && ctx.Err() == nil {
				c.Log.Warn("heartbeat failed", "err", err)
			}
			if err := c.SyncReport(ctx, base); err != nil && ctx.Err() == nil {
				c.Log.Warn("report failed", "err", err)
			}
		}
	}
}

// CreateAgentToken creates a short-lived token for agent's run on the issue.
func (c *Client) CreateAgentToken(ctx context.Context, ref string, kind TokenKind, agent string) (AgentToken, error) {
	var out AgentToken
	in := map[string]string{"kind": string(kind), "agent": agent}
	err := c.call(ctx, "createAgentToken", "POST", "/runs/"+url.PathEscape(ref)+"/agent-token", in, 201, &out)
	if err == nil {
		redact.Add(string(kind)+" token", out.Token)
	}
	return out, err
}

// DeleteAgentTokens deletes every agent token of the issue.
func (c *Client) DeleteAgentTokens(ctx context.Context, ref string) error {
	return c.call(ctx, "deleteAgentTokens", "DELETE", "/runs/"+url.PathEscape(ref)+"/agent-token", nil, 204, nil)
}

func issuePath(ref string) string { return "/issues/" + url.PathEscape(ref) }

// call does one request with the default timeout.
func (c *Client) call(ctx context.Context, op, method, path string, in any, want int, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	req, err := c.request(ctx, op, method, path, in)
	if err != nil {
		return err
	}
	_, err = c.send(req, op, want, out)
	return err
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

// send does req and decodes a want response into out. A 304 is returned as is;
// any other status becomes a typed error. Bodies are never put in errors or logs.
func (c *Client) send(req *http.Request, op string, want int, out any) (*http.Response, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer resp.Body.Close()
	c.Log.Debug("runner api request", "method", req.Method, "path", req.URL.Path, "status", resp.StatusCode)
	if v := resp.Header.Get(apiVersionHeader); v != "" {
		c.mu.Lock()
		c.apiVersion = v
		c.mu.Unlock()
	}

	switch {
	case resp.StatusCode == want:
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				return nil, fmt.Errorf("%s: decode response: %w", op, err)
			}
		}
		return resp, nil
	case resp.StatusCode == http.StatusNotModified && req.Header.Get("If-None-Match") != "":
		return resp, nil
	}

	var e struct {
		Message              string              `json:"message"`
		Status               string              `json:"status"`
		MinimumRunnerVersion string              `json:"minimum_runner_version"`
		Errors               map[string][]string `json:"errors"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e) // a body that is not JSON leaves the message empty
	switch resp.StatusCode {
	case http.StatusUpgradeRequired:
		err = &UpgradeRequiredError{MinimumRunnerVersion: e.MinimumRunnerVersion}
	case http.StatusConflict:
		err = &ConflictError{Status: e.Status}
	default:
		err = &APIError{StatusCode: resp.StatusCode, Message: e.Message, Errors: e.Errors}
	}
	return nil, fmt.Errorf("%s: %w", op, err)
}
