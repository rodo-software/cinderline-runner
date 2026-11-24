package daemon

import (
	"context"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/run"
)

// fakeLoops replaces the project loop: a loop held with hold has a run in
// flight until it is released, and then, like every loop, runs until it is
// drained or stopped. A stop during the run cuts it.
type fakeLoops struct {
	mu     sync.Mutex
	starts map[string][][]string    // by project key: each loop's environment allowlist
	held   map[string]chan struct{} // by project key: the next loop's run ends when it is closed
	cut    []string                 // the projects whose run a stop cut
}

func useFakeLoops(t *testing.T) *fakeLoops {
	t.Helper()
	f := &fakeLoops{starts: map[string][][]string{}, held: map[string]chan struct{}{}}
	old := runLoop
	runLoop = func(p *run.Project, ctx context.Context, _ *cinderline.Client, drain <-chan struct{}) {
		key := p.Config.Key
		f.mu.Lock()
		f.starts[key] = append(f.starts[key], p.Env.(*environment.Docker).Allow)
		release := f.held[key]
		delete(f.held, key)
		f.mu.Unlock()
		if release != nil {
			select {
			case <-release:
			case <-ctx.Done():
				f.mu.Lock()
				f.cut = append(f.cut, key)
				f.mu.Unlock()
				return
			}
		}
		select {
		case <-ctx.Done():
		case <-drain:
		}
	}
	t.Cleanup(func() { runLoop = old }) // after the daemon's own cleanup, which stops it
	return f
}

// hold gives the next loop of project key a run in flight; closing the channel ends it.
func (f *fakeLoops) hold(key string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan struct{})
	f.held[key] = ch
	return ch
}

// of is the allowlists of project key's loops so far, in order.
func (f *fakeLoops) of(key string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.starts[key])
}

func (f *fakeLoops) cuts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.cut)
}

func saveConfig(t *testing.T, p config.Paths, allow []string, projects ...config.Project) {
	t.Helper()
	if err := (&config.Config{AllowHosts: allow, Projects: projects}).Save(p.File); err != nil {
		t.Fatal(err)
	}
}

// rolling sets up projects with tokens on a fake loop, A with a run in flight.
func rolling(t *testing.T, keys ...string) (*fakeLoops, config.Paths, []config.Project, chan struct{}) {
	t.Helper()
	keyring.MockInit()
	f := useFakeLoops(t)
	p := paths(t)
	var projects []config.Project
	for _, k := range keys {
		pr := project("https://"+k+".example.com", k)
		setToken(t, p, pr)
		projects = append(projects, pr)
	}
	return f, p, projects, f.hold("A")
}

func last(l [][]string) []string { return l[len(l)-1] }

// CLR-213: a reload restarts an idle project at once with the new config,
// lets a busy one finish its run uncut and then restarts it, starts an
// added one, and the process keeps running.
func TestRollingReload(t *testing.T) {
	f, p, pr, release := rolling(t, "A", "B", "C")
	saveConfig(t, p, nil, pr[0], pr[1])
	c, done := start(t, p)
	waitFor(t, "A and B to start", func() bool { return len(f.of("A")) == 1 && len(f.of("B")) == 1 })

	const host = "new.example.com:443"
	saveConfig(t, p, []string{host}, pr...)
	if err := c.Reload(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "B to restart and C to start", func() bool { return len(f.of("B")) == 2 && len(f.of("C")) == 1 })
	for _, k := range []string{"B", "C"} {
		if got := last(f.of(k)); !slices.Contains(got, host) {
			t.Errorf("%s's allowlist %q, want %s", k, got, host)
		}
	}
	m := status(t, c)
	if !m["A"].Draining || m["B"].Draining || m["C"].Draining {
		t.Errorf("status %+v, want only A draining", m)
	}
	if n := len(f.of("A")); n != 1 {
		t.Fatalf("A started %d times while its run was in flight, want 1", n)
	}

	close(release)
	waitFor(t, "A to restart", func() bool { return len(f.of("A")) == 2 })
	if got := last(f.of("A")); !slices.Contains(got, host) {
		t.Errorf("A's allowlist after its run %q, want %s", got, host)
	}
	if cut := f.cuts(); len(cut) != 0 {
		t.Errorf("runs cut: %v", cut)
	}
	waitFor(t, "A not draining", func() bool { return !status(t, c)["A"].Draining })
	select {
	case err := <-done:
		t.Fatalf("Run returned %v after a reload", err)
	default:
	}
}

// CLR-213: two reloads during one run restart the project once, with the newer config.
func TestTwoReloadsDuringOneRun(t *testing.T) {
	f, p, pr, release := rolling(t, "A")
	saveConfig(t, p, nil, pr...)
	c, _ := start(t, p)
	waitFor(t, "A to start", func() bool { return len(f.of("A")) == 1 })
	for _, h := range []string{"one.example.com:443", "two.example.com:443"} {
		saveConfig(t, p, []string{h}, pr...)
		if err := c.Reload(ctxT(t)); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	waitFor(t, "A to restart", func() bool { return len(f.of("A")) >= 2 })
	waitFor(t, "A not draining", func() bool { return !status(t, c)["A"].Draining })
	got := f.of("A")
	if len(got) != 2 || !slices.Contains(got[1], "two.example.com:443") || slices.Contains(got[1], "one.example.com:443") {
		t.Errorf("A's loops %q, want two, the second with only the newest host", got)
	}
}

// CLR-213: a config that fails to load restarts nothing and reaches the command.
func TestRollingReloadBrokenConfig(t *testing.T) {
	f, p, pr, release := rolling(t, "A", "B")
	defer close(release)
	saveConfig(t, p, nil, pr...)
	c, _ := start(t, p)
	waitFor(t, "A and B to start", func() bool { return len(f.of("A")) == 1 && len(f.of("B")) == 1 })
	if err := os.WriteFile(p.File, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(ctxT(t)); err == nil {
		t.Fatal("Reload of a broken config gave no error")
	}
	time.Sleep(50 * time.Millisecond) // a restart would show by now
	if a, b := len(f.of("A")), len(f.of("B")); a != 1 || b != 1 {
		t.Errorf("A started %d and B %d times, want once each", a, b)
	}
	if m := status(t, c); len(m) != 2 || m["A"].Draining || m["B"].Draining {
		t.Errorf("status %+v, want the two projects, none draining", m)
	}
}

// CLR-213: a project the new config removes finishes its run, shows as
// draining meanwhile, and then ends without starting again.
func TestRollingReloadRemovedProject(t *testing.T) {
	f, p, pr, release := rolling(t, "A", "B")
	saveConfig(t, p, nil, pr...)
	c, _ := start(t, p)
	waitFor(t, "A and B to start", func() bool { return len(f.of("A")) == 1 && len(f.of("B")) == 1 })
	saveConfig(t, p, nil, pr[1])
	if err := c.Reload(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if m := status(t, c); len(m) != 2 || !m["A"].Draining {
		t.Errorf("status %+v, want B and A draining", m)
	}
	close(release)
	waitFor(t, "A to leave the status", func() bool { _, ok := status(t, c)["A"]; return !ok })
	if n := len(f.of("A")); n != 1 || len(f.cuts()) != 0 {
		t.Errorf("A started %d times, cut %v; want once and not cut", n, f.cuts())
	}
}

// CLR-213: a drain during a rolling reload waits for the run and ends the
// process without restarting the project; a stop cuts the run and ends it.
func TestDrainOrStopDuringRollingReload(t *testing.T) {
	for _, drain := range []bool{true, false} {
		f, p, pr, release := rolling(t, "A", "B")
		saveConfig(t, p, nil, pr...)
		c, done := start(t, p)
		waitFor(t, "A and B to start", func() bool { return len(f.of("A")) == 1 && len(f.of("B")) == 1 })
		saveConfig(t, p, []string{"new.example.com:443"}, pr...)
		if err := c.Reload(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "B to restart", func() bool { return len(f.of("B")) == 2 })
		if drain {
			if err := c.Drain(ctxT(t)); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				t.Fatalf("drain: Run returned %v while A's run was in flight", err)
			case <-time.After(50 * time.Millisecond):
			}
			close(release)
		} else if err := c.Stop(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("drain %v: Run = %v, want nil", drain, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("drain %v: Run did not return", drain)
		}
		if n := len(f.of("A")); n != 1 {
			t.Errorf("drain %v: A started %d times, want once", drain, n)
		}
		if cut := f.cuts(); drain != (len(cut) == 0) {
			t.Errorf("drain %v: runs cut %v", drain, cut)
		}
		if !drain {
			close(release)
		}
	}
}
