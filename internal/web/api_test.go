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
// lines (and event lines, whole) and its comment lines as they come; the stream ends with the test.
func stream(t *testing.T, h Handlers) (data, comments <-chan string) {
	t.Helper()
	return streamPath(t, h, "/api/status/stream")
}

// streamPath is stream of path.
func streamPath(t *testing.T, h Handlers, path string) (data, comments <-chan string) {
	t.Helper()
	dir := t.TempDir()
	tok := token(t, dir)
	srv := httptest.NewServer(Handler(dir, port, h))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequestWithContext(t.Context(), "GET", srv.URL+path, nil)
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
			case strings.HasPrefix(line, "event: "): // as is, so a test tells it from data
				d <- line
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

// following is a log of tail whose new lines a test sends; n is the tail
// length the route asked for.
type following struct {
	tail  string
	lines chan []byte
	n     chan int
}

func newFollowing(tail string) *following {
	return &following{tail: tail, lines: make(chan []byte, 10), n: make(chan int, 1)}
}

func (f *following) follow(n int) ([]byte, <-chan []byte, func(), error) {
	f.n <- n
	return []byte(f.tail), f.lines, func() {}, nil
}

func TestLogSendsTheTailThenNewLines(t *testing.T) {
	f := newFollowing(`{"msg":"one"}` + "\n" + `{"msg":"two"}` + "\n")
	data, _ := streamPath(t, Handlers{Follow: f.follow}, "/api/log?tail=2")
	if n := <-f.n; n != 2 {
		t.Errorf("followed %d lines, want 2", n)
	}
	for _, want := range []string{`{"msg":"one"}`, `{"msg":"two"}`} {
		if got := next(t, data, "tail line"); got != want {
			t.Errorf("tail event %s, want %s", got, want)
		}
	}
	f.lines <- []byte(`{"msg":"three"}` + "\n")
	if got := next(t, data, "new line"); got != `{"msg":"three"}` {
		t.Errorf("new event %s, want {\"msg\":\"three\"}", got)
	}
}

func TestLogTailDefaultsTo200(t *testing.T) {
	f := newFollowing("")
	streamPath(t, Handlers{Follow: f.follow}, "/api/log")
	if n := <-f.n; n != 200 {
		t.Errorf("followed %d lines, want 200", n)
	}
}

func TestLogRefusesABadTail(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	f := newFollowing("")
	for _, tail := range []string{"x", "-1", "5001", "1.5"} {
		w := doTo(t, Handler(dir, port, Handlers{Follow: f.follow}), "GET", "/api/log?tail="+tail, tok, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("tail=%q: %d, want 400", tail, w.Code)
		}
	}
	streamPath(t, Handlers{Follow: f.follow}, "/api/log?tail=5000") // fails the test unless 200
	if n := <-f.n; n != 5000 {
		t.Errorf("followed %d lines, want 5000", n)
	}
}
