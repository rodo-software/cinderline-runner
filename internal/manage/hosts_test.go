package manage

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
)

// hostsConfig writes a config.json with WEB, and API in two places: IDs
// WEB, API and team-API.
func hostsConfig(t *testing.T) config.Paths {
	t.Helper()
	p := paths(t)
	cfg := &config.Config{Projects: []config.Project{
		{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"},
		{Workspace: "default", URL: "https://a.example.com", Key: "API", Repository: "https://git.example.com/o/api", AllowHosts: []string{"mirror.example.com:443"}},
		{Workspace: "team", URL: "https://b.example.com", Key: "API", Repository: "https://git.example.com/o/api2"},
	}}
	if err := cfg.Save(p.File); err != nil {
		t.Fatal(err)
	}
	return p
}

// allowHosts are the runner-wide entries, then those of each project.
func allowHosts(t *testing.T, p config.Paths) [][]string {
	t.Helper()
	cfg, err := config.Load(p.File) // Load validates
	if err != nil {
		t.Fatal(err)
	}
	out := [][]string{cfg.AllowHosts}
	for _, q := range cfg.Projects {
		out = append(out, q.AllowHosts)
	}
	return out
}

// CLR-295: AddHost and RemoveHost change runner-wide allow_hosts for an
// empty project and that project's otherwise, in lower case, and nothing else.
func TestAddAndRemoveHost(t *testing.T) {
	p := hostsConfig(t)
	for _, a := range [][2]string{{"", "PyPI.org:443"}, {"WEB", "files.example.com:443"}, {"team-API", "b.example.com:443"}, {"API", "a.example.com:443"}} {
		if err := AddHost(p, a[0], a[1]); err != nil {
			t.Fatalf("AddHost %q: %v", a, err)
		}
	}
	want := [][]string{{"pypi.org:443"}, {"files.example.com:443"}, {"mirror.example.com:443", "a.example.com:443"}, {"b.example.com:443"}}
	if got := allowHosts(t, p); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("after adds %q, want %q", got, want)
	}
	for _, r := range [][2]string{{"", "pypi.org:443"}, {"API", "MIRROR.example.com:443"}} {
		if err := RemoveHost(p, r[0], r[1]); err != nil {
			t.Fatalf("RemoveHost %q: %v", r, err)
		}
	}
	want = [][]string{nil, {"files.example.com:443"}, {"a.example.com:443"}, {"b.example.com:443"}}
	if got := allowHosts(t, p); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("after removes %q, want %q", got, want)
	}
}

// AddHost works without a config.json yet, as the CLI did.
func TestAddHostWithoutAConfig(t *testing.T) {
	p := paths(t)
	if err := AddHost(p, "", "pypi.org:443"); err != nil {
		t.Fatal(err)
	}
	if got := allowHosts(t, p); !slices.Equal(got[0], []string{"pypi.org:443"}) {
		t.Errorf("allow_hosts %q", got[0])
	}
}

// Every refusal is Refused with the command's text and writes nothing;
// CheckAddHost refuses the same adds, with the same text.
func TestHostRefusals(t *testing.T) {
	p := hostsConfig(t)
	if err := AddHost(p, "WEB", "pypi.org:443"); err != nil {
		t.Fatal(err)
	}
	before := allowHosts(t, p)
	for _, c := range []struct {
		what          string
		add           bool
		project, host string
		want          string
	}{
		{"wildcard", true, "WEB", "*.example.com:443", `"*.example.com:443" must be host:port, without wildcards`},
		{"no port", true, "", "example.com", "must be host:port"},
		{"port 0", true, "WEB", "example.com:0", "must be host:port"},
		{"localhost", true, "WEB", "localhost:11434", "is the runner's own machine, which allow_hosts never opens; add a model provider instead with `cinderline add-provider NAME URL`"},
		{"loopback", true, "", "127.0.0.1:8080", "add-provider"},
		{"docker host", true, "WEB", "host.docker.internal:443", "add-provider"},
		{"duplicate", true, "WEB", "PYPI.org:443", "project WEB already allows pypi.org:443"},
		{"unknown project", true, "NOPE", "pypi.org:443", "no project NOPE"},
		{"a key, not an ID", true, "web", "pypi.org:443", "no project web"},
		{"remove missing", false, "WEB", "nope.example.com:443", "project WEB has no allow_hosts entry nope.example.com:443; its entries: pypi.org:443"},
		{"remove missing runner-wide", false, "", "nope.example.com:443", "runner-wide allow_hosts has no entry nope.example.com:443; its entries: none"},
		{"remove unknown project", false, "NOPE", "pypi.org:443", "no project NOPE"},
	} {
		do := RemoveHost
		if c.add {
			do = AddHost
		}
		err := do(p, c.project, c.host)
		if r := (Refused{}); !errors.As(err, &r) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want Refused with %q", c.what, err, c.want)
		}
		if got := allowHosts(t, p); !slices.EqualFunc(got, before, slices.Equal) {
			t.Fatalf("%s wrote %q", c.what, got)
		}
		if c.add {
			if cerr := CheckAddHost(p, c.project, c.host); cerr == nil || err == nil || cerr.Error() != err.Error() {
				t.Errorf("CheckAddHost %s = %v, want AddHost's %v", c.what, cerr, err)
			}
		}
	}
	if err := CheckAddHost(p, "WEB", "new.example.com:443"); err != nil {
		t.Errorf("CheckAddHost of a new host = %v", err)
	}
	if got := allowHosts(t, p); !slices.EqualFunc(got, before, slices.Equal) {
		t.Errorf("CheckAddHost wrote %q", got)
	}
}

// A duplicate is AlreadyAllowed with the stored entry, so the CLI can say
// nothing changed.
func TestAddHostDuplicateIsAlreadyAllowed(t *testing.T) {
	p := hostsConfig(t)
	err := AddHost(p, "API", "Mirror.Example.com:443")
	if a := (AlreadyAllowed{}); !errors.As(err, &a) || a != (AlreadyAllowed{"API", "mirror.example.com:443"}) {
		t.Errorf("AddHost = %v, want AlreadyAllowed of API mirror.example.com:443", err)
	}
	if err := AddHost(p, "", "x.example.com:443"); err != nil {
		t.Fatal(err)
	}
	if err := AddHost(p, "", "X.example.com:443"); err == nil || err.Error() != "all projects already allow x.example.com:443" {
		t.Errorf("runner-wide duplicate = %v", err)
	}
}
