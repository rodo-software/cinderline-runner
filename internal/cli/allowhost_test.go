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

// CLR-211: allow-host refuses a missing verb, host or project.
func TestAllowHostUsage(t *testing.T) {
	c, _, _ := newCLI(t, "")
	saveConfig(t, c, config.Project{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"})
	for _, bad := range [][]string{
		{},
		{"open", "pypi.org:443", "--project", "WEB"},
		{"add", "--project", "WEB"},
		{"add", "--project", "WEB", "pypi.org:443"},
		{"add", "pypi.org:443"},
		{"list", "extra", "--project", "WEB"},
		{"remove", "--project", "WEB"},
	} {
		if code := c.Run(append([]string{"allow-host"}, bad...)); code != 2 {
			t.Errorf("allow-host %q: code %d, want 2", bad, code)
		}
	}
}
