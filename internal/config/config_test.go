package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
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
		{"same key two instances", []Project{ok, with(func(p *Project) { p.URL = "http://b.example.com" })}, ""},
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
	p.AllowHosts = []string{"studio.lan:1234", "192.168.1.5:8080", "localhost.example.com:443"}
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
