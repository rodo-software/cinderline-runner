package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderstack-runner/internal/name"
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
		{URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/acme/web.git", AllowExisting: true},
		{URL: "https://b.example.com", Key: "WEB", Repository: "ssh://git@git.example.com/acme/api.git"},
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
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if !got.Projects[0].AllowExisting || got.Projects[1].AllowExisting {
		t.Errorf("allow_existing did not survive: %+v", got.Projects)
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

func TestAllowExistingDefault(t *testing.T) {
	cfg, err := Load(writeFile(t, `{"version": 1, "projects": [{"url": "https://a.example.com", "key": "WEB", "repository": "https://g.example.com/r.git"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Projects[0].AllowExisting {
		t.Error("AllowExisting true without the field")
	}
}

func TestValidate(t *testing.T) {
	ok := Project{URL: "https://a.example.com", Key: "WEB", Repository: "https://g.example.com/r.git"}
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
		{"duplicate", []Project{ok, ok}, "project 2 (WEB): "},
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
	if p.File != filepath.Join(p.Dir, "config.json") || p.Log != filepath.Join(p.Dir, name.Name+".log") || p.State != filepath.Join(p.Dir, "state") {
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
