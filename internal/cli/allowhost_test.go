package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
)

// CLR-211: allow-host adds, lists and removes a project's allow_hosts
// entries, in that project only, and reloads an idle runner after each change.
func TestAllowHost(t *testing.T) {
	c, out, _ := reloadCLI(t, "")
	errb := &strings.Builder{}
	c.Stderr = errb
	web := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"}
	api := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "API", Repository: "https://git.example.com/o/api", AllowHosts: []string{"mirror.example.com:443"}}
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
	hosts := func() (web, api []string) {
		t.Helper()
		ps := projects(t, c)
		return ps[0].AllowHosts, ps[1].AllowHosts
	}
	run := func(args ...string) int {
		t.Helper()
		out.Reset()
		errb.Reset()
		return c.Run(append([]string{"allow-host"}, args...))
	}

	if code := run("list", "--project", "WEB"); code != 0 || !strings.Contains(out.String(), "no allow_hosts entries") {
		t.Errorf("list empty: code %d, %q", code, out.String())
	}
	if code := run("add", "PyPI.org:443", "--project", "WEB"); code != 0 || !reloaded() {
		t.Fatalf("add: code %d, stderr %q", code, errb.String())
	}
	if w, a := hosts(); !slices.Equal(w, []string{"pypi.org:443"}) || !slices.Equal(a, []string{"mirror.example.com:443"}) {
		t.Fatalf("after add: WEB %q, API %q", w, a)
	}
	if code := run("add", "pypi.org:443", "--project", "WEB"); code != 0 || !strings.Contains(out.String(), "already") || reloaded() {
		t.Errorf("duplicate add: code %d, %q", code, out.String())
	}
	if w, _ := hosts(); len(w) != 1 {
		t.Errorf("duplicate add changed the entries: %q", w)
	}

	for _, bad := range []struct{ hp, want string }{
		{"*.example.com:443", "host:port"},
		{"example.com", "host:port"},
		{"example.com:0", "host:port"},
		{"localhost:11434", "add-provider"},
		{"127.0.0.1:8080", "add-provider"},
		{"host.docker.internal:443", "add-provider"},
	} {
		if code := run("add", bad.hp, "--project", "WEB"); code != 1 || !strings.Contains(errb.String(), bad.want) || reloaded() {
			t.Errorf("add %s: code %d, stderr %q; want 1 and %q", bad.hp, code, errb.String(), bad.want)
		}
	}
	if w, _ := hosts(); !slices.Equal(w, []string{"pypi.org:443"}) {
		t.Errorf("invalid adds changed the entries: %q", w)
	}

	if code := run("add", "files.pythonhosted.org:443", "--project", "WEB"); code != 0 {
		t.Fatalf("second add: code %d, stderr %q", code, errb.String())
	}
	reloaded()
	if code := run("list", "--project", "WEB"); code != 0 || out.String() != "pypi.org:443\nfiles.pythonhosted.org:443\n" {
		t.Errorf("list: code %d, %q", code, out.String())
	}

	if code := run("remove", "nope.example.com:443", "--project", "WEB"); code != 1 || !strings.Contains(errb.String(), "pypi.org:443, files.pythonhosted.org:443") || reloaded() {
		t.Errorf("remove missing: code %d, stderr %q", code, errb.String())
	}
	if code := run("remove", "pypi.org:443", "--project", "WEB"); code != 0 || !reloaded() {
		t.Fatalf("remove: code %d, stderr %q", code, errb.String())
	}
	if w, a := hosts(); !slices.Equal(w, []string{"files.pythonhosted.org:443"}) || !slices.Equal(a, []string{"mirror.example.com:443"}) {
		t.Errorf("after remove: WEB %q, API %q", w, a)
	}
}

// CLR-211: allow-host refuses a missing verb or host, or -url/-workspace without --project.
func TestAllowHostUsage(t *testing.T) {
	c, _, _ := newCLI(t, "")
	saveConfig(t, c, config.Project{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"})
	for _, bad := range [][]string{
		{},
		{"open", "pypi.org:443", "--project", "WEB"},
		{"add", "--project", "WEB"},
		{"add", "--project", "WEB", "pypi.org:443"},
		{"add", "pypi.org:443", "-url", "https://a.example.com"},
		{"add", "pypi.org:443", "-workspace", "default"},
		{"list", "extra", "--project", "WEB"},
		{"list", "extra"},
		{"list", "-url", "https://a.example.com"},
		{"remove", "--project", "WEB"},
		{"remove"},
		{"remove", "pypi.org:443", "-workspace", "default"},
	} {
		if code := c.Run(append([]string{"allow-host"}, bad...)); code != 2 {
			t.Errorf("allow-host %q: code %d, want 2", bad, code)
		}
	}
}

// CLR-212: allow-host without --project manages runner-wide allow_hosts
// for all current and future projects, and asks for extra confirmation on add.
func TestAllowHostRunnerWide(t *testing.T) {
	runnerHosts := func(t *testing.T, c *CLI) []string {
		t.Helper()
		cfg, err := config.Load(c.Paths.File)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.AllowHosts
	}

	// Confirmation: only yes or y proceeds; anything else cancels without saving or reloading.
	for _, in := range []string{"", "\n", "no\n", "yess\n"} {
		c, out, _ := reloadCLI(t, in)
		errb := &strings.Builder{}
		c.Stderr = errb
		f := serveFakeRunner(t, c, "", false)
		if code := c.Run([]string{"allow-host", "add", "pypi.org:443"}); code != 1 {
			t.Errorf("answer %q: code %d, want 1", in, code)
		}
		if !strings.Contains(out.String(), "all current and future projects") || !strings.Contains(out.String(), "yes") {
			t.Errorf("answer %q: prompt %q lacks scope warning and yes instruction", in, out.String())
		}
		if !strings.Contains(errb.String(), "Cancelled") {
			t.Errorf("answer %q: stderr %q lacks Cancelled", in, errb.String())
		}
		if got := runnerHosts(t, c); len(got) != 0 {
			t.Errorf("answer %q: AllowHosts = %q, want empty", in, got)
		}
		if _, _, reloaded := f.state(); reloaded {
			t.Errorf("answer %q: reloaded on cancel", in)
		}
	}

	for _, in := range []string{"yes\n", "Y\n", " YES \n"} {
		c, out, _ := reloadCLI(t, in)
		errb := &strings.Builder{}
		c.Stderr = errb
		// Works with zero projects too (no config.json yet).
		f := serveFakeRunner(t, c, "", false)
		if code := c.Run([]string{"allow-host", "add", "PyPI.org:443"}); code != 0 {
			t.Fatalf("answer %q: code %d, stderr %q", in, code, errb.String())
		}
		if got := runnerHosts(t, c); !slices.Equal(got, []string{"pypi.org:443"}) {
			t.Errorf("answer %q: AllowHosts = %q, want [pypi.org:443]", in, got)
		}
		if !strings.Contains(out.String(), "all current and future projects") {
			t.Errorf("answer %q: stdout %q", in, out.String())
		}
		if _, _, reloaded := f.state(); !reloaded {
			t.Errorf("answer %q: did not reload", in)
		}
	}

	// Duplicate, invalid hosts, list and remove without --project; projects stay untouched.
	c, out, _ := reloadCLI(t, "yes\n")
	errb := &strings.Builder{}
	c.Stderr = errb
	web := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web", AllowHosts: []string{"web.example.com:443"}}
	saveConfig(t, c, web)
	f := serveFakeRunner(t, c, "", false)
	reloaded := func() bool {
		t.Helper()
		f.mu.Lock()
		defer f.mu.Unlock()
		r := f.reloaded
		f.reloaded = false
		return r
	}
	run := func(stdin string, args ...string) int {
		t.Helper()
		c.Stdin = strings.NewReader(stdin)
		out.Reset()
		errb.Reset()
		return c.Run(append([]string{"allow-host"}, args...))
	}

	if code := run("", "list"); code != 0 || !strings.Contains(strings.ToLower(out.String()), "no runner-wide") {
		t.Errorf("list empty: code %d, %q", code, out.String())
	}
	// Invalid hosts fail before asking for confirmation.
	for _, bad := range []struct{ hp, want string }{
		{"*.example.com:443", "host:port"},
		{"example.com", "host:port"},
		{"localhost:11434", "add-provider"},
		{"host.docker.internal:443", "add-provider"},
	} {
		if code := run("yes\n", "add", bad.hp); code != 1 || !strings.Contains(errb.String(), bad.want) || strings.Contains(out.String(), "Type yes") || reloaded() {
			t.Errorf("add %s: code %d, stdout %q, stderr %q; want 1, no prompt, %q", bad.hp, code, out.String(), errb.String(), bad.want)
		}
	}

	if code := run("yes\n", "add", "PyPI.org:443"); code != 0 || !reloaded() {
		t.Fatalf("add: code %d, stderr %q", code, errb.String())
	}
	// Duplicate succeeds without prompting for confirmation or reloading.
	if code := run("", "add", "pypi.org:443"); code != 0 || !strings.Contains(out.String(), "already") || strings.Contains(out.String(), "Type yes") || reloaded() {
		t.Errorf("duplicate add: code %d, stdout %q", code, out.String())
	}
	if code := run("yes\n", "add", "files.pythonhosted.org:443"); code != 0 || !reloaded() {
		t.Fatalf("second add: code %d, stderr %q", code, errb.String())
	}
	if code := run("", "list"); code != 0 || out.String() != "pypi.org:443\nfiles.pythonhosted.org:443\n" {
		t.Errorf("list: code %d, %q", code, out.String())
	}
	if code := run("", "list", "--project", "WEB"); code != 0 || out.String() != "web.example.com:443\n" {
		t.Errorf("project list: code %d, %q", code, out.String())
	}
	if code := run("", "remove", "nope.example.com:443"); code != 1 || !strings.Contains(errb.String(), "pypi.org:443, files.pythonhosted.org:443") || reloaded() {
		t.Errorf("remove missing: code %d, stderr %q", code, errb.String())
	}
	if code := run("", "remove", "pypi.org:443"); code != 0 || !reloaded() {
		t.Fatalf("remove: code %d, stderr %q", code, errb.String())
	}
	if got := runnerHosts(t, c); !slices.Equal(got, []string{"files.pythonhosted.org:443"}) {
		t.Errorf("after remove: AllowHosts = %q", got)
	}
	if got := projects(t, c)[0].AllowHosts; !slices.Equal(got, []string{"web.example.com:443"}) {
		t.Errorf("project WEB AllowHosts changed to %q", got)
	}
}
