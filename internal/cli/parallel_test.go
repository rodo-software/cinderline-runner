package cli

import (
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
)

// CLR-249, decision 2026-11-25-parallel-runs-default: parallel shows, sets
// and clears the runner-wide max_parallel_runs, or one project's override
// with --project, refuses values outside 1 to 10, and reloads the runner
// after each change.
func TestParallel(t *testing.T) {
	c, out, _ := reloadCLI(t, "")
	errb := &strings.Builder{}
	c.Stderr = errb
	web := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"}
	api := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "API", Repository: "https://git.example.com/o/api", MaxParallelRuns: 4}
	saveConfig(t, c, web, api)
	f := serveFakeRunner(t, c, "", false)
	reloaded := func() bool {
		t.Helper()
		f.mu.Lock()
		defer f.mu.Unlock()
		r := f.reloaded
		f.reloaded = false
		return r
	}
	values := func() (runner, web, api int) {
		t.Helper()
		cfg, err := config.Load(c.Paths.File)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.MaxParallelRuns, cfg.Projects[0].MaxParallelRuns, cfg.Projects[1].MaxParallelRuns
	}
	run := func(args ...string) int {
		t.Helper()
		out.Reset()
		errb.Reset()
		return c.Run(append([]string{"parallel"}, args...))
	}
	shows := func(args []string, want ...string) {
		t.Helper()
		if code := run(args...); code != 0 {
			t.Errorf("%q: code %d, stderr %q", args, code, errb.String())
		}
		for _, w := range want {
			if !strings.Contains(out.String(), w) {
				t.Errorf("%q: %q, want %q in it", args, out.String(), w)
			}
		}
	}

	shows([]string{"show"}, "Runner-wide: up to 1 team run at once per project, the default", "WEB: 1, the default", "API: 4, its own")
	shows([]string{"show", "--project", "WEB"}, "WEB runs up to 1 team run at once, the default")
	shows([]string{"show", "--project", "API"}, "API runs up to 4 team runs at once, its own max_parallel_runs")

	if code := run("set", "3"); code != 0 || !reloaded() || !strings.Contains(out.String(), "once its runs in flight have finished") || !strings.Contains(out.String(), "API keeps its own") {
		t.Fatalf("set runner-wide: code %d, %q, stderr %q", code, out.String(), errb.String())
	}
	if r, w, a := values(); r != 3 || w != 0 || a != 4 {
		t.Errorf("after set runner-wide: %d, WEB %d, API %d", r, w, a)
	}
	shows([]string{"show"}, "Runner-wide: up to 3 team runs at once per project\n", "WEB: 3, the runner-wide value", "API: 4, its own")
	shows([]string{"show", "--project", "WEB"}, "WEB runs up to 3 team runs at once, the runner-wide value")

	if code := run("set", "2", "--project", "WEB"); code != 0 || !reloaded() || !strings.Contains(out.String(), "WEB runs up to 2 team runs at once, its own max_parallel_runs, once its runs in flight have finished") {
		t.Fatalf("set WEB: code %d, %q, stderr %q", code, out.String(), errb.String())
	}
	if r, w, a := values(); r != 3 || w != 2 || a != 4 {
		t.Errorf("after set WEB: %d, WEB %d, API %d", r, w, a)
	}

	for _, bad := range [][]string{{"set", "0"}, {"set", "11"}, {"set", "two"}, {"set", "1.5"}, {"set", "0", "--project", "WEB"}, {"set", "11", "--project", "WEB"}} {
		if code := run(bad...); code != 1 || !strings.Contains(errb.String(), "1 to 10") || reloaded() {
			t.Errorf("%q: code %d, stderr %q; want 1 and the range", bad, code, errb.String())
		}
	}
	if r, w, a := values(); r != 3 || w != 2 || a != 4 {
		t.Errorf("refused sets changed the values: %d, WEB %d, API %d", r, w, a)
	}

	if code := run("clear", "--project", "WEB"); code != 0 || !reloaded() || !strings.Contains(out.String(), "WEB runs up to 3 team runs at once, the runner-wide value") {
		t.Errorf("clear WEB: code %d, %q, stderr %q", code, out.String(), errb.String())
	}
	if r, w, _ := values(); r != 3 || w != 0 {
		t.Errorf("after clear WEB: %d, WEB %d", r, w)
	}
	if code := run("clear", "--project", "WEB"); code != 0 || reloaded() || !strings.Contains(out.String(), "nothing changed") {
		t.Errorf("clear WEB again: code %d, %q", code, out.String())
	}
	if code := run("clear"); code != 0 || !reloaded() || !strings.Contains(out.String(), "up to 1 team run at once, the default") {
		t.Errorf("clear runner-wide: code %d, %q, stderr %q", code, out.String(), errb.String())
	}
	if r, w, a := values(); r != 0 || w != 0 || a != 4 {
		t.Errorf("after clear runner-wide: %d, WEB %d, API %d", r, w, a)
	}
	if code := run("clear"); code != 0 || reloaded() || !strings.Contains(out.String(), "nothing changed") {
		t.Errorf("clear runner-wide again: code %d, %q", code, out.String())
	}

	for _, usage := range [][]string{{}, {"set"}, {"set", "--project", "WEB"}, {"set", "-1"}, {"grow"}, {"show", "3"}, {"clear", "3"}, {"show", "-url", "https://a.example.com"}} {
		if code := run(usage...); code != 2 {
			t.Errorf("parallel %q: code %d, want 2", usage, code)
		}
	}
	if code := run("show", "--project", "NOPE"); code != 1 {
		t.Errorf("show unknown project: code %d, want 1", code)
	}
}
