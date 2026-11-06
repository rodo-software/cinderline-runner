package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderstack-runner/internal/config"
	"github.com/rodo-software/cinderstack-runner/internal/localapi"
	"github.com/rodo-software/cinderstack-runner/internal/scheduler"
	"github.com/rodo-software/cinderstack-runner/internal/secrets"
)

// app is a fake CinderStack: the first /work answers an issue, a repeated one is
// held until the client goes away. polls counts the first ones, one per client.
type app struct {
	url   string
	mu    sync.Mutex
	polls int
}

func (a *app) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.polls
}

func newApp(t *testing.T) *app {
	a := &app{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch path := strings.TrimPrefix(r.URL.Path, "/api/runner/v1"); {
		case path == "/work" && r.Header.Get("If-None-Match") != "":
			<-r.Context().Done()
		case path == "/work":
			a.mu.Lock()
			a.polls++
			a.mu.Unlock()
			w.Header().Set("ETag", `"v1"`)
			io.WriteString(w, `{"issue":{"ref":"A-1","title":"T","status":{"value":"todo","label":"To do"}}}`)
		case path == "/issues": // recovery's list of In progress issues
			io.WriteString(w, `{"issues":[]}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	a.url = srv.URL
	return a
}

// paths returns paths in a short temp folder: not t.TempDir(), macOS limits socket paths to 104 bytes.
func paths(t *testing.T) config.Paths {
	dir, err := os.MkdirTemp("", "cs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return config.Paths{
		Dir:    dir,
		File:   filepath.Join(dir, "config.json"),
		Log:    filepath.Join(dir, "state", "log"),
		State:  filepath.Join(dir, "state"),
		Socket: filepath.Join(dir, "run", "sock"),
	}
}

func project(url, key string) config.Project {
	return config.Project{URL: url, Key: key, Repository: "https://git.example.com/" + key}
}

func writeConfig(t *testing.T, p config.Paths, projects ...config.Project) {
	t.Helper()
	if err := (&config.Config{Projects: projects}).Save(p.File); err != nil {
		t.Fatal(err)
	}
}

func setToken(t *testing.T, p config.Paths, pr config.Project) {
	t.Helper()
	if _, err := secrets.New(p.State).Set(pr, secrets.RunnerToken, "tok-"+pr.Key); err != nil {
		t.Fatal(err)
	}
}

// start runs the daemon until the test ends and waits for its socket.
func start(t *testing.T, p config.Paths) (localapi.Client, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done, exited := make(chan error, 1), make(chan struct{})
	go func() { done <- Run(ctx, Options{Paths: p, Version: "1.2.3"}); close(exited) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	c := localapi.Client{Socket: p.Socket}
	waitFor(t, "the socket", func() bool { _, err := c.Status(ctxT(t)); return err == nil })
	return c, done
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func status(t *testing.T, c localapi.Client) map[string]scheduler.ProjectStatus {
	t.Helper()
	s, err := c.Status(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != "1.2.3" {
		t.Errorf("version = %q", s.Version)
	}
	m := map[string]scheduler.ProjectStatus{}
	for _, p := range s.Projects {
		m[p.Project] = p
	}
	return m
}

func TestStatus(t *testing.T) {
	keyring.MockInit()
	a := newApp(t)
	p := paths(t)
	withToken, without := project(a.url, "A"), project(a.url, "B")
	writeConfig(t, p, withToken, without)
	setToken(t, p, withToken)
	c, _ := start(t, p)
	waitFor(t, "A to poll", func() bool { return status(t, c)["A"].Connected })
	m := status(t, c)
	if len(m) != 2 {
		t.Fatalf("status lists %d projects, want 2: %+v", len(m), m)
	}
	if a := m["A"]; a.LastPoll.IsZero() || a.LastError != "" {
		t.Errorf("A = %+v, want a last poll and no error", a)
	}
	if b := m["B"]; b.Connected || !strings.Contains(b.LastError, "not found") || b.LastErrorAt.IsZero() {
		t.Errorf("B = %+v, want not connected with its token error", b)
	}
	if raw, _ := os.ReadFile(p.Log); strings.Contains(string(raw), "tok-A") {
		t.Errorf("the token is in the log:\n%s", raw)
	}
}

func TestLogStream(t *testing.T) {
	keyring.MockInit()
	a := newApp(t)
	p := paths(t)
	pa := project(a.url, "A")
	writeConfig(t, p, pa)
	setToken(t, p, pa)
	c, _ := start(t, p)
	waitFor(t, "the first poll", func() bool { return a.count() == 1 })

	pr, pw := io.Pipe()
	defer pr.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { pw.CloseWithError(c.Logs(ctx, 100, pw)) }()
	lines := bufio.NewScanner(pr)
	var got []map[string]any
	next := func() map[string]any {
		t.Helper()
		if !lines.Scan() {
			t.Fatalf("stream ended: %v", lines.Err())
		}
		var m map[string]any
		if err := json.Unmarshal(lines.Bytes(), &m); err != nil {
			t.Fatalf("line %q: %v", lines.Bytes(), err)
		}
		got = append(got, m)
		return m
	}
	// The tail: up to the first skip line, which the scheduler logged before we connected.
	for m := next(); m["msg"] != "no run step yet, skipping"; m = next() {
	}
	if err := c.Reload(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	// Followed: the new scheduler's skip line, logged after the stream was opened.
	for m := next(); m["msg"] != "no run step yet, skipping"; m = next() {
	}
	reloads := 0
	for _, m := range got {
		if m["msg"] == "reloading" {
			reloads++
		}
		if m["msg"] == "no run step yet, skipping" && (m["project"] != "A" || m["issue"] != "A-1") {
			t.Errorf("scheduler line without project and issue: %v", m)
		}
	}
	if reloads != 1 {
		t.Errorf("got %d reload lines in the stream, want 1: %v", reloads, got)
	}
}

func TestStop(t *testing.T) {
	keyring.MockInit()
	p := paths(t)
	writeConfig(t, p)
	c, done := start(t, p)
	if err := c.Stop(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return within a second of stop")
	}
	if _, err := os.Stat(p.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
	if _, err := c.Status(ctxT(t)); !errors.Is(err, localapi.ErrNotRunning) {
		t.Errorf("Status after stop = %v, want ErrNotRunning", err)
	}
}

func TestReload(t *testing.T) {
	keyring.MockInit()
	a := newApp(t)
	p := paths(t)
	pa, pb := project(a.url, "A"), project(a.url, "B")
	writeConfig(t, p, pa)
	setToken(t, p, pa)
	setToken(t, p, pb)
	c, _ := start(t, p)
	waitFor(t, "A to poll", func() bool { return a.count() == 1 })

	writeConfig(t, p, pa, pb)
	if err := c.Reload(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if _, ok := status(t, c)["B"]; !ok {
		t.Fatal("status does not list B after reload")
	}
	// A's new client polls without an ETag, B's for the first time.
	waitFor(t, "both slots to poll", func() bool { return a.count() == 3 })
	raw, _ := os.ReadFile(p.Log)
	if n, m := strings.Count(string(raw), `"msg":"scheduler started"`), strings.Count(string(raw), `"msg":"scheduler stopped"`); n != 2 || m != 1 {
		t.Errorf("scheduler started %d and stopped %d times, want 2 and 1:\n%s", n, m, raw)
	}

	if err := os.WriteFile(p.File, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(ctxT(t)); err == nil {
		t.Fatal("Reload of a broken config gave no error")
	}
	if m := status(t, c); len(m) != 2 {
		t.Errorf("after a failed reload status = %+v, want the old two projects", m)
	}
}

func TestHeld(t *testing.T) {
	keyring.MockInit()
	p := paths(t)
	writeConfig(t, p)
	c, _ := start(t, p)
	err := Run(context.Background(), Options{Paths: p, Version: "x"})
	if !errors.Is(err, localapi.ErrHeld) || !strings.Contains(err.Error(), p.Socket) {
		t.Fatalf("second Run = %v, want ErrHeld naming %s", err, p.Socket)
	}
	if _, err := c.Status(ctxT(t)); err != nil {
		t.Fatalf("first daemon stopped serving: %v", err)
	}
}

func TestStaleSocket(t *testing.T) {
	keyring.MockInit()
	p := paths(t)
	writeConfig(t, p)
	if err := os.MkdirAll(filepath.Dir(p.Socket), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", p.Socket)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if _, err := os.Stat(p.Socket); err != nil {
		t.Fatalf("no stale socket file: %v", err)
	}
	if _, err := (localapi.Client{Socket: p.Socket}).Status(ctxT(t)); !errors.Is(err, localapi.ErrNotRunning) {
		t.Errorf("Status on a stale socket = %v, want ErrNotRunning", err)
	}
	start(t, p)
}

func TestNotRunning(t *testing.T) {
	c := localapi.Client{Socket: filepath.Join(paths(t).Dir, "none")}
	ctx := ctxT(t)
	for name, err := range map[string]error{
		"status": func() error { _, err := c.Status(ctx); return err }(),
		"logs":   c.Logs(ctx, 1, io.Discard),
		"stop":   c.Stop(ctx),
		"reload": c.Reload(ctx),
	} {
		if !errors.Is(err, localapi.ErrNotRunning) {
			t.Errorf("%s = %v, want ErrNotRunning", name, err)
		}
	}
}
