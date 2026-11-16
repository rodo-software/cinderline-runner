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
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/name"
)

const (
	runnerToken = "runner-secret-7f3a"
	agentToken  = "agent-secret-9b1c"
	version     = "1.2.0"
)

const issueJSON = `{"id":1,"number":9,"ref":"CSR-9","title":"T","body":"b",
	"status":{"value":"todo","label":"To do"},"priority":{"value":"normal","label":"Normal"},
	"merge_status":null,"position":3,"team":{"id":2,"name":"Dev"},"parent":{"ref":"CSR-1","title":"P"},
	"agent":null,"sub_issues":[{"ref":"CSR-10","title":"S","status":{"value":"done","label":"Done"}}],
	"attachments":[{"id":5,"name":"spec.pdf","size":1234,"mime_type":"application/pdf","url":"https://cs.example.com/api/runner/v1/issues/CSR-9/attachments/5"}],
	"created_at":"2026-11-05T10:00:00Z","updated_at":"2026-11-05T11:00:00Z","completed_at":null,"archived_at":null}`

const commentJSON = `{"id":4,"body":"hi","author":{"name":"Runner","agent":null},"created_at":"2026-11-05T10:00:00Z"}`

var issueWithCommentsJSON = strings.TrimSuffix(issueJSON, "}") + `,"comments":[` + commentJSON + `],
	"usage":{"input_tokens":10,"output_tokens":20,"cache_read_tokens":30,"cache_write_tokens":40,"cost_usd":5.126,"cost_incomplete":true},
	"budget_usd":5,"over_budget":true}`

// recorded is what the test server received.
type recorded struct {
	method, path, query, body string
	header                    http.Header
}

// server answers every request with status and body and records the last request.
func server(t *testing.T, status int, body string, hdr map[string]string) (*Client, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*rec = recorded{r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Clone()}
		w.Header().Set(name.Name+"-Api-Version", "1.0.0")
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		if body != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL+"/", runnerToken, version)
	c.HTTP = srv.Client()
	return c, rec
}

// sameJSON reports whether a and b hold the same JSON value.
func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal([]byte(a), &x); err != nil {
		t.Fatalf("bad JSON %q: %v", a, err)
	}
	if err := json.Unmarshal([]byte(b), &y); err != nil {
		t.Fatalf("bad JSON %q: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

// operation is one runner API call: what the server must receive, what it answers,
// and the result, which must marshal back to the response body.
type operation struct {
	name                     string
	call                     func(context.Context, *Client) (any, error)
	method, path, query, req string
	status                   int
	resp                     string
}

var operations = []operation{
	{"getProject", func(ctx context.Context, c *Client) (any, error) { return c.GetProject(ctx) },
		"GET", "/api/runner/v1/project", "", "", 200,
		`{"key":"CSR","name":"Runner","repository_url":"https://git.example.com/r","forge":"forgejo","base_branch":"main","environment":"docker","compose_file":"ops/compose.yml","agent_service":"php","check_command":"make check","linked_projects":[{"key":"EMBER","name":"App","repository_url":"https://git.example.com/app","base_branch":"main"}]}`},
	{"getWork", func(ctx context.Context, c *Client) (any, error) {
		iss, err := c.Work(ctx)
		return map[string]any{"issue": iss}, err
	}, "GET", "/api/runner/v1/work", "", "", 200, `{"issue":` + issueJSON + `}`},
	{"listIssues", func(ctx context.Context, c *Client) (any, error) {
		is, err := c.ListIssues(ctx, "in_progress", "review")
		return map[string]any{"issues": is}, err
	}, "GET", "/api/runner/v1/issues", "status=in_progress%2Creview", "", 200, `{"issues":[` + issueJSON + `]}`},
	{"getIssue", func(ctx context.Context, c *Client) (any, error) { return c.GetIssue(ctx, "CSR-9") },
		"GET", "/api/runner/v1/issues/CSR-9", "", "", 200, issueWithCommentsJSON},
	{"updateIssue", func(ctx context.Context, c *Client) (any, error) {
		return c.UpdateIssue(ctx, "CSR-9", UpdateIssueRequest{Status: "review", ExpectedStatus: "in_progress"})
	}, "PATCH", "/api/runner/v1/issues/CSR-9", "", `{"status":"review","expected_status":"in_progress"}`, 200, issueWithCommentsJSON},
	{"updateIssue merge", func(ctx context.Context, c *Client) (any, error) {
		return c.UpdateIssue(ctx, "CSR-9", UpdateIssueRequest{MergeStatus: "success"})
	}, "PATCH", "/api/runner/v1/issues/CSR-9", "", `{"merge_status":"success"}`, 200, issueWithCommentsJSON},
	{"updateIssue clear merge", func(ctx context.Context, c *Client) (any, error) {
		return c.UpdateIssue(ctx, "CSR-9", UpdateIssueRequest{ClearMergeStatus: true})
	}, "PATCH", "/api/runner/v1/issues/CSR-9", "", `{"merge_status":null}`, 200, issueWithCommentsJSON},
	{"claimIssue", func(ctx context.Context, c *Client) (any, error) { return c.ClaimIssue(ctx, "CSR-9", "todo") },
		"POST", "/api/runner/v1/issues/CSR-9/claim", "", `{"expected_status":"todo"}`, 200, issueWithCommentsJSON},
	{"listComments", func(ctx context.Context, c *Client) (any, error) {
		cs, err := c.ListComments(ctx, "CSR-9", 0)
		return map[string]any{"comments": cs}, err
	}, "GET", "/api/runner/v1/issues/CSR-9/comments", "", "", 200, `{"comments":[` + commentJSON + `]}`},
	{"listComments after", func(ctx context.Context, c *Client) (any, error) {
		cs, err := c.ListComments(ctx, "CSR-9", 250)
		return map[string]any{"comments": cs}, err
	}, "GET", "/api/runner/v1/issues/CSR-9/comments", "after=250", "", 200, `{"comments":[` + commentJSON + `]}`},
	{"addComment", func(ctx context.Context, c *Client) (any, error) { return c.AddComment(ctx, "CSR-9", "hi") },
		"POST", "/api/runner/v1/issues/CSR-9/comments", "", `{"body":"hi"}`, 201, commentJSON},
	{"addProgress", func(ctx context.Context, c *Client) (any, error) { return nil, c.AddProgress(ctx, "CSR-9", "step 2") },
		"POST", "/api/runner/v1/issues/CSR-9/progress", "", `{"body":"step 2"}`, 204, ""},
	{"getTeam", func(ctx context.Context, c *Client) (any, error) { return c.GetTeam(ctx, 2) },
		"GET", "/api/runner/v1/teams/2", "", "", 200,
		`{"id":2,"name":"Dev","max_minutes":30,"max_turns":0,"trusted":true,"max_sub_issues":7,"ponytail":true,"generic_runs":true,"agents":[{"name":"Ada","role":"orchestrator","client":"claude","model":null,"effort":null,"instructions":"be brief","description":null,"price":{"input":5,"output":25}},{"name":"Qwen","role":"worker","client":"opencode","model":"q","effort":"high","instructions":null,"description":"Cheap.","price":null}]}`},
	{"putReport", func(ctx context.Context, c *Client) (any, error) {
		v := "2.0.1"
		return nil, c.PutReport(ctx, Report{RunnerVersion: version, Clients: []ReportClient{
			{Name: "claude", Version: &v, State: "ready"}, {Name: "opencode", State: "missing_binary"},
		}, Environments: []string{"default", "docker"}})
	}, "PUT", "/api/runner/v1/report", "",
		`{"runner_version":"1.2.0","clients":[{"name":"claude","version":"2.0.1","state":"ready"},{"name":"opencode","version":null,"state":"missing_binary"}],"environments":["default","docker"],"errors":[]}`,
		204, ""},
	{"heartbeat", func(ctx context.Context, c *Client) (any, error) { return nil, c.Heartbeat(ctx) },
		"POST", "/api/runner/v1/heartbeat", "", "", 204, ""},
	{"createAgentToken", func(ctx context.Context, c *Client) (any, error) {
		return c.CreateAgentToken(ctx, "CSR-9", KindOrchestrator, "Ada")
	}, "POST", "/api/runner/v1/runs/CSR-9/agent-token", "", `{"kind":"orchestrator","agent":"Ada"}`, 201,
		`{"token":"` + agentToken + `","expires_at":"2026-11-05T12:00:00Z"}`},
	{"reportUsage", func(ctx context.Context, c *Client) (any, error) {
		return nil, c.ReportUsage(ctx, "CSR-9", []UsageEntry{
			{Issue: "CSR-9", Agent: "Ada", Model: ptr("m"), InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4, CostUSD: ptr(0.5)},
			{Issue: "CSR-10", Agent: "Qwen"},
		})
	}, "POST", "/api/runner/v1/runs/CSR-9/usage", "", `{"entries":[
		{"issue":"CSR-9","agent":"Ada","model":"m","input_tokens":1,"output_tokens":2,"cache_read_tokens":3,"cache_write_tokens":4,"cost_usd":0.5},
		{"issue":"CSR-10","agent":"Qwen","model":null,"input_tokens":0,"output_tokens":0,"cache_read_tokens":0,"cache_write_tokens":0,"cost_usd":null}]}`, 204, ""},
	{"deleteAgentTokens", func(ctx context.Context, c *Client) (any, error) { return nil, c.DeleteAgentTokens(ctx, "CSR-9") },
		"DELETE", "/api/runner/v1/runs/CSR-9/agent-token", "", "", 204, ""},
}

func ptr[T any](v T) *T { return &v }

func TestOperations(t *testing.T) {
	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			c, rec := server(t, op.status, op.resp, nil)
			got, err := op.call(context.Background(), c)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if rec.method != op.method || rec.path != op.path || rec.query != op.query {
				t.Errorf("request = %s %s ?%s, want %s %s ?%s", rec.method, rec.path, rec.query, op.method, op.path, op.query)
			}
			if v := rec.header.Get(name.Name + "-Runner-Version"); v != version {
				t.Errorf("runner version header = %q", v)
			}
			if a := rec.header.Get("Authorization"); a != "Bearer "+runnerToken {
				t.Errorf("Authorization = %q", a)
			}
			if op.req == "" {
				if rec.body != "" {
					t.Errorf("body = %q, want none", rec.body)
				}
			} else {
				if !sameJSON(t, rec.body, op.req) {
					t.Errorf("body = %s, want %s", rec.body, op.req)
				}
				if ct := rec.header.Get("Content-Type"); ct != "application/json" {
					t.Errorf("Content-Type = %q", ct)
				}
			}
			if op.resp != "" {
				b, err := json.Marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				if !sameJSON(t, string(b), op.resp) {
					t.Errorf("result = %s\nwant %s", b, op.resp)
				}
			}
			if c.APIVersion() != "1.0.0" {
				t.Errorf("APIVersion = %q", c.APIVersion())
			}
		})
	}
}

func TestWorkETag(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("If-None-Match"))
		w.Header().Set(name.Name+"-Api-Version", "1.0.0")
		w.Header().Set("ETag", `"a"`)
		if r.Header.Get("If-None-Match") == `"a"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = io.WriteString(w, `{"issue":null}`)
	}))
	defer srv.Close()
	c := New(srv.URL, runnerToken, version)

	iss, err := c.Work(context.Background())
	if iss != nil || err != nil {
		t.Fatalf("first Work = %v, %v; want nil, nil", iss, err)
	}
	if _, err := c.Work(context.Background()); !errors.Is(err, ErrNoChange) {
		t.Fatalf("second Work err = %v, want ErrNoChange", err)
	}
	if want := []string{"", `"a"`}; !reflect.DeepEqual(seen, want) {
		t.Errorf("If-None-Match = %q, want %q", seen, want)
	}
}

// A weak ETag goes back verbatim (CSR-113: weak forms match on the server).
func TestWorkWeakETagVerbatim(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("If-None-Match"))
		w.Header().Set(name.Name+"-Api-Version", "1.0.0")
		w.Header().Set("ETag", `W/"a-1"`)
		_, _ = io.WriteString(w, `{"issue":null}`)
	}))
	defer srv.Close()
	c := New(srv.URL, runnerToken, version)
	for range 2 {
		if _, err := c.Work(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"", `W/"a-1"`}; !reflect.DeepEqual(seen, want) {
		t.Errorf("If-None-Match = %q, want %q", seen, want)
	}
}

func TestUpgradeRequired(t *testing.T) {
	c, _ := server(t, 426, `{"message":"Upgrade the runner.","minimum_runner_version":"1.3.0"}`,
		map[string]string{name.Name + "-Api-Version": "1.4.0"})
	_, err := c.GetProject(context.Background())
	var up *UpgradeRequiredError
	if !errors.As(err, &up) || up.MinimumRunnerVersion != "1.3.0" {
		t.Fatalf("err = %v, want UpgradeRequiredError 1.3.0", err)
	}
	if c.APIVersion() != "1.4.0" {
		t.Errorf("APIVersion = %q, want 1.4.0", c.APIVersion())
	}
}

func TestConflict(t *testing.T) {
	calls := map[string]func(*Client) error{
		"claim": func(c *Client) error { _, err := c.ClaimIssue(context.Background(), "CSR-9", "todo"); return err },
		"update": func(c *Client) error {
			_, err := c.UpdateIssue(context.Background(), "CSR-9", UpdateIssueRequest{Status: "review", ExpectedStatus: "in_progress"})
			return err
		},
	}
	for n, call := range calls {
		c, _ := server(t, 409, `{"message":"Status changed.","status":"blocked"}`, nil)
		var conflict *ConflictError
		if err := call(c); !errors.As(err, &conflict) || conflict.Status != "blocked" {
			t.Errorf("%s: err = %v, want ConflictError blocked", n, err)
		}
	}
}

func TestAPIError(t *testing.T) {
	c, _ := server(t, 422, `{"message":"Invalid.","errors":{"status":["bad"]}}`, nil)
	_, err := c.ListIssues(context.Background(), "nope")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 422 || apiErr.Message != "Invalid." ||
		!reflect.DeepEqual(apiErr.Errors, map[string][]string{"status": {"bad"}}) {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(err.Error(), "listIssues") {
		t.Errorf("error %q does not name the operation", err)
	}
}

// DownloadAttachment streams the bytes to the file from the issue's own
// route; a file over the cap or an error status leaves no file (CSR-173).
func TestDownloadAttachment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spec.pdf")
	c, rec := server(t, 200, "%PDF", nil)
	if err := c.DownloadAttachment(context.Background(), "CSR-9.1", 5, path); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "%PDF" {
		t.Errorf("file = %q", got)
	}
	if rec.path != "/api/runner/v1/issues/CSR-9.1/attachments/5" || rec.header.Get("Authorization") != "Bearer "+runnerToken {
		t.Errorf("request %s, auth %q", rec.path, rec.header.Get("Authorization"))
	}

	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"too large": {200, strings.Repeat("x", MaxAttachmentBytes+1)},
		"not found": {404, `{"message":"Unknown attachment 5 on CSR-9."}`},
	} {
		c, _ := server(t, tc.status, tc.body, nil)
		path := filepath.Join(t.TempDir(), "a")
		if err := c.DownloadAttachment(context.Background(), "CSR-9", 5, path); err == nil {
			t.Errorf("%s: no error", name)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: file left behind: %v", name, err)
		}
	}
}

func TestWorkTimeout(t *testing.T) {
	d := New("https://x.example", runnerToken, version)
	if d.WorkTimeout <= WorkWait*time.Second || d.HTTP.Timeout != 0 {
		t.Fatalf("defaults cut the long-poll short: WorkTimeout %v, HTTP.Timeout %v", d.WorkTimeout, d.HTTP.Timeout)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := New(srv.URL, runnerToken, version)
	c.WorkTimeout, c.longPoll = 50*time.Millisecond, true
	if _, err := c.Work(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

// memClient returns a client whose requests h answers in memory, so a
// synctest bubble's fake clock can advance between them.
func memClient(h http.HandlerFunc) *Client {
	c := New("http://cinderline.test", runnerToken, version)
	c.HTTP = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		h(w, r)
		return w.Result(), nil
	})}
	c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return c
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// RunHeartbeats sends a heartbeat per wait and, after it, the report when
// it changed since the last one the server accepted (decision
// 2026-11-08-errors-reach-cinderline): a failed heartbeat is logged, and an
// unchanged report is not sent again. The wait is the interval the last
// heartbeat answer sent, else HeartbeatInterval, plus at most 3s of jitter
// (CSR-174); a failed heartbeat keeps the interval before it.
func TestRunHeartbeats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		var at []time.Time
		reports := 0
		// The answer to heartbeat n: its status and Poll-After header.
		answers := []struct {
			status int
			after  string
		}{{204, ""}, {204, "10"}, {500, "999"}, {204, "0"}, {204, ""}, {204, ""}}
		c := memClient(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "PUT" && r.URL.Path == "/api/runner/v1/report" {
				reports++
				w.WriteHeader(204)
				return
			}
			if r.Method != "POST" || r.URL.Path != "/api/runner/v1/heartbeat" {
				t.Errorf("request %s %s", r.Method, r.URL.Path)
			}
			a := answers[min(len(at), len(answers)-1)]
			at = append(at, time.Now())
			if a.after != "" {
				w.Header().Set(name.Name+"-Poll-After", a.after)
			}
			w.WriteHeader(a.status)
			_, _ = io.WriteString(w, `{"message":"down"}`)
		})
		c.Log = slog.New(slog.NewTextHandler(&buf, nil))
		ctx, cancel := context.WithCancel(context.Background())
		start := time.Now()
		go c.RunHeartbeats(ctx, Report{RunnerVersion: version, Clients: []ReportClient{}, Environments: []string{"default"}})
		time.Sleep(250 * time.Second)
		cancel()
		synctest.Wait()

		// The wait before each heartbeat: none sent yet (60s), none (60s),
		// 10s, 10s again after the failure, 0 clamped to 1s, none (60s).
		want := []time.Duration{60, 60, 10, 10, 1, 60}
		if len(at) < len(want) {
			t.Fatalf("heartbeats = %d, want at least %d", len(at), len(want))
		}
		prev := start
		for i, w := range want {
			gap := at[i].Sub(prev)
			if lo := w * time.Second; gap < lo || gap > lo+lo/4 || gap > lo+3*time.Second {
				t.Errorf("wait before heartbeat %d = %v, want %v plus jitter", i+1, gap, lo)
			}
			prev = at[i]
		}
		if reports != 1 {
			t.Errorf("reports = %d, want 1: the first sends it, unchanged ones do not", reports)
		}
		if got := strings.Count(buf.String(), "level=WARN"); got != 1 {
			t.Errorf("warn lines = %d, want 1:\n%s", got, buf.String())
		}
	})
}

// PollAfter keeps the interval of the last successful answer per request,
// clamped to 1s..1h, forgets it when an answer sends none or garbage, and is
// left alone by an error. ListReview's purpose keys review and merge apart,
// and Work's 304 counts.
func TestPollAfter(t *testing.T) {
	var status int
	var after string
	c := memClient(func(w http.ResponseWriter, r *http.Request) {
		if after != "" {
			w.Header().Set(name.Name+"-Poll-After", after)
		}
		if r.URL.Path == "/api/runner/v1/work" {
			w.Header().Set("ETag", `"e"`)
			if r.Header.Get("If-None-Match") != "" {
				w.WriteHeader(304)
				return
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"issue":null}`)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"issues":[]}`)
	})
	ctx := context.Background()
	step := func(what string, call func() error, set string, code int, p Poll, want time.Duration, wantOK bool) {
		t.Helper()
		after, status = set, code
		_ = call()
		if d, ok := c.PollAfter(p); d != want || ok != wantOK {
			t.Errorf("%s: PollAfter(%s) = %v, %v; want %v, %v", what, p, d, ok, want, wantOK)
		}
	}
	review := func() error { _, err := c.ListReview(ctx, PollReview); return err }
	merge := func() error { _, err := c.ListReview(ctx, PollMerge); return err }
	work := func() error { _, err := c.Work(ctx); return err }

	step("before any answer", func() error { return nil }, "", 200, PollReview, 0, false)
	step("sent", review, "30", 200, PollReview, 30*time.Second, true)
	step("other purpose untouched", review, "30", 200, PollMerge, 0, false)
	step("merge keyed apart", merge, "7", 200, PollMerge, 7*time.Second, true)
	step("review still", func() error { return nil }, "", 200, PollReview, 30*time.Second, true)
	step("error keeps it", review, "1", 500, PollReview, 30*time.Second, true)
	step("too small", review, "0", 200, PollReview, time.Second, true)
	step("negative", review, "-5", 200, PollReview, time.Second, true)
	step("too large", review, "99999999999999999", 200, PollReview, time.Hour, true)
	step("garbage", review, "soon", 200, PollReview, 0, false)
	step("sent again", review, "45", 200, PollReview, 45*time.Second, true)
	step("missing falls back", review, "", 200, PollReview, 0, false)
	step("work 200", work, "12", 200, PollWork, 12*time.Second, true)
	step("work 304", work, "20", 200, PollWork, 20*time.Second, true)
}

// ListReview adds the purpose to GET /issues?status=review.
func TestListReviewPurpose(t *testing.T) {
	var got string
	c := memClient(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"issues":[]}`)
	})
	if _, err := c.ListReview(context.Background(), PollMerge); err != nil {
		t.Fatal(err)
	}
	if got != "purpose=merge&status=review" {
		t.Fatalf("query = %q", got)
	}
}

// Jitter adds at most a quarter of d, and at most three seconds.
func TestJitter(t *testing.T) {
	for _, d := range []time.Duration{0, time.Millisecond, 4 * time.Second, time.Hour} {
		for range 100 {
			if j := Jitter(d); j < d || j > d+min(d/4, 3*time.Second) {
				t.Fatalf("Jitter(%v) = %v", d, j)
			}
		}
	}
}

func TestNoTokensInLogsOrErrors(t *testing.T) {
	leak := `{"message":"boom","detail":"` + runnerToken + agentToken + `"}`
	modes := []struct {
		name   string
		status func(operation) int
		body   func(operation) string
	}{
		{"ok", func(op operation) int { return op.status }, func(op operation) string { return op.resp }},
		{"error", func(operation) int { return 500 }, func(operation) string { return leak }},
		{"garbage", func(op operation) int { return op.status }, func(operation) string { return "not json " + agentToken }},
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for _, m := range modes {
		for _, op := range operations {
			c, _ := server(t, m.status(op), m.body(op), nil)
			c.Log = log
			_, err := op.call(context.Background(), c)
			if (m.name == "error" || m.name == "garbage" && op.status != 204) && err == nil {
				t.Errorf("%s %s: no error", m.name, op.name)
			}
			if err != nil && (strings.Contains(err.Error(), runnerToken) || strings.Contains(err.Error(), agentToken)) {
				t.Errorf("%s %s: error leaks a token: %v", m.name, op.name, err)
			}
		}
	}
	if s := buf.String(); strings.Contains(s, runnerToken) || strings.Contains(s, agentToken) {
		t.Errorf("log leaks a token:\n%s", s)
	}
	if !strings.Contains(buf.String(), "path=/api/runner/v1/runs/CSR-9/agent-token") {
		t.Errorf("log has no request lines:\n%s", buf.String())
	}
}

// GET /issues returns at most 200 rows: a full page logs one warning naming
// the status and is used as it is; a page below the cap logs nothing.
func TestListIssuesWarnsAtTheCap(t *testing.T) {
	for n, warn := range map[int]bool{199: false, 200: true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(name.Name+"-Api-Version", "1.0.0")
			_, _ = io.WriteString(w, `{"issues":[`+strings.Repeat(issueJSON+",", n-1)+issueJSON+`]}`)
		}))
		c := New(srv.URL, runnerToken, version)
		var buf bytes.Buffer
		c.Log = slog.New(slog.NewTextHandler(&buf, nil))
		is, err := c.ListIssues(context.Background(), "todo")
		srv.Close()
		if err != nil || len(is) != n {
			t.Fatalf("%d: ListIssues = %d issues, %v", n, len(is), err)
		}
		got := strings.Count(buf.String(), "more than 200 todo issues; only the first 200 are handled")
		if want := map[bool]int{false: 0, true: 1}[warn]; got != want {
			t.Errorf("%d issues: %d warnings, want %d; log:\n%s", n, got, want, buf.String())
		}
	}
}

// A runner token may not move a top-level issue to done (decision
// 2026-11-08-runner-tokens-without-done): the 403 comes back as an APIError
// with the server's message after one request, not retried.
func TestDoneForbidden(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"This token may not move issues to done."}`)
	}))
	defer srv.Close()
	c := New(srv.URL, runnerToken, version)
	_, err := c.UpdateIssue(context.Background(), "CSR-9", UpdateIssueRequest{Status: "done", ExpectedStatus: "review"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 403 || apiErr.Message != "This token may not move issues to done." {
		t.Fatalf("err = %v, want APIError 403 with the server's message", err)
	}
	if n != 1 {
		t.Errorf("%d requests, want 1 (no retry)", n)
	}
}

// TestRedactsWhatItPosts: an agent token Cinderline issues is registered, and
// comments, progress lines and report errors never carry it (CSR-157).
func TestRedactsWhatItPosts(t *testing.T) {
	const tok = "cl_agent_redacttest_0123456789"
	c, rec := server(t, 201, `{"token":"`+tok+`","expires_at":"2026-11-11T10:00:00Z"}`, nil)
	if _, err := c.CreateAgentToken(context.Background(), "CSR-9", KindOrchestrator, "Coder"); err != nil {
		t.Fatal(err)
	}
	_, _ = c.AddComment(context.Background(), "CSR-9", "output: "+tok)
	if strings.Contains(rec.body, tok) || !strings.Contains(rec.body, "[redacted orchestrator token]") {
		t.Errorf("comment body %s", rec.body)
	}
	_ = c.AddProgress(context.Background(), "CSR-9", "line "+tok)
	if strings.Contains(rec.body, tok) {
		t.Errorf("progress body %s", rec.body)
	}
	_ = c.PutReport(context.Background(), Report{Errors: []ReportError{{Kind: KindPollFailed, Message: "failed: " + tok}}})
	if strings.Contains(rec.body, tok) {
		t.Errorf("report body %s", rec.body)
	}
}

// CSR-97: GET project names the token's workspace (EMBER-159); a Cinderline
// from before workspaces sends none and gets the slug "default".
func TestProjectWorkspace(t *testing.T) {
	for body, want := range map[string]string{
		`{"key":"APP","workspace":{"id":7,"slug":"acme"}}`: "acme",
		`{"key":"APP"}`:                  DefaultWorkspace,
		`{"key":"APP","workspace":null}`: DefaultWorkspace,
	} {
		c, _ := server(t, 200, body, nil)
		p, err := c.GetProject(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got := p.WorkspaceSlug(); got != want {
			t.Errorf("%s: slug %q, want %q", body, got, want)
		}
	}
}

// Work sends wait only after the handshake offered long_poll, and stops when
// a later handshake withdraws it (CSR-93): by default the app answers at once.
func TestWorkLongPollsOnlyWhenOffered(t *testing.T) {
	var queries []string
	longPoll := ""
	c := memClient(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/runner/v1/project" {
			_, _ = io.WriteString(w, `{"key":"CSR"`+longPoll+`}`)
			return
		}
		queries = append(queries, r.URL.RawQuery)
		_, _ = io.WriteString(w, `{"issue":null}`)
	})
	ctx := context.Background()
	for _, lp := range []string{"", `,"long_poll":true`, `,"long_poll":false`} {
		longPoll = lp
		if _, err := c.GetProject(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Work(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"", "wait=25", ""}; !reflect.DeepEqual(queries, want) {
		t.Fatalf("work queries = %q, want %q", queries, want)
	}
}

// A Retry-After on any answer holds every request of the client until it
// has passed, in seconds or as an HTTP date, and is capped at an hour
// (CSR-93); an answer without one holds nothing.
func TestRetryAfterHoldsRequests(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header func(now time.Time) string
		status int
		want   time.Duration
	}{
		{"seconds on a 503", func(time.Time) string { return "30" }, 503, 30 * time.Second},
		{"seconds on a 429", func(time.Time) string { return "7" }, 429, 7 * time.Second},
		{"date", func(now time.Time) string { return now.Add(90 * time.Second).UTC().Format(http.TimeFormat) }, 503, 90 * time.Second},
		{"capped", func(time.Time) string { return "999999999999" }, 503, time.Hour},
		{"none", func(time.Time) string { return "" }, 503, 0},
		{"garbage", func(time.Time) string { return "soon" }, 503, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var at []time.Time
				c := memClient(func(w http.ResponseWriter, r *http.Request) {
					at = append(at, time.Now())
					if len(at) == 1 {
						if h := tc.header(time.Now()); h != "" {
							w.Header().Set("Retry-After", h)
						}
						w.WriteHeader(tc.status)
						return
					}
					w.WriteHeader(204)
				})
				ctx := context.Background()
				var api *APIError
				if err := c.Heartbeat(ctx); !errors.As(err, &api) || api.StatusCode != tc.status {
					t.Fatalf("first heartbeat: %v", err)
				}
				// Another endpoint waits too: the limit is the token's.
				if err := c.AddProgress(ctx, "CSR-9", "x"); err != nil {
					t.Fatal(err)
				}
				// HTTP dates have whole seconds, so allow one less.
				if gap := at[1].Sub(at[0]); gap < tc.want-time.Second || gap > tc.want {
					t.Fatalf("second request after %v, want %v", gap, tc.want)
				}
			})
		})
	}
}

// A held request gives up when its context is done, without sending.
func TestRetryAfterWaitEndsWithContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := 0
		c := memClient(func(w http.ResponseWriter, r *http.Request) {
			n++
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(503)
		})
		_ = c.Heartbeat(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := c.Work(ctx); !errors.Is(err, context.DeadlineExceeded) || n != 1 {
			t.Fatalf("err = %v after %d requests, want deadline exceeded after 1", err, n)
		}
	})
}

// Unavailable is true for what a deploy or an overload looks like, and
// false for answers that will not change by waiting.
func TestUnavailable(t *testing.T) {
	_, err := New("http://127.0.0.1:1", runnerToken, version).GetProject(context.Background())
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{err, true}, // connection refused
		{fmt.Errorf("getWork: %w", &APIError{StatusCode: 503}), true},
		{&APIError{StatusCode: 502}, true},
		{&APIError{StatusCode: 429}, true},
		{&APIError{StatusCode: 408}, true},
		{&APIError{StatusCode: 404}, false},
		{&APIError{StatusCode: 401}, false},
		{&ConflictError{}, false},
		{&UpgradeRequiredError{}, false},
		{errors.New("the issue has no team"), false},
		{nil, false},
	} {
		if got := Unavailable(tc.err); got != tc.want {
			t.Errorf("Unavailable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
