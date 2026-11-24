package cli

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
)

// fakeRunner is a daemon on c's socket whose project CLR runs issue (none
// when "") until a reload drains it: then it shows CLR draining for a few
// polls and then gone, or forever when long, or until stopped.
type fakeRunner struct {
	mu                         sync.Mutex
	drained, stopped, reloaded bool
	polls                      int
	finished                   bool // CLR's draining loop has ended
}

func (f *fakeRunner) state() (drained, stopped, reloaded bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drained, f.stopped, f.reloaded
}

func (f *fakeRunner) done() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finished || f.stopped
}

func serveFakeRunner(t *testing.T, c *CLI, issue string, long bool) *fakeRunner {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(c.Paths.Socket), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := localapi.Listen(c.Paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{}
	srv := &http.Server{}
	srv.Handler = localapi.Handler(localapi.Handlers{
		Drain:  func() { f.mu.Lock(); f.drained = true; f.mu.Unlock() },
		Stop:   func() { f.mu.Lock(); f.stopped = true; f.mu.Unlock(); go srv.Close() },
		Reload: func() error { f.mu.Lock(); f.reloaded = true; f.mu.Unlock(); return nil },
		Status: func() []localapi.ProjectStatus {
			f.mu.Lock()
			defer f.mu.Unlock()
			if !f.reloaded || issue == "" {
				return []localapi.ProjectStatus{{Project: "CLR", Connected: true, Issues: []string{issue}}}
			}
			if f.polls++; !long && f.polls > 3 {
				f.finished = true // the run finished and the loop ended
				return nil
			}
			return []localapi.ProjectStatus{{Project: "CLR", Connected: true, Issues: []string{issue}, Draining: true}}
		},
	})
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return f
}

// reloadCLI is daemonCLI with a fast drain poll, project CLR and a fake service.
func reloadCLI(t *testing.T, stdin string) (c *CLI, out *strings.Builder, started *int) {
	t.Helper()
	old := drainPoll
	drainPoll = 10 * time.Millisecond
	t.Cleanup(func() { drainPoll = old })
	c, _, _ = daemonCLI(t, stdin)
	saveConfig(t, c, config.Project{Workspace: "default", URL: "https://a.example.com", Key: "CLR", Repository: "https://git.example.com/o/clr"})
	started = new(int)
	c.startService = func(string) (bool, error) { *started++; return true, nil }
	out = &strings.Builder{}
	c.Stdout = out
	return c, out, started
}

// CLR-195: with no run in flight, a change reloads the runner as before.
func TestReloadWithoutRunsInFlight(t *testing.T) {
	c, out, started := reloadCLI(t, "")
	f := serveFakeRunner(t, c, "", false)
	if code := c.Run([]string{"strict-hosts", "on"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, c.Stderr)
	}
	if drained, stopped, reloaded := f.state(); !reloaded || drained || stopped {
		t.Errorf("reloaded %v, drained %v, stopped %v; want only a reload", reloaded, drained, stopped)
	}
	if *started != 0 || strings.Contains(out.String(), "old config") {
		t.Errorf("service started %d times, stdout %q; want neither", *started, out.String())
	}
}

// CLR-213: with a run in flight, a config change reloads the runner and
// returns at once, saying which issues still run on the old config; the
// runner is neither drained nor started again (decision 2026-11-24-rolling-reload).
func TestReloadWithRunsInFlightReturnsAtOnce(t *testing.T) {
	c, out, started := reloadCLI(t, "")
	f := serveFakeRunner(t, c, "CLR-7", true)
	if code := c.Run([]string{"allow-host", "add", "pypi.org:443", "--project", "CLR"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, c.Stderr)
	}
	if drained, stopped, reloaded := f.state(); !reloaded || drained || stopped {
		t.Errorf("reloaded %v, drained %v, stopped %v; want only a reload", reloaded, drained, stopped)
	}
	if *started != 0 {
		t.Errorf("service started %d times, want 0", *started)
	}
	if !strings.Contains(out.String(), "CLR-7 still run on the old config") {
		t.Errorf("stdout %q, want it to name CLR-7 on the old config", out.String())
	}
}

// removeWithRun runs `remove CLR` against a fake runner whose CLR runs
// CLR-7, and returns the Docker calls, each marked with whether CLR's loop
// had ended by then.
func removeWithRun(t *testing.T, stdin string, long bool) (f *fakeRunner, out *strings.Builder, started *int, calls []bool) {
	t.Helper()
	c, out, started := reloadCLI(t, stdin)
	f = serveFakeRunner(t, c, "CLR-7", long)
	c.exec = func(context.Context, []string, ...string) (string, error) {
		calls = append(calls, f.done())
		return "", nil
	}
	if code := c.Run([]string{"remove", "CLR"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, c.Stderr)
	}
	return f, out, started, calls
}

// CLR-213: remove waits until its own project's run has finished before it
// removes the project's Docker volumes; the runner is not drained.
func TestRemoveWaitsForItsProjectsRuns(t *testing.T) {
	f, out, started, calls := removeWithRun(t, "", false)
	if drained, stopped, _ := f.state(); drained || stopped {
		t.Errorf("drained %v, stopped %v; want neither", drained, stopped)
	}
	if len(calls) == 0 || slices.Contains(calls, false) {
		t.Errorf("docker calls, true once CLR's loop had ended: %v; want some, all after", calls)
	}
	if *started != 0 {
		t.Errorf("service started %d times, want 0", *started)
	}
	if !strings.Contains(out.String(), "Waiting for CLR-7") || !strings.Contains(out.String(), "press s") {
		t.Errorf("stdout %q, want the wait for CLR-7 with the s key", out.String())
	}
}

// CLR-195: s, confirmed with y, stops the runner at once during the wait,
// which cuts the run; the service then starts again and the volumes go.
func TestRemoveStopKeyConfirmed(t *testing.T) {
	f, out, started, calls := removeWithRun(t, "sy", true)
	if _, stopped, _ := f.state(); !stopped {
		t.Error("the runner was not stopped")
	}
	if *started != 1 {
		t.Errorf("service started %d times, want 1", *started)
	}
	if len(calls) == 0 {
		t.Error("the volumes were not removed")
	}
	if !strings.Contains(out.String(), "[y/N]") {
		t.Errorf("stdout %q, want the confirmation", out.String())
	}
}

// CLR-195: s answered with anything but y keeps waiting for the run.
func TestRemoveStopKeyDeclined(t *testing.T) {
	f, _, started, calls := removeWithRun(t, "s\n", false)
	if drained, stopped, _ := f.state(); drained || stopped {
		t.Errorf("drained %v, stopped %v; want neither", drained, stopped)
	}
	if *started != 0 || len(calls) == 0 || slices.Contains(calls, false) {
		t.Errorf("service started %d times, docker calls %v; want none, and the volumes removed after the run", *started, calls)
	}
}

// CLR-195: Ctrl-C, a byte on a raw terminal, ends the wait; the volumes
// stay, as the run still uses them, and the runner keeps running.
func TestRemoveInterrupted(t *testing.T) {
	f, out, started, calls := removeWithRun(t, "\x03", true)
	if _, stopped, _ := f.state(); stopped {
		t.Error("the runner was stopped")
	}
	if *started != 0 || len(calls) != 0 {
		t.Errorf("service started %d times, docker calls %v; want none", *started, calls)
	}
	if !strings.Contains(out.String(), "kept") {
		t.Errorf("stdout %q, want it to say the volumes are kept", out.String())
	}
}
