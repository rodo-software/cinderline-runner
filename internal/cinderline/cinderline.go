// Package cinderline is the runner's REST client for the issue tracker's runner
// API, as specified in docs/runner-api/openapi.yaml. It sends the runner version
// on every request, records the API version of every response, long-polls for
// work and sends heartbeats, or one sync request in their place when the app
// offers it (sync.go).
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
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/redact"
)

// WorkWait is how many seconds the app may hold a Work long-poll open, sent
// only when the handshake offers long_poll (CSR-93).
const WorkWait = 25

// HeartbeatInterval is how often the runner sends a heartbeat when the app sends no interval.
const HeartbeatInterval = 60 * time.Second

// Poll names a request the runner repeats on a timer: the app's successful
// answer to it may say, in pollAfterHeader, how many seconds to wait before
// sending it again (CSR-174, EMBER-210). Work and heartbeat are keyed by
// their operation, review and merge by the purpose ListReview sends. While
// the app offers sync, PollSync is the only one that is sent
// (decision 2026-11-16-sync-request).
type Poll string

const (
	PollWork      Poll = "getWork"
	PollHeartbeat Poll = "heartbeat"
	PollReview    Poll = "review"
	PollMerge     Poll = "merge"
	PollSync      Poll = "sync"
)

// The range server intervals are clamped to, so a bad value can neither hot-loop nor stall the runner.
const (
	minPollAfter = time.Second
	maxPollAfter = time.Hour
)

// UpgradeWait is how long every poll waits after a 426: a new minimum
// version stays until the person updates the runner, so asking every few
// seconds only loads the app (CSR-187).
const UpgradeWait = time.Hour

// maxRows caps GET /issues and GET /issues/{ref}/comments (decision 2026-11-08-comment-cursor).
const maxRows = 200

// Header names.
var (
	runnerVersionHeader = name.Name + "-Runner-Version"
	apiVersionHeader    = name.Name + "-Api-Version"
	pollAfterHeader     = name.Name + "-Poll-After"
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
	WorkTimeout time.Duration // per Work request while long-polling; above WorkWait seconds
	// Outbox is the file that keeps the writes Send could not deliver, on
	// the state volume; empty keeps them in memory only (CSR-159).
	Outbox string
	// SendWaits are the waits between Send's tries; nil: 2 and 8 seconds.
	SendWaits []time.Duration
	// SyncWait and SyncWatchWait are the waits between syncs when the app
	// sends no interval: while no turn runs and while one does. Zero:
	// SyncInterval and SyncWatchInterval.
	SyncWait, SyncWatchWait time.Duration

	outMu sync.Mutex
	out   outbox

	base, token, version string

	mu         sync.Mutex
	etag       string
	apiVersion string
	pollAfter  map[string]time.Duration // by operation or purpose; the interval the last successful answer sent
	longPoll   bool                     // the last GetProject offered long_poll
	retryAt    time.Time                // no request before this: the latest Retry-After (CSR-93)
	upgradeAt  time.Time                // no poll before this: UpgradeWait after the latest 426 (CSR-187)

	// Sync mode (sync.go).
	syncing  bool                    // the last GetProject offered sync
	last     *syncResult             // how the latest sync ended; nil before the first
	fresh    bool                    // Work has not returned last yet
	watchers map[chan Watched]string // the refs of the turns that run now, by their watcher's channel
	waiters  chan struct{}           // closed when last or syncing changes, for a Work that waits; nil while none waits
	wake     chan struct{}           // ends the wait in RunHeartbeats early; has room for one

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
		wake:        make(chan struct{}, 1),
		base:        strings.TrimRight(baseURL, "/") + "/api/runner/v1",
		token:       token,
		version:     version,
	}
}

// Version is the runner's version, as it sends it on every request.
func (c *Client) Version() string { return c.version }

// APIVersion is the API version of the last response, or "" before the first.
func (c *Client) APIVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.apiVersion
}

// PollAfter returns the interval, clamped, that the last successful answer
// to p sent, and false when it sent none (an older app): the caller then
// keeps its own default. Within UpgradeWait of a 426 it returns the time
// left of that wait instead.
func (c *Client) PollAfter(p Poll) (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if left := time.Until(c.upgradeAt); left > 0 {
		return left, true
	}
	d, ok := c.pollAfter[string(p)]
	return d, ok
}

// notePollAfter records the interval of a successful answer to req, or forgets
// the one before when it sends none or one that is not whole seconds.
func (c *Client) notePollAfter(req *http.Request, op string, resp *http.Response) {
	key := cmp.Or(req.URL.Query().Get("purpose"), op)
	n, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get(pollAfterHeader)))
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		delete(c.pollAfter, key)
		return
	}
	if c.pollAfter == nil {
		c.pollAfter = map[string]time.Duration{}
	}
	// Clamped in seconds first, so a huge value cannot overflow the Duration.
	c.pollAfter[key] = time.Duration(min(max(n, int(minPollAfter/time.Second)), int(maxPollAfter/time.Second))) * time.Second
}

// GetProject returns the project the token belongs to.
func (c *Client) GetProject(ctx context.Context) (Project, error) {
	var p Project
	err := c.call(ctx, "getProject", "GET", "/project", nil, 200, &p)
	if err == nil {
		c.mu.Lock()
		c.longPoll = p.LongPoll
		if c.syncing != p.Sync {
			c.setSyncing(p.Sync)
		}
		c.mu.Unlock()
	}
	return p, err
}

// Work polls for the first To do issue with a team; nil when there is none.
// It long-polls only when the last GetProject offered it; before that, or
// against an app that does not, it asks for an answer at once (CSR-93).
// It returns ErrNoChange when the answer equals the previous one.
//
// While the app offers sync it sends nothing: it returns the work of a sync
// answer, each answer once, waiting for the next one when it has returned the
// latest, and a failed sync's error (decision 2026-11-16-sync-request).
func (c *Client) Work(ctx context.Context) (*Issue, error) {
	if issue, ok, err := c.syncedWork(ctx); ok {
		return issue, err
	}
	if err := c.pause(ctx); err != nil {
		return nil, fmt.Errorf("getWork: %w", err)
	}
	c.mu.Lock()
	path, timeout := "/work", c.Timeout
	if c.longPoll {
		path, timeout = "/work?wait="+strconv.Itoa(WorkWait), c.WorkTimeout
	}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := c.request(ctx, "getWork", "GET", path, nil)
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
	return c.listIssues(ctx, url.Values{"status": {strings.Join(status, ",")}})
}

// ListReview returns the Review issues, as ListIssues, for the review-branch
// or merge check: purpose tells the app which, so it can send each its own
// interval (EMBER-210). While the app offers sync it sends nothing and
// returns the Review list of the latest sync answer.
func (c *Client) ListReview(ctx context.Context, purpose Poll) ([]Issue, error) {
	if issues, ok, err := c.syncedReview(); ok {
		if err == nil && len(issues) >= maxRows {
			c.Log.Warn(fmt.Sprintf("more than %d review issues; only the first %d are handled", maxRows, maxRows))
		}
		return issues, err
	}
	return c.listIssues(ctx, url.Values{"status": {"review"}, "purpose": {string(purpose)}})
}

func (c *Client) listIssues(ctx context.Context, q url.Values) ([]Issue, error) {
	var out struct {
		Issues []Issue `json:"issues"`
	}
	status := q["status"]
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

// RunHeartbeats waits, sends a heartbeat and syncs the report afterwards,
// until ctx is done. The wait is the interval the last heartbeat answer sent,
// else HeartbeatInterval, with jitter, measured from that answer (CSR-174).
// A failed heartbeat is logged as a warn. A sync failure logs one Warn
// "report failed" unless ctx is done.
//
// While the app offers sync, a POST /sync takes the heartbeat's place
// (decision 2026-11-16-sync-request): one per wait, and one at once when the
// handshake first offers it or SyncNow asks. The wait is then the interval
// the last sync answer sent, else SyncWait, or SyncWatchWait while a turn is
// watched.
func (c *Client) RunHeartbeats(ctx context.Context, base Report) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
			if !c.Syncing() {
				continue // sync was withdrawn: the heartbeat waits its own interval
			}
		case <-time.After(Jitter(c.beatWait())):
		}
		if c.Syncing() {
			c.syncOnce(ctx)
		} else if err := c.Heartbeat(ctx); err != nil && ctx.Err() == nil {
			c.Log.Warn("heartbeat failed", "err", err)
		}
		if err := c.SyncReport(ctx, base); err != nil && ctx.Err() == nil {
			c.Log.Warn("report failed", "err", err)
		}
	}
}

// Jitter returns d plus a random wait of up to a quarter of d, at most three
// seconds, so many runners polling one app do not line up.
func Jitter(d time.Duration) time.Duration {
	j := min(d/4, 3*time.Second)
	if j <= 0 {
		return d
	}
	return d + rand.N(j)
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

// ReportUsage records the usage of a run's agent CLI runs on the top-level
// issue ref and its sub-issues, in one call per run (EMBER-112, EMBER-161).
func (c *Client) ReportUsage(ctx context.Context, ref string, entries []UsageEntry) error {
	in := map[string][]UsageEntry{"entries": entries}
	return c.call(ctx, "reportUsage", "POST", "/runs/"+url.PathEscape(ref)+"/usage", in, 204, nil)
}

// Next reports the step the runner just did in the run of the top-level
// issue ref and returns the next one (nextStep, decision
// 2026-11-15-server-decides-steps). One try: the caller decides on retries.
func (c *Client) Next(ctx context.Context, ref string, r NextRequest) (NextResponse, error) {
	var out NextResponse
	err := c.call(ctx, "nextStep", "POST", "/runs/"+url.PathEscape(ref)+"/next", r, 200, &out)
	return out, err
}

// DeleteAgentTokens deletes every agent token of the issue.
func (c *Client) DeleteAgentTokens(ctx context.Context, ref string) error {
	return c.call(ctx, "deleteAgentTokens", "DELETE", "/runs/"+url.PathEscape(ref)+"/agent-token", nil, 204, nil)
}

// MaxAttachmentBytes caps one attachment's download: Cinderline allows 10 MB
// per issue (EMBER-208).
const MaxAttachmentBytes = 11 << 20

// errTooLarge is a download over MaxAttachmentBytes.
var errTooLarge = errors.New("attachment larger than " + strconv.Itoa(MaxAttachmentBytes>>20) + " MB")

// downloadTimeout is per attachment: 10 MB on a slow line takes longer than Timeout.
const downloadTimeout = 5 * time.Minute

// DownloadAttachment streams attachment id of issue ref to a new file at
// path, refusing more than MaxAttachmentBytes; on an error the file is
// removed. The path is built from ref and id rather than the attachment's
// url, so the runner token only ever goes to the instance's own API.
func (c *Client) DownloadAttachment(ctx context.Context, ref string, id int, path string) error {
	if err := c.pause(ctx); err != nil {
		return fmt.Errorf("downloadAttachment: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := c.request(ctx, "downloadAttachment", "GET", issuePath(ref)+"/attachments/"+strconv.Itoa(id), nil)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) // read by the container's agent user
	if err != nil {
		return err
	}
	_, err = c.send(req, "downloadAttachment", 200, &capped{w: f, n: MaxAttachmentBytes})
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

func issuePath(ref string) string { return "/issues/" + url.PathEscape(ref) }

// call does one request with the default timeout.
func (c *Client) call(ctx context.Context, op, method, path string, in any, want int, out any) error {
	if err := c.pause(ctx); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	req, err := c.request(ctx, op, method, path, in)
	if err != nil {
		return err
	}
	_, err = c.send(req, op, want, out)
	return err
}

// pause waits until the latest Retry-After has passed, or ctx is done. It
// holds every request, not only the one refused: the app limits per token
// (EMBER-161), and a 503 during a deploy means the whole app.
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

// retryAfter parses a Retry-After value, whole seconds or an HTTP date,
// capped like the poll intervals so a bad value cannot stall the runner for good.
func retryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		// Clamped in seconds first, so a huge value cannot overflow the Duration.
		return time.Duration(min(max(n, 0), int64(maxPollAfter/time.Second))) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(t), 0), maxPollAfter), true
	}
	return 0, false
}

// Unavailable reports whether err means Cinderline did not answer for now:
// no answer, a timeout, a 5xx, 408 or 429, as during a deploy or under load
// (CSR-93). Waiting may help; any other answer will not change by waiting.
func Unavailable(err error) bool {
	var api *APIError
	if errors.As(err, &api) {
		return api.StatusCode >= 500 || api.StatusCode == http.StatusRequestTimeout || api.StatusCode == http.StatusTooManyRequests
	}
	var u *url.Error
	return errors.As(err, &u)
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
		if w, ok := out.(io.Writer); ok {
			if _, err := io.Copy(w, resp.Body); err != nil {
				return nil, fmt.Errorf("%s: %w", op, err)
			}
		} else if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				return nil, fmt.Errorf("%s: decode response: %w", op, err)
			}
		}
		c.notePollAfter(req, op, resp)
		return resp, nil
	case resp.StatusCode == http.StatusNotModified && req.Header.Get("If-None-Match") != "":
		c.notePollAfter(req, op, resp)
		return resp, nil
	}

	if d, ok := retryAfter(resp.Header.Get("Retry-After")); ok {
		c.mu.Lock()
		if t := time.Now().Add(d); t.After(c.retryAt) {
			c.retryAt = t
		}
		c.mu.Unlock()
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
		c.mu.Lock()
		c.upgradeAt = time.Now().Add(UpgradeWait)
		c.mu.Unlock()
	case http.StatusConflict:
		err = &ConflictError{Status: e.Status}
	default:
		err = &APIError{StatusCode: resp.StatusCode, Message: e.Message, Errors: e.Errors}
	}
	return nil, fmt.Errorf("%s: %w", op, err)
}
