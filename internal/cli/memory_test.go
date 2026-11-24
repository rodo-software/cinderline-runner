package cli

import (
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
)

// CLR-215, decision 2026-11-24-agent-memory-limit: memory shows, sets and
// clears one project's memory limit for its agent containers, refuses a
// size under the minimum, and reloads the runner after each change.
func TestMemory(t *testing.T) {
	c, out, _ := reloadCLI(t, "")
	errb := &strings.Builder{}
	c.Stderr = errb
	web := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"}
	api := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "API", Repository: "https://git.example.com/o/api", Memory: "2g"}
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
	memory := func() (web, api string) {
		t.Helper()
		ps := projects(t, c)
		return ps[0].Memory, ps[1].Memory
	}
	run := func(args ...string) int {
		t.Helper()
		out.Reset()
		errb.Reset()
		return c.Run(append([]string{"memory"}, args...))
	}

	if code := run("show", "--project", "WEB"); code != 0 || !strings.Contains(out.String(), "4g of memory, the default") || !strings.Contains(out.String(), "mem_limit") {
		t.Errorf("show default: code %d, %q", code, out.String())
	}
	if code := run("show", "--project", "API"); code != 0 || !strings.Contains(out.String(), "2g") || strings.Contains(out.String(), "default") {
		t.Errorf("show set: code %d, %q", code, out.String())
	}
	if code := run("set", "8G", "--project", "WEB"); code != 0 || !reloaded() {
		t.Fatalf("set: code %d, stderr %q", code, errb.String())
	}
	if w, a := memory(); w != "8g" || a != "2g" {
		t.Errorf("after set: WEB %q, API %q", w, a)
	}
	for _, bad := range []string{"100m", "4GB", "4096", "0g", "1.5g", "lots"} {
		if code := run("set", bad, "--project", "WEB"); code != 1 || !strings.Contains(errb.String(), "256m") || reloaded() {
			t.Errorf("set %s: code %d, stderr %q; want 1 and the minimum", bad, code, errb.String())
		}
	}
	if w, _ := memory(); w != "8g" {
		t.Errorf("refused sets changed the setting: %q", w)
	}
	if code := run("clear", "--project", "WEB"); code != 0 || !reloaded() || !strings.Contains(out.String(), "4g") {
		t.Errorf("clear: code %d, %q, stderr %q", code, out.String(), errb.String())
	}
	if w, a := memory(); w != "" || a != "2g" {
		t.Errorf("after clear: WEB %q, API %q", w, a)
	}
	if code := run("clear", "--project", "WEB"); code != 0 || reloaded() || !strings.Contains(out.String(), "nothing changed") {
		t.Errorf("clear again: code %d, %q", code, out.String())
	}

	for _, usage := range [][]string{{}, {"show"}, {"set", "--project", "WEB"}, {"set", "-1g", "--project", "WEB"}, {"set", "8g"}, {"grow", "--project", "WEB"}, {"clear", "8g", "--project", "WEB"}} {
		if code := run(usage...); code != 2 {
			t.Errorf("memory %q: code %d, want 2", usage, code)
		}
	}
	if code := run("show", "--project", "NOPE"); code != 1 {
		t.Errorf("show unknown project: code %d, want 1", code)
	}
}
