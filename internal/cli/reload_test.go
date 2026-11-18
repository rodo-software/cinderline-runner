package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/localapi"
)

// fakeRunner is a daemon on c's socket that runs issue (none when "") until
// it has been drained for a few polls, or forever when long, or until stopped.
type fakeRunner struct {
	mu                         sync.Mutex
	drained, stopped, reloaded bool
	polls                      int
}

func (f *fakeRunner) state() (drained, stopped, reloaded bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drained, f.stopped, f.reloaded
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
			if f.drained {
				if f.polls++; !long && f.polls > 2 {
					go srv.Close() // the run finished and the daemon exits
				}
			}
			return []localapi.ProjectStatus{{Project: "CLR", Connected: true, Issue: issue}}
		},
	})
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return f
}

// reloadCLI is daemonCLI with a fast drain poll, a project and a fake service.
func reloadCLI(t *testing.T, stdin string) (c *CLI, out *strings.Builder, started *int) {
	t.Helper()
	old := drainPoll
	drainPoll = 10 * time.Millisecond
	t.Cleanup(func() { drainPoll = old })
	c, _, _ = daemonCLI(t, stdin)
	saveConfig(t, c)
	started = new(int)
	c.startService = func(string) (bool, error) { *started++; return true, nil }
	out = &strings.Builder{}
	c.Stdout = out
	return c, out, started
}

// CLR-195: with no run in flight, a change reloads the runner as before.
func TestReloadWithoutRunsInFlight(t *testing.T) {
	c, _, started := reloadCLI(t, "")
	f := serveFakeRunner(t, c, "", false)
	if code := c.Run([]string{"strict-hosts", "on"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, c.Stderr)
	}
	if drained, stopped, reloaded := f.state(); !reloaded || drained || stopped {
		t.Errorf("reloaded %v, drained %v, stopped %v; want only a reload", reloaded, drained, stopped)
	}
	if *started != 0 {
		t.Errorf("service started %d times, want 0", *started)
	}
}

// CLR-195: with a run in flight, add-provider drains the runner instead of
// reloading it, waits until it has exited, and starts the service again.
func TestReloadDrainsRunsInFlight(t *testing.T) {
	c, out, started := reloadCLI(t, "\n") // no API key
	f := serveFakeRunner(t, c, "CLR-7", false)
	if code := c.Run([]string{"add-provider", "lan", "http://10.0.0.5:8000/v1"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, c.Stderr)
	}
	if drained, stopped, reloaded := f.state(); !drained || stopped || reloaded {
		t.Errorf("drained %v, stopped %v, reloaded %v; want only a drain", drained, stopped, reloaded)
	}
	if *started != 1 {
		t.Errorf("service started %d times, want 1", *started)
	}
	for _, want := range []string{"CLR-7", "press s"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q, want it to contain %q", out.String(), want)
		}
	}
}

// CLR-195: s, confirmed with y, stops the runner at once during the drain,
// which cuts the run; the service then starts again.
func TestReloadStopKeyConfirmed(t *testing.T) {
	c, out, started := reloadCLI(t, "sy")
	f := serveFakeRunner(t, c, "CLR-7", true)
	if code := c.Run([]string{"strict-hosts", "on"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, c.Stderr)
	}
	if _, stopped, _ := f.state(); !stopped {
		t.Error("the runner was not stopped")
	}
	if *started != 1 {
		t.Errorf("service started %d times, want 1", *started)
	}
	if !strings.Contains(out.String(), "[y/N]") {
		t.Errorf("stdout %q, want the confirmation", out.String())
	}
}

// CLR-195: s answered with anything but y keeps waiting for the drain.
func TestReloadStopKeyDeclined(t *testing.T) {
	c, _, started := reloadCLI(t, "s\n")
	f := serveFakeRunner(t, c, "CLR-7", false)
	if code := c.Run([]string{"strict-hosts", "on"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, c.Stderr)
	}
	if drained, stopped, _ := f.state(); !drained || stopped {
		t.Errorf("drained %v, stopped %v; want a drain only", drained, stopped)
	}
	if *started != 1 {
		t.Errorf("service started %d times, want 1", *started)
	}
}

// CLR-195: Ctrl-C, a byte on a raw terminal, ends the wait; the draining
// runner is not started again, and the change itself stays.
func TestReloadInterrupted(t *testing.T) {
	c, out, started := reloadCLI(t, "\x03")
	f := serveFakeRunner(t, c, "CLR-7", true)
	if code := c.Run([]string{"strict-hosts", "on"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, c.Stderr)
	}
	if _, stopped, _ := f.state(); stopped {
		t.Error("the runner was stopped")
	}
	if *started != 0 {
		t.Errorf("service started %d times, want 0", *started)
	}
	if !strings.Contains(out.String(), "not started again") {
		t.Errorf("stdout %q, want it to say the runner is not started again", out.String())
	}
}
