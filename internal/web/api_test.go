package web

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

func TestAPIStatusAnswersTheStatus(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	st := localapi.Status{Version: "1.2.3", PID: 42, Projects: []localapi.ProjectStatus{{Project: "CLR", Connected: true}}, Memory: &localapi.Memory{Budget: 8, Reserved: 2}}
	w := doTo(t, Handler(dir, port, Handlers{Status: func() localapi.Status { return st }}), "GET", "/api/status", tok, nil)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("GET /api/status = %d, %q, want 200 application/json", w.Code, w.Header().Get("Content-Type"))
	}
	want, _ := json.Marshal(st)
	if got := strings.TrimSpace(w.Body.String()); got != string(want) {
		t.Errorf("body %s, want %s", got, want)
	}
}

// stream opens /api/status/stream on a server of h and returns its data
// lines and its comment lines as they come; the stream ends with the test.
func stream(t *testing.T, h Handlers) (data, comments <-chan string) {
	t.Helper()
	dir := t.TempDir()
	tok := token(t, dir)
	srv := httptest.NewServer(Handler(dir, port, h))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequestWithContext(t.Context(), "GET", srv.URL+"/api/status/stream", nil)
	req.Host = "127.0.0.1:7420"
	req.AddCookie(&http.Cookie{Name: name.Name, Value: tok})
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream = %d, %q, want 200 text/event-stream", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	d, c := make(chan string, 100), make(chan string, 100)
	go func() {
		s := bufio.NewScanner(resp.Body)
		for s.Scan() {
			switch line := s.Text(); {
			case strings.HasPrefix(line, "data: "):
				d <- strings.TrimPrefix(line, "data: ")
			case strings.HasPrefix(line, ":"):
				c <- line
			}
		}
	}()
	return d, c
}

// changing is a status whose version a test sets.
type changing struct {
	mu      sync.Mutex
	version string
}

func (c *changing) status() localapi.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return localapi.Status{Version: c.version}
}

func (c *changing) set(v string) {
	c.mu.Lock()
	c.version = v
	c.mu.Unlock()
}

func next(t *testing.T, ch <-chan string, what string) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(5 * time.Second):
		t.Fatalf("no %s", what)
		return ""
	}
}

func TestStreamSendsTheStatusAtOnce(t *testing.T) {
	c := &changing{version: "1"}
	data, _ := stream(t, Handlers{Status: c.status, Tick: time.Hour}) // no tick comes: the first event must not wait for one
	if got := next(t, data, "first event"); got != `{"version":"1","pid":0,"projects":null}` {
		t.Errorf("first event %s", got)
	}
}

func TestStreamSendsAChangeOnly(t *testing.T) {
	c := &changing{version: "1"}
	data, _ := stream(t, Handlers{Status: c.status, Tick: time.Millisecond})
	next(t, data, "first event")
	select {
	case got := <-data:
		t.Fatalf("event %s without a change", got)
	case <-time.After(50 * time.Millisecond): // fifty ticks
	}
	c.set("2")
	if got := next(t, data, "event after a change"); !strings.Contains(got, `"version":"2"`) {
		t.Errorf("event after a change %s, want version 2", got)
	}
}

func TestStreamPings(t *testing.T) {
	c := &changing{version: "1"}
	_, comments := stream(t, Handlers{Status: c.status, Tick: time.Millisecond})
	if got := next(t, comments, "ping"); got != ": ping" {
		t.Errorf("comment %q, want \": ping\"", got)
	}
}
