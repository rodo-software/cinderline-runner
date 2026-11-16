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

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// app is a fake Cinderline: it answers every poll with the token's project
// and a job its runner cannot start (an agent on a client nobody runs), so
// each loop logs one line about it. polls are the polls' tokens and
// reports, in order.
type app struct {
	url   string
	mu    sync.Mutex
	polls []string
}

func (a *app) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.polls)
}

func newApp(t *testing.T) *app {
	a := &app{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "POST /api/runner/v2/poll" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Report json.RawMessage `json:"report"`
			Idle   bool            `json:"idle"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		a.mu.Lock()
		a.polls = append(a.polls, r.Header.Get("Authorization")+" "+string(req.Report))
		a.mu.Unlock()
		job := "null"
		if req.Idle {
			job = `{"issue":{"ref":"` + key + `-1","title":"T","body":""},"team":{"name":"Dev","max_minutes":10,"max_turns":50,"agents":[{"name":"X","client":"nope"}]}}`
		}
		io.WriteString(w, `{"poll_after":30,"review_after":300,"project":{"key":"`+key+`","repository_url":"https://git.example.com/`+key+`","forge":"forgejo","base_branch":"main","environment":"default","idle_minutes":20,"linked_projects":[]},"job":`+job+`,"stop":[]}`)
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
	return config.Project{URL: url, Key: key, Workspace: config.DefaultWorkspace, Repository: "https://git.example.com/" + key}
}

func writeConfig(t *testing.T, p config.Paths, projects ...config.Project) {
	t.Helper()
	if err := (&config.Config{Projects: projects}).Save(p.File); err != nil {
		t.Fatal(err)
	}
}

// setToken stores the two tokens `add` always stores together.
func setToken(t *testing.T, p config.Paths, pr config.Project) {
	t.Helper()
	for _, k := range []secrets.Kind{secrets.RunnerToken, secrets.GitToken} {
		if _, err := secrets.New(p.State).Set(pr, k, "tok-"+pr.Key); err != nil {
			t.Fatal(err)
		}
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
	// Generous: a loop shells out to the real docker before its first poll, which takes seconds on CI's Windows runners (CSR-154).
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func status(t *testing.T, c localapi.Client) map[string]localapi.ProjectStatus {
	t.Helper()
	s, err := c.Status(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != "1.2.3" {
		t.Errorf("version = %q", s.Version)
	}
	m := map[string]localapi.ProjectStatus{}
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

// Two projects with one key would share their Docker volumes, so only the
// first runs (decision 2026-11-11-audit-fixes).
func TestSecondProjectWithTheSameKeyDoesNotRun(t *testing.T) {
	keyring.MockInit()
	a, b := newApp(t), newApp(t)
	p := paths(t)
	first, second := project(a.url, "A"), project(b.url, "A")
	writeConfig(t, p, first, second)
	setToken(t, p, first)
	setToken(t, p, second)
	c, _ := start(t, p)
	waitFor(t, "A to poll", func() bool {
		s, err := c.Status(ctxT(t))
		return err == nil && len(s.Projects) == 2 && s.Projects[0].Connected
	})
	s, err := c.Status(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Projects[1]; got.Connected || !strings.Contains(got.LastError, "another project has key A") {
		t.Errorf("the second A = %+v, want not started with the reason", got)
	}
}

// CSR-97: one key in two workspaces on one URL are two projects: both run,
// each under its own name.
func TestSameKeyInTwoWorkspacesRuns(t *testing.T) {
	keyring.MockInit()
	a := newApp(t)
	p := paths(t)
	acme, beta := project(a.url, "A"), project(a.url, "A")
	acme.Workspace, beta.Workspace = "acme", "beta"
	writeConfig(t, p, acme, beta)
	setToken(t, p, acme)
	setToken(t, p, beta)
	c, _ := start(t, p)
	waitFor(t, "both to poll", func() bool { m := status(t, c); return m["acme-A"].Connected && m["beta-A"].Connected })
}

// Each project polls at start with its whole report.
func TestPollAtStart(t *testing.T) {
	keyring.MockInit()
	a := newApp(t)
	p := paths(t)
	pa := project(a.url, "A")
	writeConfig(t, p, pa)
	setToken(t, p, pa)
	start(t, p)
	waitFor(t, "a poll", func() bool { return a.count() > 0 })
	a.mu.Lock()
	defer a.mu.Unlock()
	// clients is left out: detection runs alongside the first poll and may land before it.
	auth, body, _ := strings.Cut(strings.TrimSpace(a.polls[0]), " {")
	var r map[string]json.RawMessage
	if err := json.Unmarshal([]byte("{"+body), &r); err != nil {
		t.Fatalf("report %s: %v", a.polls[0], err)
	}
	delete(r, "clients")
	got, _ := json.Marshal(r)
	if want := `{"environments":["default","docker"],"errors":[],"refused_connections":0,"runner_version":"1.2.3"}`; auth != "Bearer tok-A" || string(got) != want {
		t.Errorf("report = %s, want Bearer tok-A with %s", a.polls[0], want)
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
	waitFor(t, "the first poll", func() bool { return a.count() >= 1 })
	const line = "run did not start"

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
	// The tail: up to the first line about A-1, which the loop logged before we connected.
	for m := next(); m["msg"] != line; m = next() {
	}
	if err := c.Reload(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	// Followed: the new loop's line, logged after the stream was opened.
	for m := next(); m["msg"] != line; m = next() {
	}
	reloads := 0
	for _, m := range got {
		if m["msg"] == "reloading" {
			reloads++
		}
		if m["msg"] == line && (m["project"] != "A" || m["issue"] != "A-1") {
			t.Errorf("loop line without project and issue: %v", m)
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
	// Twice: the job it cannot start, and at once after that run.
	waitFor(t, "A to poll", func() bool { return a.count() == 2 })

	writeConfig(t, p, pa, pb)
	if err := c.Reload(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if _, ok := status(t, c)["B"]; !ok {
		t.Fatal("status does not list B after reload")
	}
	// A's new loop polls again, B's for the first time.
	waitFor(t, "both loops to poll", func() bool { return a.count() == 6 })
	raw, _ := os.ReadFile(p.Log)
	if n, m := strings.Count(string(raw), `"msg":"projects started"`), strings.Count(string(raw), `"msg":"projects stopped"`); n != 2 || m != 1 {
		t.Errorf("projects started %d and stopped %d times, want 2 and 1:\n%s", n, m, raw)
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

// CSR-44: POST /drain makes an idle daemon exit cleanly, like stop, once its
// loops have finished (the run package's tests cover a run in flight).
func TestDrain(t *testing.T) {
	keyring.MockInit()
	p := paths(t)
	writeConfig(t, p)
	c, done := start(t, p)
	if err := c.Drain(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return within a second of a drain with nothing in flight")
	}
	if _, err := c.Status(ctxT(t)); !errors.Is(err, localapi.ErrNotRunning) {
		t.Errorf("Status after drain = %v, want ErrNotRunning", err)
	}
}
