package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
)

// CLR-257: allow-skill adds, lists and removes a project's allow_skills
// entries by name and URL, in that project only, and reloads a running
// runner after each change.
func TestAllowSkill(t *testing.T) {
	c, out, _ := reloadCLI(t, "")
	errb := &strings.Builder{}
	c.Stderr = errb
	web := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"}
	api := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "API", Repository: "https://git.example.com/o/api", AllowSkills: []config.AllowedSkill{{Name: "api-only", URL: "https://github.com/o/a"}}}
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
	skills := func() (web, api []config.AllowedSkill) {
		t.Helper()
		ps := projects(t, c)
		return ps[0].AllowSkills, ps[1].AllowSkills
	}
	run := func(args ...string) int {
		t.Helper()
		out.Reset()
		errb.Reset()
		return c.Run(append([]string{"allow-skill"}, args...))
	}
	pony := config.AllowedSkill{Name: "ponytail", URL: "https://github.com/o/ponytail"}

	if code := run("list", "--project", "WEB"); code != 0 || !strings.Contains(out.String(), "No entries in the allow_skills of WEB") {
		t.Errorf("list empty: code %d, %q", code, out.String())
	}
	if code := run("add", "ponytail", "https://github.com/o/ponytail", "--project", "WEB"); code != 0 || !reloaded() {
		t.Fatalf("add: code %d, stderr %q", code, errb.String())
	}
	if w, a := skills(); !slices.Equal(w, []config.AllowedSkill{pony}) || len(a) != 1 {
		t.Fatalf("after add: WEB %v, API %v", w, a)
	}
	if code := run("add", "ponytail", "https://github.com/o/ponytail", "--project", "WEB"); code != 0 || !strings.Contains(out.String(), "already") || reloaded() {
		t.Errorf("duplicate add: code %d, %q", code, out.String())
	}
	for _, bad := range []struct{ name, url, want string }{
		{"Pony", "https://github.com/o/p", "not a skill package's name"},
		{"a--b", "https://github.com/o/p", "not a skill package's name"},
		{"p", "http://github.com/o/p", "https://host/path"},
		{"p", "https://u:tok@github.com/o/p", "https://host/path"},
		{"p", "https://github.com/o/p?ref=main", "https://host/path"},
	} {
		if code := run("add", bad.name, bad.url, "--project", "WEB"); code != 1 || !strings.Contains(errb.String(), bad.want) || reloaded() {
			t.Errorf("add %s %s: code %d, stderr %q; want 1 and %q", bad.name, bad.url, code, errb.String(), bad.want)
		}
		if strings.Contains(errb.String(), "u:tok") {
			t.Errorf("a password in the error: %q", errb.String())
		}
	}
	// One URL per name: a new URL replaces the old one, and says so.
	if code := run("add", "ponytail", "https://github.com/fork/ponytail", "--project", "WEB"); code != 0 || !strings.Contains(out.String(), "no longer from https://github.com/o/ponytail") || !reloaded() {
		t.Errorf("new URL: code %d, %q", code, out.String())
	}
	if code := run("add", "impeccable", "https://github.com/o/impeccable", "--project", "WEB"); code != 0 || !reloaded() {
		t.Fatalf("second add: code %d, stderr %q", code, errb.String())
	}
	if code := run("list", "--project", "WEB"); code != 0 || out.String() != "ponytail https://github.com/fork/ponytail\nimpeccable https://github.com/o/impeccable\n" {
		t.Errorf("list: code %d, %q", code, out.String())
	}
	if code := run("remove", "nope", "--project", "WEB"); code != 1 || !strings.Contains(errb.String(), "its entries: ponytail, impeccable") || reloaded() {
		t.Errorf("remove missing: code %d, stderr %q", code, errb.String())
	}
	if code := run("remove", "ponytail", "--project", "WEB"); code != 0 || !reloaded() {
		t.Fatalf("remove: code %d, stderr %q", code, errb.String())
	}
	if w, a := skills(); len(w) != 1 || w[0].Name != "impeccable" || len(a) != 1 {
		t.Errorf("after remove: WEB %v, API %v", w, a)
	}
}

// CLR-257: without --project, allow-skill manages the runner-wide entries
// and asks for a confirmation on add.
func TestAllowSkillRunnerWide(t *testing.T) {
	runnerSkills := func(t *testing.T, c *CLI) []config.AllowedSkill {
		t.Helper()
		cfg, err := config.Load(c.Paths.File)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.AllowSkills
	}
	c, out, _ := reloadCLI(t, "no\n")
	errb := &strings.Builder{}
	c.Stderr = errb
	f := serveFakeRunner(t, c, "", false)
	if code := c.Run([]string{"allow-skill", "add", "ponytail", "https://github.com/o/ponytail"}); code != 1 || !strings.Contains(out.String(), "all current and future projects") || len(runnerSkills(t, c)) != 0 {
		t.Errorf("cancelled add: code %d, %q", code, out.String())
	}
	if _, _, r := f.state(); r {
		t.Error("reloaded on cancel")
	}
	c.Stdin = strings.NewReader("yes\n")
	if code := c.Run([]string{"allow-skill", "add", "ponytail", "https://github.com/o/ponytail"}); code != 0 {
		t.Fatalf("add: code %d, stderr %q", code, errb.String())
	}
	if got := runnerSkills(t, c); !slices.Equal(got, []config.AllowedSkill{{Name: "ponytail", URL: "https://github.com/o/ponytail"}}) {
		t.Errorf("runner-wide %v", got)
	}
	if code := c.Run([]string{"allow-skill", "remove", "ponytail"}); code != 0 || len(runnerSkills(t, c)) != 0 {
		t.Errorf("remove: code %d, stderr %q", code, errb.String())
	}
}

// CLR-257: allow-skill refuses a missing verb, name or URL, or -url/-workspace without --project.
func TestAllowSkillUsage(t *testing.T) {
	c, _, _ := newCLI(t, "")
	saveConfig(t, c, config.Project{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"})
	for _, bad := range [][]string{
		{},
		{"allow", "p", "https://github.com/o/p"},
		{"add", "p", "--project", "WEB"},
		{"add", "p"},
		{"add", "--project", "WEB", "p", "https://github.com/o/p"},
		{"add", "p", "https://github.com/o/p", "-workspace", "default"},
		{"list", "extra"},
		{"remove"},
		{"remove", "--project", "WEB"},
	} {
		if code := c.Run(append([]string{"allow-skill"}, bad...)); code != 2 {
			t.Errorf("allow-skill %q: code %d, want 2", bad, code)
		}
	}
}
