package cinderstack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderstack-runner/internal/name"
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
	"created_at":"2026-11-05T10:00:00Z","updated_at":"2026-11-05T11:00:00Z","completed_at":null,"archived_at":null}`

const commentJSON = `{"id":4,"body":"hi","author":{"name":"Runner","agent":null},"created_at":"2026-11-05T10:00:00Z"}`

var issueWithCommentsJSON = strings.TrimSuffix(issueJSON, "}") + `,"comments":[` + commentJSON + `]}`

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
		`{"key":"CSR","name":"Runner","repository_url":"https://git.example.com/r","forge":"forgejo","base_branch":"main","environment":"docker"}`},
	{"getWork", func(ctx context.Context, c *Client) (any, error) {
		iss, err := c.Work(ctx)
		return map[string]any{"issue": iss}, err
	}, "GET", "/api/runner/v1/work", "wait=25", "", 200, `{"issue":` + issueJSON + `}`},
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
		cs, err := c.ListComments(ctx, "CSR-9")
		return map[string]any{"comments": cs}, err
	}, "GET", "/api/runner/v1/issues/CSR-9/comments", "", "", 200, `{"comments":[` + commentJSON + `]}`},
	{"addComment", func(ctx context.Context, c *Client) (any, error) { return c.AddComment(ctx, "CSR-9", "hi") },
		"POST", "/api/runner/v1/issues/CSR-9/comments", "", `{"body":"hi"}`, 201, commentJSON},
	{"addProgress", func(ctx context.Context, c *Client) (any, error) { return nil, c.AddProgress(ctx, "CSR-9", "step 2") },
		"POST", "/api/runner/v1/issues/CSR-9/progress", "", `{"body":"step 2"}`, 204, ""},
	{"getTeam", func(ctx context.Context, c *Client) (any, error) { return c.GetTeam(ctx, 2) },
		"GET", "/api/runner/v1/teams/2", "", "", 200,
		`{"id":2,"name":"Dev","max_minutes":30,"trusted":true,"max_sub_issues":7,"ponytail":true,"agents":[{"name":"Ada","role":"orchestrator","client":"claude","model":null,"instructions":"be brief","description":null,"price":{"input":5,"output":25}},{"name":"Qwen","role":"worker","client":"opencode","model":"q","instructions":null,"description":"Cheap.","price":null}]}`},
	{"putReport", func(ctx context.Context, c *Client) (any, error) {
		v := "2.0.1"
		return nil, c.PutReport(ctx, Report{RunnerVersion: version, Clients: []ReportClient{
			{Name: "claude", Version: &v, State: "ready"}, {Name: "opencode", State: "missing_binary"},
		}, Environments: []string{"default", "docker"}})
	}, "PUT", "/api/runner/v1/report", "",
		`{"runner_version":"1.2.0","clients":[{"name":"claude","version":"2.0.1","state":"ready"},{"name":"opencode","version":null,"state":"missing_binary"}],"environments":["default","docker"]}`,
		204, ""},
	{"heartbeat", func(ctx context.Context, c *Client) (any, error) { return nil, c.Heartbeat(ctx) },
		"POST", "/api/runner/v1/heartbeat", "", "", 204, ""},
	{"createAgentToken", func(ctx context.Context, c *Client) (any, error) {
		return c.CreateAgentToken(ctx, "CSR-9", KindOrchestrator, "Ada")
	}, "POST", "/api/runner/v1/runs/CSR-9/agent-token", "", `{"kind":"orchestrator","agent":"Ada"}`, 201,
		`{"token":"` + agentToken + `","expires_at":"2026-11-05T12:00:00Z"}`},
	{"deleteAgentTokens", func(ctx context.Context, c *Client) (any, error) { return nil, c.DeleteAgentTokens(ctx, "CSR-9") },
		"DELETE", "/api/runner/v1/runs/CSR-9/agent-token", "", "", 204, ""},
}

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
	c.WorkTimeout = 50 * time.Millisecond
	if _, err := c.Work(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

func TestRunHeartbeats(t *testing.T) {
	hits := make(chan struct{}, 10)
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if r.Method != "POST" || r.URL.Path != "/api/runner/v1/heartbeat" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		if first {
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"message":"down"}`)
		} else {
			w.WriteHeader(204)
		}
		hits <- struct{}{}
	}))
	defer srv.Close()
	var buf bytes.Buffer
	c := New(srv.URL, runnerToken, version)
	c.Log = slog.New(slog.NewTextHandler(&buf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { c.RunHeartbeats(ctx, ticks); close(done) }()
	for range 3 {
		ticks <- time.Now()
		<-hits
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunHeartbeats did not stop after cancel")
	}
	if n != 3 {
		t.Errorf("heartbeats = %d, want 3", n)
	}
	if got := strings.Count(buf.String(), "level=WARN"); got != 1 {
		t.Errorf("warn lines = %d, want 1:\n%s", got, buf.String())
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
