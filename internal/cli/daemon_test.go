package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/daemon"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/scheduler"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// daemonCLI is newCLI with a socket and log in a short temp folder: macOS limits socket paths to 104 bytes.
func daemonCLI(t *testing.T, stdin string) (*CLI, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	c, out, errb := newCLI(t, stdin)
	dir, err := os.MkdirTemp("", "cs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	c.Paths.Socket = filepath.Join(dir, "run", "sock")
	c.Paths.Log = filepath.Join(c.Paths.State, "log")
	c.processGone = func(int) bool { return true } // the daemon runs in this process, which never exits
	return c, out, errb
}

// serve serves h on c's socket until the test ends.
func serve(t *testing.T, c *CLI, h localapi.Handlers) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(c.Paths.Socket), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := localapi.Listen(c.Paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: localapi.Handler(h)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

// startDaemon runs daemon.Run on c's paths and waits for its socket.
func startDaemon(t *testing.T, c *CLI) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done, exited := make(chan error, 1), make(chan struct{})
	go func() { done <- daemon.Run(ctx, daemon.Options{Paths: c.Paths, Version: "1.2.3"}); close(exited) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			t.Error("daemon.Run did not return after cancel")
		}
	})
	waitFor(t, "the socket", func() bool {
		_, err := (localapi.Client{Socket: c.Paths.Socket}).Status(context.Background())
		return err == nil
	})
	return done
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

func TestRunRefusesHeldSocket(t *testing.T) {
	c, out, errb := daemonCLI(t, "")
	saveConfig(t, c)
	startDaemon(t, c)
	if code := c.Run([]string{"run"}); code != 1 {
		t.Fatalf("code %d, want 1; stdout %q", code, out.String())
	}
	if !strings.Contains(errb.String(), "holds the socket") {
		t.Errorf("stderr %q does not say the socket is held", errb.String())
	}
}

func TestStatusPrintsEachProject(t *testing.T) {
	c, out, errb := daemonCLI(t, "")
	at := time.Date(2026, 11, 6, 14, 30, 0, 0, time.Local)
	serve(t, c, localapi.Handlers{Version: "4.5.6", Status: func() []scheduler.ProjectStatus {
		return []scheduler.ProjectStatus{
			{Project: "WEB", Connected: true, LastPoll: at, Issue: "WEB-7"},
			{Project: "API", LastError: "token not found", LastErrorAt: at},
		}
	}})
	if code := c.Run([]string{"status"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(got) != 3 {
		t.Fatalf("want a version line and two project lines:\n%s", out.String())
	}
	if !strings.Contains(got[0], "4.5.6") {
		t.Errorf("line %q lacks the daemon version", got[0])
	}
	for _, s := range []string{"WEB", "connected", "last poll 2026-11-06 14:30:00", "working on WEB-7"} {
		if !strings.Contains(got[1], s) {
			t.Errorf("line %q lacks %q", got[1], s)
		}
	}
	for _, s := range []string{"API", "not connected", "never polled", "last error 2026-11-06 14:30:00: token not found"} {
		if !strings.Contains(got[2], s) {
			t.Errorf("line %q lacks %q", got[2], s)
		}
	}
	if strings.Contains(got[2], "working on") {
		t.Errorf("line %q names an issue", got[2])
	}
}

func TestStatusNoProjects(t *testing.T) {
	c, out, _ := daemonCLI(t, "")
	serve(t, c, localapi.Handlers{Version: "4.5.6", Status: func() []scheduler.ProjectStatus { return nil }})
	if code := c.Run([]string{"status"}); code != 0 || !strings.Contains(out.String(), "no projects") {
		t.Errorf("code %d, stdout %q", code, out.String())
	}
}

func TestLogsPrintsTailThenFollows(t *testing.T) {
	c, out, errb := daemonCLI(t, "")
	var mu sync.Mutex
	var asked []int
	serve(t, c, localapi.Handlers{Follow: func(n int) ([]byte, <-chan []byte, func(), error) {
		mu.Lock()
		asked = append(asked, n)
		mu.Unlock()
		lines := make(chan []byte, 2)
		lines <- []byte("new 1\n")
		lines <- []byte("new 2\n")
		close(lines) // the daemon dropped us: the stream ends and logs returns
		return []byte("old 1\nold 2\n"), lines, func() {}, nil
	}})
	if code := c.Run([]string{"logs"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if want := "old 1\nold 2\nnew 1\nnew 2\n"; out.String() != want {
		t.Errorf("stdout %q, want %q", out.String(), want)
	}
	if code := c.Run([]string{"logs", "-n", "5"}); code != 0 {
		t.Fatalf("-n 5: code %d, stderr %q", code, errb.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 2 || asked[0] != 100 || asked[1] != 5 {
		t.Errorf("tails asked %v, want [100 5]", asked)
	}
}

func TestStopEndsRunningDaemon(t *testing.T) {
	c, _, errb := daemonCLI(t, "")
	saveConfig(t, c)
	done := startDaemon(t, c)
	if code := c.Run([]string{"stop"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon.Run = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon did not stop within a second")
	}
}

func TestNotRunning(t *testing.T) {
	for _, args := range [][]string{{"status"}, {"logs"}, {"stop"}} {
		c, out, errb := daemonCLI(t, "")
		if code := c.Run(args); code != 1 {
			t.Errorf("%v: code %d, want 1", args, code)
		}
		if !strings.Contains(errb.String(), "not running") {
			t.Errorf("%v: stderr %q lacks not running", args, errb.String())
		}
		if out.Len() != 0 {
			t.Errorf("%v: stdout %q", args, out.String())
		}
	}
}

// app is a fake Cinderline for the M2 check: it knows goodToken's project
// WEB, has no work, and counts the reports and heartbeats it gets.
type app struct {
	url                 string
	mu                  sync.Mutex
	reports, heartbeats int
	report              string
}

func (a *app) counts() (reports, heartbeats int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reports, a.heartbeats
}

func newApp(t *testing.T) *app {
	a := &app{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+goodToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + strings.TrimPrefix(r.URL.Path, "/api/runner/v1") {
		case "GET /project":
			io.WriteString(w, `{"key":"WEB","name":"Web shop","repository_url":"`+repoURL+`","forge":"forgejo","base_branch":"main","environment":"laravel"}`)
		case "GET /work":
			if r.Header.Get("If-None-Match") != "" {
				<-r.Context().Done() // nothing changed: hold the long-poll
				return
			}
			w.Header().Set("ETag", `"none"`)
			io.WriteString(w, `{"issue":null}`)
		case "GET /issues":
			io.WriteString(w, `{"issues":[]}`)
		case "PUT /report":
			body, _ := io.ReadAll(r.Body)
			a.mu.Lock()
			a.reports++
			a.report = string(body)
			a.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case "POST /heartbeat":
			a.mu.Lock()
			a.heartbeats++
			a.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	a.url = srv.URL
	return a
}

// TestM2AddRunStatus is the M2 check against fakes: add a project, run the
// daemon, see it report, heartbeat and find no work, and see that in status.
func TestM2AddRunStatus(t *testing.T) {
	// daemon startup cleanup shells out to docker; empty PATH keeps it off the host's real Docker
	t.Setenv("PATH", t.TempDir())
	a := newApp(t)
	c, out, errb := daemonCLI(t, lines(a.url, goodToken, "", "git-tok"))
	if code := c.Run([]string{"add"}); code != 0 {
		t.Fatalf("add: code %d, stderr %q", code, errb.String())
	}

	ran := make(chan int, 1)
	go func() { ran <- c.Run([]string{"run"}) }()
	sc := localapi.Client{Socket: c.Paths.Socket}
	waitFor(t, "WEB to poll", func() bool {
		s, err := sc.Status(context.Background())
		return err == nil && len(s.Projects) == 1 && s.Projects[0].Connected
	})
	waitFor(t, "a report and a heartbeat", func() bool { r, h := a.counts(); return r > 0 && h > 0 })
	a.mu.Lock()
	if !strings.Contains(a.report, `"runner_version":"1.2.3"`) {
		t.Errorf("report %s lacks the runner version", a.report)
	}
	a.mu.Unlock()

	out.Reset()
	if code := c.Run([]string{"status"}); code != 0 {
		t.Fatalf("status: code %d, stderr %q", code, errb.String())
	}
	if s := out.String(); !strings.Contains(s, "WEB: connected, last poll") || strings.Contains(s, "working on") || strings.Contains(s, "error") {
		t.Errorf("status shows %q, want WEB connected with a poll, no issue and no error", s)
	}

	if code := c.Run([]string{"stop"}); code != 0 {
		t.Fatalf("stop: code %d, stderr %q", code, errb.String())
	}
	select {
	case code := <-ran:
		if code != 0 {
			t.Errorf("run: code %d, stderr %q", code, errb.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after stop")
	}
}

// CSR-44: stop --drain asks the daemon to drain and waits until it has exited.
func TestStopDrainWaitsForDaemonToExit(t *testing.T) {
	defer func(d time.Duration) { drainPoll = d }(drainPoll)
	drainPoll = 10 * time.Millisecond
	c, out, errb := daemonCLI(t, "")
	saveConfig(t, c)
	done := startDaemon(t, c)
	if code := c.Run([]string{"stop", "--drain"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon.Run = %v, want nil", err)
		}
	case <-time.After(time.Second): // the socket closes in Run's defers, a moment before Run returns
		t.Fatal("stop --drain returned before the daemon exited")
	}
	if !strings.Contains(out.String(), "Stopped") {
		t.Errorf("stdout %q, want it to say the daemon stopped", out.String())
	}
}

// CSR-44: while draining, stop --drain says which issues it waits for, and
// returns once the daemon has gone.
func TestStopDrainNamesIssuesInFlight(t *testing.T) {
	defer func(d time.Duration) { drainPoll = d }(drainPoll)
	drainPoll = 10 * time.Millisecond
	c, out, errb := daemonCLI(t, "")
	if err := os.MkdirAll(filepath.Dir(c.Paths.Socket), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := localapi.Listen(c.Paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	drained, polls := false, 0
	srv := &http.Server{}
	srv.Handler = localapi.Handler(localapi.Handlers{
		Drain: func() { mu.Lock(); drained = true; mu.Unlock() },
		Status: func() []scheduler.ProjectStatus {
			mu.Lock()
			defer mu.Unlock()
			if polls++; drained && polls > 2 {
				go srv.Close() // the run finished and the daemon exits
			}
			return []scheduler.ProjectStatus{{Project: "CSR", Connected: true, Issue: "CSR-7"}}
		},
	})
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	if code := c.Run([]string{"stop", "--drain"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "CSR-7") {
		t.Errorf("stdout %q, want it to name CSR-7", out.String())
	}
}

// syncBuffer is a bytes.Buffer the daemon's goroutines can write while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// CSR-152: run -v also logs to stderr as text, and both outputs log at Debug; no secret is in any line.
func TestRunVerbose(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // as in TestM2AddRunStatus: keep startup cleanup off the host's Docker
	const modelKey = "sk-ant-model-secret"
	for _, verbose := range []bool{true, false} {
		t.Run(fmt.Sprint("verbose=", verbose), func(t *testing.T) {
			a := newApp(t)
			c, _, errb := daemonCLI(t, lines(a.url, goodToken, "", "git-tok-secret"))
			if code := c.Run([]string{"add"}); code != 0 {
				t.Fatalf("add: code %d, stderr %q", code, errb.String())
			}
			cfg, err := c.loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Secrets.Set(cfg.Projects[0], secrets.AnthropicCredential, modelKey); err != nil {
				t.Fatal(err)
			}
			stderr := &syncBuffer{}
			c.Stderr = stderr

			args := []string{"run"}
			if verbose {
				args = append(args, "-v")
			}
			ran := make(chan int, 1)
			go func() { ran <- c.Run(args) }()
			waitFor(t, "a report and a heartbeat", func() bool { r, h := a.counts(); return r > 0 && h > 0 })
			if code := c.Run([]string{"stop"}); code != 0 {
				t.Fatalf("stop: code %d", code)
			}
			select {
			case code := <-ran:
				if code != 0 {
					t.Fatalf("run: code %d, stderr %q", code, stderr.String())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("run did not return after stop")
			}

			raw, err := os.ReadFile(c.Paths.Log)
			if err != nil {
				t.Fatal(err)
			}
			file, term := string(raw), stderr.String()
			if verbose {
				if !strings.Contains(term, "level=DEBUG") || !strings.Contains(term, `msg="runner api request"`) {
					t.Errorf("stderr has no Debug request line:\n%s", term)
				}
				if !strings.Contains(term, "msg=started") {
					t.Errorf("stderr lacks the Info lines:\n%s", term)
				}
				if !strings.Contains(file, `"level":"DEBUG","msg":"runner api request"`) {
					t.Errorf("log file has no Debug request line:\n%s", file)
				}
			} else {
				if term != "" {
					t.Errorf("stderr = %q, want empty", term)
				}
				if strings.Contains(file, `"level":"DEBUG"`) {
					t.Errorf("log file has Debug lines without -v:\n%s", file)
				}
			}
			for _, secret := range []string{goodToken, "git-tok-secret", modelKey} {
				if strings.Contains(term, secret) || strings.Contains(file, secret) {
					t.Errorf("%q is in the log:\nstderr:\n%s\nfile:\n%s", secret, term, file)
				}
			}
		})
	}
}
