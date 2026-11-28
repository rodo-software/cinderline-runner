package config

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/name"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func twoProjects() *Config {
	return &Config{Projects: []Project{
		{URL: "https://a.example.com", Key: "WEB", Workspace: "default", Repository: "https://git.example.com/acme/web.git"},
		{URL: "https://b.example.com", Key: "WEB", Workspace: "acme", Repository: "https://git.example.com/acme/api.git"},
	}}
}

func TestRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub")
	path := filepath.Join(dir, "config.json")
	cfg := twoProjects()
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := twoProjects()
	want.Version = 1
	want.Projects[0].LocalID, want.Projects[1].LocalID = "WEB", "acme-WEB" // Load pins the ID
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}\n") || !strings.Contains(s, "\n  \"version\": 1,") {
		t.Errorf("file not indented JSON with version 1 and trailing newline:\n%s", s)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("folder holds %d entries, want only config.json", len(entries))
	}
	if runtime.GOOS != "windows" {
		for p, want := range map[string]fs.FileMode{dir: 0o700, path: 0o600} {
			if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != want {
				t.Errorf("%s: mode %v (%v), want %v", p, fi.Mode().Perm(), err, want)
			}
		}
	}
}

func TestLoadVersion(t *testing.T) {
	for _, tt := range []struct{ body, found string }{
		{`{"version": 2, "projects": []}`, "version 2"},
		{`{"version": 0, "projects": []}`, "version 0"},
		{`{"projects": []}`, "version 0"},
	} {
		path := writeFile(t, tt.body)
		_, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), tt.found) || !strings.Contains(err.Error(), "wants 1") || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: err %v, want it to name %q, wants 1 and the file", tt.body, err, tt.found)
		}
	}
}

func TestLoadBadFile(t *testing.T) {
	for _, body := range []string{
		`{"version": 1, "projects": [{"url": "https://a.example.com", "key": "WEB", "repository": "https://g.example.com/r.git", "token": "x"}]}`,
		`{"version": 1, "projects": [`,
		`{"version": 1, "projects": [{"url": "ftp://x", "key": "WEB", "repository": "https://g.example.com/r.git"}]}`,
	} {
		path := writeFile(t, body)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: err %v, want an error naming %s", body, err, path)
		}
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err %v, want fs.ErrNotExist", err)
	}
}

func TestValidate(t *testing.T) {
	ok := Project{URL: "https://a.example.com", Key: "WEB", Workspace: "default", Repository: "https://g.example.com/r.git"}
	with := func(f func(*Project)) Project { p := ok; f(&p); return p }
	tests := []struct {
		name     string
		projects []Project
		wantErr  string // empty means valid
	}{
		{"none", nil, ""},
		{"one", []Project{ok}, ""},
		{"same key two instances", []Project{ok, with(func(p *Project) { p.URL = "https://b.example.com" })}, ""},
		// CLR-208, decision 2026-11-19-https-instance: plain http only on the runner's own machine.
		{"http localhost", []Project{with(func(p *Project) { p.URL = "http://localhost:8000" })}, ""},
		{"http loopback", []Project{with(func(p *Project) { p.URL = "http://127.0.0.1:8000" })}, ""},
		{"http loopback v6", []Project{with(func(p *Project) { p.URL = "http://[::1]:8000" })}, ""},
		{"http other host", []Project{with(func(p *Project) { p.URL = "http://cinderline.example.com" })}, "project 1 (WEB): url must be an absolute https URL; plain http only"},
		{"http private address", []Project{with(func(p *Project) { p.URL = "http://192.168.1.5:8000" })}, "project 1 (WEB): url must be an absolute https URL; plain http only"},
		{"http docker host", []Project{with(func(p *Project) { p.URL = "http://host.docker.internal:8000" })}, "project 1 (WEB): url must be an absolute https URL; plain http only"},
		{"http localhost lookalike", []Project{with(func(p *Project) { p.URL = "http://localhost.example.com" })}, "project 1 (WEB): url must be an absolute https URL; plain http only"},
		{"ftp url", []Project{with(func(p *Project) { p.URL = "ftp://x" })}, "project 1 (WEB): url"},
		{"url without scheme", []Project{with(func(p *Project) { p.URL = "cinder.example.com" })}, "project 1 (WEB): url"},
		{"empty url", []Project{with(func(p *Project) { p.URL = "" })}, "project 1 (WEB): url"},
		{"empty key", []Project{with(func(p *Project) { p.Key = "" })}, "project 1 (): key"},
		{"key with space", []Project{ok, with(func(p *Project) { p.Key = "W EB" })}, "project 2 (W EB): key"},
		{"empty repository", []Project{with(func(p *Project) { p.Repository = "" })}, "project 1 (WEB): repository"},
		{"relative repository", []Project{with(func(p *Project) { p.Repository = "acme/web.git" })}, "project 1 (WEB): repository"},
		{"ssh repository", []Project{with(func(p *Project) { p.Repository = "git@g.example.com:acme/web.git" })}, "project 1 (WEB): repository"},
		{"repository with a password", []Project{with(func(p *Project) { p.Repository = "https://u:tok@g.example.com/acme/web.git" })}, "project 1 (WEB): repository \"https://g.example.com/acme/web.git\" must"},
		{"duplicate", []Project{ok, ok}, "project 2 (WEB): "},
		{"id with space", []Project{with(func(p *Project) { p.LocalID = "W EB" })}, "project 1 (WEB): id"},
		{"no workspace", []Project{with(func(p *Project) { p.Workspace = "" })}, "project 1 (WEB): workspace"},
		// CSR-63, Willem's decision (option C): extra allowlist entries come from local config only, as exact host:port.
		{"allow hosts", []Project{with(func(p *Project) { p.AllowHosts = []string{"mirror.example.com:443", "api.example.com:8443"} })}, ""},
		{"allow host without port", []Project{with(func(p *Project) { p.AllowHosts = []string{"mirror.example.com"} })}, "project 1 (WEB): allow_hosts"},
		{"allow host wildcard", []Project{with(func(p *Project) { p.AllowHosts = []string{"*.example.com:443"} })}, "project 1 (WEB): allow_hosts"},
		{"allow host bad port", []Project{with(func(p *Project) { p.AllowHosts = []string{"x.example:0"} })}, "project 1 (WEB): allow_hosts"},
		{"allow host empty host", []Project{with(func(p *Project) { p.AllowHosts = []string{":443"} })}, "project 1 (WEB): allow_hosts"},
		// CLR-215, decision 2026-11-24-agent-memory-limit: a size of at least 256m, as docker run --memory takes it.
		{"memory", []Project{with(func(p *Project) { p.Memory = "8g" })}, ""},
		{"memory at the minimum", []Project{with(func(p *Project) { p.Memory = "256M" })}, ""},
		{"memory under the minimum", []Project{with(func(p *Project) { p.Memory = "255m" })}, "project 1 (WEB): memory"},
		{"memory without unit", []Project{with(func(p *Project) { p.Memory = "4294967296" })}, "project 1 (WEB): memory"},
		{"memory in GB", []Project{with(func(p *Project) { p.Memory = "4GB" })}, "project 1 (WEB): memory"},
		{"memory zero", []Project{with(func(p *Project) { p.Memory = "0g" })}, "project 1 (WEB): memory"},
		{"memory negative", []Project{with(func(p *Project) { p.Memory = "-4g" })}, "project 1 (WEB): memory"},
		{"max_parallel_runs", []Project{with(func(p *Project) { p.MaxParallelRuns = 10 })}, ""},
		{"max_parallel_runs over 10", []Project{with(func(p *Project) { p.MaxParallelRuns = 11 })}, "project 1 (WEB): max_parallel_runs"},
		{"max_parallel_runs negative", []Project{with(func(p *Project) { p.MaxParallelRuns = -1 })}, "project 1 (WEB): max_parallel_runs"},
		{"memory fraction", []Project{with(func(p *Project) { p.Memory = "1.5g" })}, "project 1 (WEB): memory"},
		{"memory overflow", []Project{with(func(p *Project) { p.Memory = "99999999999999g" })}, "project 1 (WEB): memory"},
		// CLR-257: a skill package allowed on a strict runner, by name and URL.
		{"allow skill", []Project{with(func(p *Project) { p.AllowSkills = []AllowedSkill{{"ponytail", "https://github.com/o/ponytail"}} })}, ""},
		{"allow skill bad name", []Project{with(func(p *Project) { p.AllowSkills = []AllowedSkill{{"Pony--tail", "https://github.com/o/ponytail"}} })}, "project 1 (WEB): allow_skills name"},
		{"allow skill bad url", []Project{with(func(p *Project) { p.AllowSkills = []AllowedSkill{{"ponytail", "https://u:p@github.com/o/ponytail"}} })}, "project 1 (WEB): allow_skills ponytail"},
		{"allow skill twice", []Project{with(func(p *Project) {
			p.AllowSkills = []AllowedSkill{{"ponytail", "https://github.com/o/ponytail"}, {"ponytail", "https://github.com/x/ponytail"}}
		})}, "project 1 (WEB): allow_skills names ponytail twice"},
	}
	for _, tt := range tests {
		err := (&Config{Version: 1, Projects: tt.projects}).Validate()
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tt.name, err)
		case tt.wantErr != "" && (err == nil || !strings.HasPrefix(err.Error(), tt.wantErr)):
			t.Errorf("%s: err %v, want prefix %q", tt.name, err, tt.wantErr)
		}
	}
}

// CLR-257: a skill package's name and URL as the app's contract has them.
func TestSkillNameAndURL(t *testing.T) {
	for _, n := range []string{"a", "impeccable", "pony-tail-2", strings.Repeat("a", 64)} {
		if !ValidSkillName(n) {
			t.Errorf("%q refused", n)
		}
	}
	for _, n := range []string{"", "-a", "a-", "a--b", "A", "a_b", "a.b", "../a", "a/b", strings.Repeat("a", 65)} {
		if ValidSkillName(n) {
			t.Errorf("%q allowed", n)
		}
	}
	if err := CheckSkillURL("https://github.com:8443/o/impeccable"); err != nil {
		t.Error(err)
	}
	for _, u := range []string{"http://github.com/o/r", "https://u@github.com/o/r", "https://github.com/o/r?x=1", "https://github.com/o/r#x", "https://github.com/o/r?", "https://github.com/o/r#", "https://github.com/o r", "https://github.com/o/r\n", "file:///etc", "-https://x/y", "https://github.com/" + strings.Repeat("a", 500)} {
		if CheckSkillURL(u) == nil {
			t.Errorf("%q allowed", u)
		}
	}
}

// CLR-257: runner-wide allow_skills are checked as a project's are.
func TestValidateRunnerAllowSkills(t *testing.T) {
	c := &Config{Version: 1, AllowSkills: []AllowedSkill{{"ok", "ssh://x/y"}}}
	if err := c.Validate(); err == nil || !strings.HasPrefix(err.Error(), "allow_skills ok: ") {
		t.Errorf("err %v", err)
	}
}

func TestSaveReplaces(t *testing.T) {
	path := writeFile(t, "old")
	if err := (&Config{}).Save(path); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "{\n  \"version\": 1,\n  \"projects\": null\n}\n" {
		t.Errorf("contents %q", data)
	}
}

func TestSaveFailureKeepsOldFile(t *testing.T) {
	path := writeFile(t, "old")
	// The folder Save needs is the old file itself, so it cannot be created.
	if err := (&Config{}).Save(filepath.Join(path, "config.json")); err == nil {
		t.Fatal("Save succeeded where its folder is a file")
	}
	if data, _ := os.ReadFile(path); string(data) != "old" {
		t.Errorf("old file changed to %q", data)
	}
	if err := (&Config{Projects: []Project{{}}}).Save(path); err == nil {
		t.Fatal("Save of an invalid config succeeded")
	}
	if data, _ := os.ReadFile(path); string(data) != "old" {
		t.Errorf("old file changed to %q", data)
	}
}

// runtimeVar is the variable the socket's runtime folder is read from.
func runtimeVar() string {
	switch runtime.GOOS {
	case "darwin":
		return "TMPDIR"
	case "windows":
		return "LocalAppData"
	}
	return "XDG_RUNTIME_DIR"
}

func runtimeDir(t *testing.T) string {
	t.Helper()
	switch runtime.GOOS {
	case "darwin":
		return os.TempDir()
	case "windows":
		dir, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		return dir
	}
	return os.Getenv("XDG_RUNTIME_DIR")
}

func TestDefaultPaths(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	}
	p, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ path, root string }{
		{p.Dir, cfgDir}, {p.File, cfgDir}, {p.Log, cfgDir}, {p.State, cfgDir}, {p.Socket, runtimeDir(t)},
	} {
		rel, err := filepath.Rel(tt.root, tt.path)
		if err != nil || strings.HasPrefix(rel, "..") || !slices.Contains(strings.Split(rel, string(filepath.Separator)), name.Name) {
			t.Errorf("%s: not under %s with a %s element", tt.path, tt.root, name.Name)
		}
	}
	if p.File != filepath.Join(p.Dir, "config.json") || p.Log != filepath.Join(p.State, name.Name+".log") || p.State != filepath.Join(p.Dir, "state") {
		t.Errorf("unexpected paths %+v", p)
	}
	if len(p.Socket) > maxSocketPath {
		t.Errorf("socket %s is %d bytes, over %d", p.Socket, len(p.Socket), maxSocketPath)
	}
}

func TestDefaultSocketPath(t *testing.T) {
	// The real environment, not a temp folder: this is the path a person gets.
	p, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Socket) > maxSocketPath || filepath.Base(p.Socket) != "sock" {
		t.Errorf("socket %s", p.Socket)
	}
}

func TestSocketTooLong(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("x", maxSocketPath))
	t.Setenv(runtimeVar(), long)
	_, err := DefaultPaths()
	if err == nil || !strings.Contains(err.Error(), long) || !strings.Contains(err.Error(), "103") {
		t.Errorf("err %v, want one naming the path and the limit", err)
	}
}

func TestSocketWithoutRuntimeDir(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("XDG_RUNTIME_DIR is read on Linux only")
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	p, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if p.Socket != filepath.Join(cache, name.Name, "sock") {
		t.Errorf("socket %s, want under %s", p.Socket, cache)
	}
}

func TestSameRepository(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		same bool
	}{
		{"https://h.example.com/o/r", "https://h.example.com/o/r.git", true},
		{"https://h.example.com/o/r", "https://H.example.com/O/R.git/", true},
		{"https://h.example.com:443/o/r", "https://h.example.com/o/r/", true},
		{"https://h.example.com/o/r", "https://h.example.com/o/r2", false},
		{"https://h.example.com/o/r", "https://g.example.com/o/r", false},
		// Git signs in over https only, so no other form is the configured repository.
		{"git@git.rodo.nl:willem/cinderline.git", "https://git.rodo.nl/willem/cinderline", false},
		{"ssh://git@git.rodo.nl/willem/cinderline.git", "https://git.rodo.nl/willem/cinderline", false},
		{"http://git.rodo.nl/willem/cinderline", "https://git.rodo.nl/willem/cinderline", false},
		{"https://u:p@git.rodo.nl/willem/cinderline", "https://git.rodo.nl/willem/cinderline", false},
		// A project without a repository, or one that does not parse, is never the configured one.
		{"", "https://h.example.com/o/r", false},
		{"https://h.example.com/o/r", "::", false},
		{"https:///o/r", "https:///o/r", false},
	} {
		if got := SameRepository(tt.a, tt.b); got != tt.same {
			t.Errorf("SameRepository(%q, %q) = %v", tt.a, tt.b, got)
		}
	}
}

// CSR-63: a project's extra allowlist entries are read from allow_hosts.
func TestAllowHostsField(t *testing.T) {
	var p Project
	if err := json.Unmarshal([]byte(`{"allow_hosts":["mirror.example.com:443"]}`), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.AllowHosts) != 1 || p.AllowHosts[0] != "mirror.example.com:443" {
		t.Errorf("AllowHosts = %q", p.AllowHosts)
	}
}

func TestKeptSessionLimits(t *testing.T) {
	for _, tt := range []struct {
		keep, free string
		age        time.Duration
		bytes      int64
	}{
		{"", "", 7 * 24 * time.Hour, 10e9},
		{"30d", "500MB", 30 * 24 * time.Hour, 500e6},
		{"36h", "2TB", 36 * time.Hour, 2e12},
		{"off", "off", 0, 0},
	} {
		c := Config{KeepSessionsFor: tt.keep, MinFreeDisk: tt.free}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(%q, %q) = %v", tt.keep, tt.free, err)
		}
		if got := c.KeepSessions(); got != tt.age {
			t.Errorf("KeepSessions(%q) = %v, want %v", tt.keep, got, tt.age)
		}
		if got := c.MinFree(); got != tt.bytes {
			t.Errorf("MinFree(%q) = %d, want %d", tt.free, got, tt.bytes)
		}
	}
	for _, bad := range []Config{
		{KeepSessionsFor: "0d"}, {KeepSessionsFor: "-1h"}, {KeepSessionsFor: "week"}, {KeepSessionsFor: "0"},
		{MinFreeDisk: "20"}, {MinFreeDisk: "0GB"}, {MinFreeDisk: "1.5GB"}, {MinFreeDisk: "20gb"}, {MinFreeDisk: "99999999999TB"},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("Validate(%+v) accepted it", bad)
		}
	}
}

func TestExtraRepositoryValidate(t *testing.T) {
	for _, tt := range []struct {
		url, ref string
		ok       bool
	}{
		{"https://git.example.com/o/lib.git", "", true},
		{"https://git.example.com:8443/o/lib", "release/1.2", true},
		{"https://git.example.com/o/lib/", "v1.0.0", true},
		{"ssh://git@git.example.com:2222/o/lib", "", false},
		{"git@git.example.com:o/lib", "", false},
		{"https://user:pw@git.example.com/o/lib", "", false},
		{"https://tok@git.example.com/o/lib", "", false},
		{"ssh://root@git.example.com/o/lib", "", false},
		{"http://git.example.com/o/lib", "", false},
		{"file:///srv/lib", "", false},
		{"ext::sh -c x", "", false},
		{"git@git.example.com:-o/lib", "", false},
		{"https://git.example.com/", "", false},
		{"https://git.example.com/o/lib?x=1", "", false},
		{"https://git.example.com/o/lib", "-u", false},
		{"https://git.example.com/o/lib", "a..b", false},
		{"https://git.example.com/o/lib", "main@{1}", false},
		{"https://git.example.com/o/lib", "x.lock", false},
		{"https://git.example.com/o/lib", "HEAD", false},
		{"https://git.example.com/o/..", "", false},
	} {
		err := ExtraRepository{URL: tt.url, Ref: tt.ref}.Validate()
		if (err == nil) != tt.ok {
			t.Errorf("%q %q: err = %v, want ok %v", tt.url, tt.ref, err, tt.ok)
		}
	}
	if n := (ExtraRepository{URL: "https://h/o/lib.git"}).Name(); n != "lib" {
		t.Errorf("Name = %q", n)
	}
}

func TestValidateRefusesTwoExtraRepositoriesWithOneName(t *testing.T) {
	c := Config{Projects: []Project{{URL: "https://cs.example.com", Key: "CSR", Workspace: "default", Repository: "https://git.example.com/o/r",
		ExtraRepositories: []ExtraRepository{{URL: "https://git.example.com/a/lib"}, {URL: "https://git.example.com/b/lib"}}}}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "named lib") {
		t.Errorf("err = %v", err)
	}
}

// CSR-97: a project is URL, workspace slug and key; Docker names, keychain
// entries and state files derive from ID.
func TestProjectID(t *testing.T) {
	tests := []struct {
		p    Project
		want string
	}{
		{Project{Key: "APP", Workspace: "default"}, "APP"},                 // a Cinderline without workspaces
		{Project{Key: "APP", Workspace: "acme"}, "acme-APP"},               // added with a workspace
		{Project{LocalID: "CSR", Key: "CLR", Workspace: "default"}, "CSR"}, // renamed in the app (CLR-191)
	}
	for _, tt := range tests {
		if got := tt.p.ID(); got != tt.want {
			t.Errorf("%+v: ID %q, want %q", tt.p, got, tt.want)
		}
	}
}

func TestValidateWorkspace(t *testing.T) {
	a := Project{URL: "https://a.example.com", Key: "APP", Repository: "https://g.example.com/r.git", Workspace: "acme"}
	b := a
	b.Workspace = "beta"
	if err := (&Config{Projects: []Project{a, b}}).Validate(); err != nil {
		t.Errorf("one key in two workspaces on one URL: %v", err)
	}
	if err := (&Config{Projects: []Project{a, a}}).Validate(); err == nil || !strings.Contains(err.Error(), "same url, workspace and key") {
		t.Errorf("the same triple twice: %v", err)
	}
	// Same workspace and key on another URL: the Docker names would collide; the
	// daemon runs only the first, as with a shared key before (decision 2026-11-11-audit-fixes).
	other := a
	other.URL = "https://b.example.com"
	if err := (&Config{Projects: []Project{a, other}}).Validate(); err != nil {
		t.Errorf("same ID on two URLs must still load, so remove -url can fix it: %v", err)
	}
	for _, slug := range []string{"Acme Corp", "-acme", "acme/x", "acme.x", "../x"} {
		bad := a
		bad.Workspace = slug
		if err := (&Config{Projects: []Project{bad}}).Validate(); err == nil || !strings.Contains(err.Error(), "workspace") {
			t.Errorf("workspace %q: %v, want refused", slug, err)
		}
	}
}

// Decision 2026-11-17-local-model-providers point 1: a provider is a name
// (lower-case letters, digits and dashes, not openrouter, unique on the
// runner, at the top of the config file, not per project) and an OpenAI-compatible base URL (http or https, a host, an
// optional port and path, no user info).
func TestValidateProviders(t *testing.T) {
	ok := Project{URL: "https://a.example.com", Key: "WEB", Workspace: "default", Repository: "https://g.example.com/r.git"}
	for _, good := range [][]Provider{
		{{Name: "cerebellum", BaseURL: "http://studio.local:8080/v1"}},
		{{Name: "local-1", BaseURL: "http://localhost:11434/v1"}, {Name: "lan", BaseURL: "https://192.168.1.5/v1"}},
		{{Name: "v6", BaseURL: "http://[::1]:8080"}},
		{{Name: "plain", BaseURL: "https://llm.example.com"}},
		{{Name: "limited", BaseURL: "http://h:1/v1", MaxAgents: 1}, {Name: "most", BaseURL: "http://g:1/v1", MaxAgents: MaxProviderAgents}},
		{{Name: "ahead", BaseURL: "http://h:1/v1", MaxAgents: 1, Lookahead: MaxProviderAgents}},
	} {
		if err := (&Config{Projects: []Project{ok}, Providers: good}).Validate(); err != nil {
			t.Errorf("%+v: %v", good, err)
		}
	}
	for name, bad := range map[string][]Provider{
		"upper case":    {{Name: "Cere", BaseURL: "http://h:1/v1"}},
		"underscore":    {{Name: "a_b", BaseURL: "http://h:1/v1"}},
		"empty name":    {{Name: "", BaseURL: "http://h:1/v1"}},
		"leading dash":  {{Name: "-a", BaseURL: "http://h:1/v1"}},
		"too long":      {{Name: strings.Repeat("a", 51), BaseURL: "http://h:1/v1"}},
		"openrouter":    {{Name: "openrouter", BaseURL: "http://h:1/v1"}},
		"twice":         {{Name: "a", BaseURL: "http://h:1/v1"}, {Name: "a", BaseURL: "http://g:1/v1"}},
		"ftp":           {{Name: "a", BaseURL: "ftp://h/v1"}},
		"no scheme":     {{Name: "a", BaseURL: "h:1/v1"}},
		"no host":       {{Name: "a", BaseURL: "http:///v1"}},
		"user info":     {{Name: "a", BaseURL: "http://u:p@h:1/v1"}},
		"query":         {{Name: "a", BaseURL: "http://h:1/v1?x=1"}},
		"fragment":      {{Name: "a", BaseURL: "http://h:1/v1#x"}},
		"port zero":     {{Name: "a", BaseURL: "http://h:0/v1"}},
		"port too high": {{Name: "a", BaseURL: "http://h:70000/v1"}},
		"space":         {{Name: "a", BaseURL: "http://h:1/v 1"}},
		"bad host":      {{Name: "a", BaseURL: "http://h*st:1/v1"}},
		"negative max":  {{Name: "a", BaseURL: "http://h:1/v1", MaxAgents: -1}},
		"max too high":  {{Name: "a", BaseURL: "http://h:1/v1", MaxAgents: MaxProviderAgents + 1}},
		// CLR-253: lookahead is runs past max_agents, so it needs one.
		"lookahead without max": {{Name: "a", BaseURL: "http://h:1/v1", Lookahead: 1}},
		"negative lookahead":    {{Name: "a", BaseURL: "http://h:1/v1", MaxAgents: 1, Lookahead: -1}},
		"lookahead too high":    {{Name: "a", BaseURL: "http://h:1/v1", MaxAgents: 1, Lookahead: MaxProviderAgents + 1}},
	} {
		if err := (&Config{Projects: []Project{ok}, Providers: bad}).Validate(); err == nil || !strings.Contains(err.Error(), "provider") {
			t.Errorf("%s: err = %v, want a provider error", name, err)
		}
	}
	// A password in the URL is never repeated in the error.
	if err := (&Config{Providers: []Provider{{Name: "a", BaseURL: "http://u:hunter2@h:1/v1"}}}).Validate(); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("err = %v", err)
	}
}

// Providers are a top-level list; a project entry with its own is refused
// as an unknown field (pre-release, no migration).
func TestLoadProvidersAtTheTop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	project := `{"url":"https://a.example.com","key":"WEB","workspace":"default","repository":"https://g.example.com/r.git"`
	if err := os.WriteFile(path, []byte(`{"version":1,"providers":[{"name":"cerebellum","base_url":"http://localhost:8080/v1"}],"projects":[`+project+`}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || len(cfg.Providers) != 1 || cfg.Providers[0].Name != "cerebellum" {
		t.Fatalf("Load = %+v, %v", cfg, err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"projects":[`+project+`,"providers":[]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "providers") {
		t.Errorf("a project's providers: %v, want an unknown field", err)
	}
}

func TestProviderFreeRoundTrip(t *testing.T) {
	path := writeFile(t, `{"version":1,"providers":[{"name":"legacy","base_url":"http://localhost:8080/v1"},{"name":"local","base_url":"http://localhost:8000/v1","free":true},{"name":"paid","base_url":"https://provider.example/v1","free":false}],"projects":[]}`)
	check := func() *Config {
		t.Helper()
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Providers; len(got) != 3 || got[0].Free || !got[1].Free || got[2].Free {
			t.Fatalf("providers = %+v", got)
		}
		return cfg
	}
	if err := check().Save(path); err != nil {
		t.Fatal(err)
	}
	check()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), `"free"`) != 1 {
		t.Errorf("expected free to be stored only for the marked provider: %s", data)
	}
}

// Decision 2026-11-17-local-model-providers point 3: allow_hosts never
// opens the runner's own machine; a provider does, on one port.
func TestAllowHostsRefusesTheRunnersMachine(t *testing.T) {
	ok := Project{URL: "https://a.example.com", Key: "WEB", Workspace: "default", Repository: "https://g.example.com/r.git"}
	for _, hp := range []string{"localhost:8080", "LocalHost:1", "127.0.0.1:11434", "127.1.2.3:80", "host.docker.internal:8080", "HOST.DOCKER.INTERNAL:1", "[::1]:8080"} {
		p := ok
		p.AllowHosts = []string{hp}
		err := (&Config{Projects: []Project{p}}).Validate()
		if err == nil || !strings.Contains(err.Error(), "add-provider") {
			t.Errorf("%s: %v, want refused with a pointer to add-provider", hp, err)
		}
	}
	p := ok
	p.AllowHosts = []string{"provider.example:8080", "192.168.1.5:8080", "localhost.example.com:443"}
	if err := (&Config{Projects: []Project{p}}).Validate(); err != nil {
		t.Errorf("ordinary hosts: %v", err)
	}
}

// CLR-191: a key renamed after Load keeps every local name.
func TestLoadPinsTheID(t *testing.T) {
	path := writeFile(t, `{"version": 1, "projects": [
		{"url": "https://a.example.com", "key": "CSR", "workspace": "acme", "repository": "https://g.example.com/r.git"},
		{"id": "OLD", "url": "https://a.example.com", "key": "CL", "workspace": "acme", "repository": "https://g.example.com/r.git"}]}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Projects[0].Key = "CLR"
	if got := cfg.Projects[0].ID(); got != "acme-CSR" {
		t.Errorf("renamed entry: ID %q, want acme-CSR", got)
	}
	if got := cfg.Projects[1].ID(); got != "OLD" {
		t.Errorf("entry with an id: ID %q, want OLD", got)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(path); err != nil || cfg.Projects[0].ID() != "acme-CSR" || cfg.Projects[0].Key != "CLR" {
		t.Errorf("after save: %+v, %v; want key CLR with ID acme-CSR", cfg, err)
	}
}

// CLR-191: fields an older runner wrote are dropped and named, not refused;
// names_by_key keeps the key-only names it stood for.
func TestLoadDropsOldFields(t *testing.T) {
	path := writeFile(t, `{"version": 1, "merge_poll_seconds": 30, "build_image": true, "projects": [
		{"url": "https://a.example.com", "key": "CSR", "workspace": "acme", "names_by_key": true, "allow_existing": true, "repository": "https://g.example.com/r.git"},
		{"url": "https://b.example.com", "key": "WEB", "workspace": "acme", "names_by_key": true, "repository": "https://g.example.com/w.git"}]}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"merge_poll_seconds", "build_image", "names_by_key", "allow_existing"}; !slices.Equal(cfg.Dropped, want) {
		t.Errorf("dropped %q, want %q", cfg.Dropped, want)
	}
	if got := cfg.Projects[0].ID(); got != "CSR" {
		t.Errorf("names_by_key entry: ID %q, want CSR", got)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if s := string(data); strings.Contains(s, "names_by_key") || strings.Contains(s, "build_image") {
		t.Errorf("saved file keeps an old field:\n%s", s)
	}
	if _, err := Load(writeFile(t, `{"version": 1, "projects": [], "nmaes_by_key": true}`)); err == nil {
		t.Error("an unknown field that no runner wrote loaded, want an error")
	}
}

// CLR-193: a host a person allowed in Cinderline passes as an allow_hosts
// entry would: host:port, never the runner's own machine.
func TestAllowedHost(t *testing.T) {
	for _, hp := range []string{"pypi.org:443", "Files.PythonHosted.org:443", "192.168.1.5:8080"} {
		if !AllowedHost(hp) {
			t.Errorf("%s refused", hp)
		}
	}
	for _, hp := range []string{"", "pypi.org", "pypi.org:0", "pypi.org:65536", "*.pypi.org:443", "a b:1", "localhost:8080", "127.0.0.1:1", "[::1]:80", "HOST.DOCKER.INTERNAL:11434", "u@h:1", "h:443/x"} {
		if AllowedHost(hp) {
			t.Errorf("%s allowed", hp)
		}
	}
}

// CLR-193: strict_hosts is runner-wide, off unless set.
func TestStrictHostsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := (&Config{StrictHosts: true}).Save(path); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(path); err != nil || !got.StrictHosts {
		t.Errorf("strict_hosts = %v, %v; want true", got, err)
	}
	if got, err := Load(writeFile(t, `{"version": 1, "projects": []}`)); err != nil || got.StrictHosts {
		t.Errorf("strict_hosts unset = %v, %v; want false", got, err)
	}
}

// CLR-212: runner-wide allow_hosts applies to every current and future
// project and follows the same host:port and local-machine rules.
func TestConfigAllowHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := (&Config{AllowHosts: []string{"pypi.org:443", "192.168.1.5:8080"}}).Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := []string{"pypi.org:443", "192.168.1.5:8080"}; !slices.Equal(got.AllowHosts, want) {
		t.Errorf("AllowHosts = %q, want %q", got.AllowHosts, want)
	}
	for _, bad := range []string{"pypi.org", "*.example.com:443", "x.example:0", ":443"} {
		err := (&Config{AllowHosts: []string{bad}}).Validate()
		if err == nil || !strings.Contains(err.Error(), "allow_hosts") {
			t.Errorf("%q: err = %v, want allow_hosts error", bad, err)
		}
	}
	for _, machine := range []string{"localhost:8080", "127.0.0.1:11434", "[::1]:8080", "host.docker.internal:8080"} {
		err := (&Config{AllowHosts: []string{machine}}).Validate()
		if err == nil || !strings.Contains(err.Error(), "add-provider") {
			t.Errorf("%q: err = %v, want add-provider error", machine, err)
		}
	}
}

// CLR-215: a project's agent containers get its memory setting, or 4g.
func TestProjectMemoryLimit(t *testing.T) {
	for _, tt := range []struct {
		memory string
		bytes  int64
	}{
		{"", 4 << 30},
		{"8g", 8 << 30},
		{"512M", 512 << 20},
		{"256m", 256 << 20},
	} {
		if got := (Project{Memory: tt.memory}).MemoryLimit(); got != tt.bytes {
			t.Errorf("MemoryLimit(%q) = %d, want %d", tt.memory, got, tt.bytes)
		}
	}
}

func TestParseMemory(t *testing.T) {
	if n, err := ParseMemory("2G"); err != nil || n != 2<<30 {
		t.Errorf("ParseMemory(2G) = %d, %v", n, err)
	}
	for _, bad := range []string{"", "g", "4", "4 g", " 4g", "4gb", "4k", "100m", "0x10g", "+4g", "04g"} {
		if _, err := ParseMemory(bad); err == nil {
			t.Errorf("ParseMemory(%q) accepted it", bad)
		}
	}
}

// CLR-249, decision 2026-11-25-parallel-runs-default: a project's own
// max_parallel_runs wins, else the runner-wide one, else 1.
func TestParallel(t *testing.T) {
	for _, tt := range []struct {
		name            string
		runner, project int
		want            int
	}{
		{"unset", 0, 0, 1},
		{"runner-wide only", 3, 0, 3},
		{"project overrides", 3, 5, 5},
		{"project under the runner-wide", 4, 1, 1},
		{"project only", 0, 2, 2},
	} {
		c := Config{MaxParallelRuns: tt.runner, Projects: []Project{{MaxParallelRuns: tt.project}}}
		if got := c.Projects[0].Parallel(c.MaxParallelRuns); got != tt.want {
			t.Errorf("%s: Parallel %d, want %d", tt.name, got, tt.want)
		}
	}
	// A cleared override falls back to the runner-wide value.
	c := Config{MaxParallelRuns: 4, Projects: []Project{{MaxParallelRuns: 2}}}
	c.Projects[0].MaxParallelRuns = 0
	if got := c.Projects[0].Parallel(c.MaxParallelRuns); got != 4 {
		t.Errorf("cleared: Parallel %d, want 4", got)
	}
}

// CLR-249: the runner-wide max_parallel_runs is 1 to 10, as a project's is,
// and round-trips at the top of the file.
func TestValidateRunnerParallel(t *testing.T) {
	for n, ok := range map[int]bool{-1: false, 0: true, 1: true, 10: true, 11: false} {
		err := (&Config{Version: 1, MaxParallelRuns: n}).Validate()
		if ok != (err == nil) || err != nil && !strings.HasPrefix(err.Error(), "max_parallel_runs must be 1 to 10") {
			t.Errorf("max_parallel_runs %d: err %v", n, err)
		}
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := (&Config{MaxParallelRuns: 3}).Save(path); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(path); err != nil || got.MaxParallelRuns != 3 {
		t.Fatalf("Load: %+v, %v", got, err)
	}
}

// CLR-269, decision 2026-11-26-local-web-page: web is "on", "off" or unset,
// web_port 1024 to 65535 or unset, both runner-wide.
func TestValidateWeb(t *testing.T) {
	for _, tt := range []struct {
		web  string
		port int
		ok   bool
	}{
		{"", 0, true}, {"on", 0, true}, {"off", 8080, true}, {"", 1024, true}, {"", 65535, true},
		{"yes", 0, false}, {"On", 0, false}, {"", 1023, false}, {"", 65536, false}, {"", -1, false},
	} {
		err := (&Config{Web: tt.web, WebPort: tt.port}).Validate()
		if tt.ok != (err == nil) {
			t.Errorf("web %q, web_port %d: err %v", tt.web, tt.port, err)
		}
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := (&Config{Web: "on", WebPort: 8080}).Save(path); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(path); err != nil || got.Web != "on" || got.WebPort != 8080 {
		t.Fatalf("Load: %+v, %v", got, err)
	}
}

// CLR-269: the page follows web, else webDefaultOn, on 127.0.0.1 at web_port or 7420.
func TestWebOnAndAddr(t *testing.T) {
	for web, want := range map[string]bool{"": webDefaultOn, "on": true, "off": false} {
		if got := (&Config{Web: web}).WebOn(); got != want {
			t.Errorf("web %q: WebOn %v, want %v", web, got, want)
		}
	}
	if got := (&Config{}).WebAddr(); got != "127.0.0.1:7420" {
		t.Errorf("WebAddr = %q, want 127.0.0.1:7420", got)
	}
	if got := (&Config{WebPort: 8080}).WebAddr(); got != "127.0.0.1:8080" {
		t.Errorf("WebAddr = %q, want 127.0.0.1:8080", got)
	}
}

// CLR-273: agents never reach the web page's port on the runner's own
// machine. allow_hosts, runner-wide and per project, and an app approval
// refuse it as any port there; a provider there is refused on that port
// only, the default 7420 or web_port, with the page on or off.
func TestTheWebPortIsNeverOpenedToAgents(t *testing.T) {
	ok := Project{URL: "https://a.example.com", Key: "WEB", Workspace: "default", Repository: "https://g.example.com/r.git"}
	for _, port := range []int{0, 8080} {
		web := strconv.Itoa(cmp.Or(port, DefaultWebPort))
		for _, host := range []string{"localhost", "127.0.0.1", "[::1]", "host.docker.internal", "HOST.Docker.Internal"} {
			hp := host + ":" + web
			if AllowedHost(hp) {
				t.Errorf("AllowedHost(%s) = true, want an app approval refused", hp)
			}
			if err := (&Config{WebPort: port, AllowHosts: []string{hp}}).Validate(); err == nil {
				t.Errorf("runner allow_hosts %s: valid, want refused", hp)
			}
			p := ok
			p.AllowHosts = []string{hp}
			if err := (&Config{WebPort: port, Projects: []Project{p}}).Validate(); err == nil {
				t.Errorf("project allow_hosts %s: valid, want refused", hp)
			}
			for _, on := range []string{"on", "off"} {
				c := &Config{Web: on, WebPort: port, Providers: []Provider{{Name: "local", BaseURL: "http://" + hp + "/v1"}}}
				if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "provider local") || !strings.Contains(err.Error(), "web page's port") || !strings.Contains(err.Error(), name.Name+" web port N") {
					t.Errorf("web %s, provider at %s: err %v, want refused as the web page's port", on, hp, err)
				}
			}
		}
	}
	for _, base := range []string{"http://localhost:11434/v1", "http://host.docker.internal:7421", "https://localhost/v1", "https://models.example:7420/v1", "http://192.168.1.5:7420"} {
		if err := (&Config{Providers: []Provider{{Name: "p", BaseURL: base}}}).Validate(); err != nil {
			t.Errorf("provider at %s: %v, want valid", base, err)
		}
	}
	if err := (&Config{WebPort: 8080, Providers: []Provider{{Name: "p", BaseURL: "http://localhost:7420"}}}).Validate(); err != nil {
		t.Errorf("provider on 7420 with web_port 8080: %v, want valid", err)
	}
}

// CLR-282, decision 2026-11-26-local-web-page: transcript_days is 0 to 365,
// runner-wide or a project's own, which wins, and 0 is kept; transcript_max_size
// is written like memory, at least 100m, 1g by default.
func TestTranscriptSettings(t *testing.T) {
	days := func(n int) *int { return &n }
	ok := Project{URL: "https://a.example.com", Key: "WEB", Workspace: "default", Repository: "https://g.example.com/r.git"}
	for _, tt := range []struct {
		name    string
		cfg     Config
		wantErr string // empty means valid
	}{
		{"defaults", Config{}, ""},
		{"days 0", Config{TranscriptDays: days(0)}, ""},
		{"days 365", Config{TranscriptDays: days(365)}, ""},
		{"days 366", Config{TranscriptDays: days(366)}, "transcript_days"},
		{"days negative", Config{TranscriptDays: days(-1)}, "transcript_days"},
		{"project days 0", Config{Projects: []Project{{URL: ok.URL, Key: ok.Key, Workspace: ok.Workspace, Repository: ok.Repository, TranscriptDays: days(0)}}}, ""},
		{"project days 366", Config{Projects: []Project{{URL: ok.URL, Key: ok.Key, Workspace: ok.Workspace, Repository: ok.Repository, TranscriptDays: days(366)}}}, "project 1 (WEB): transcript_days"},
		{"size 2g", Config{TranscriptMaxSize: "2g"}, ""},
		{"size at the minimum", Config{TranscriptMaxSize: "100M"}, ""},
		{"size under the minimum", Config{TranscriptMaxSize: "99m"}, "transcript_max_size"},
		{"size in GB", Config{TranscriptMaxSize: "1GB"}, "transcript_max_size"},
		{"size without unit", Config{TranscriptMaxSize: "1073741824"}, "transcript_max_size"},
		{"size fraction", Config{TranscriptMaxSize: "1.5g"}, "transcript_max_size"},
		{"size zero", Config{TranscriptMaxSize: "0g"}, "transcript_max_size"},
	} {
		err := tt.cfg.Validate()
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tt.name, err)
		case tt.wantErr != "" && (err == nil || !strings.HasPrefix(err.Error(), tt.wantErr)):
			t.Errorf("%s: err %v, want prefix %q", tt.name, err, tt.wantErr)
		}
	}

	for _, tt := range []struct {
		runner, project *int
		want            int
	}{
		{nil, nil, 14},
		{days(30), nil, 30},
		{nil, days(3), 3},
		{days(30), days(3), 3},
		{days(30), days(0), 0},
		{days(0), nil, 0},
	} {
		c := Config{TranscriptDays: tt.runner}
		if got := c.TranscriptDaysFor(Project{TranscriptDays: tt.project}); got != tt.want {
			t.Errorf("TranscriptDaysFor(runner %v, project %v) = %d, want %d", tt.runner, tt.project, got, tt.want)
		}
	}

	for size, want := range map[string]int64{"": 1 << 30, "512m": 512 << 20, "2G": 2 << 30, "100m": 100 << 20} {
		if got := (&Config{TranscriptMaxSize: size}).TranscriptMax(); got != want {
			t.Errorf("TranscriptMax(%q) = %d, want %d", size, got, want)
		}
	}
}
