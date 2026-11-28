package manage

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
)

func paths(t *testing.T) config.Paths {
	dir := t.TempDir()
	return config.Paths{Dir: dir, File: filepath.Join(dir, "config.json")}
}

// CLR-294: SetStrictHosts turns strict hosts on and off and keeps the rest
// of a valid config.
func TestSetStrictHosts(t *testing.T) {
	p := paths(t)
	web := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"}
	if err := (&config.Config{Projects: []config.Project{web}, AllowHosts: []string{"x.example.com:443"}}).Save(p.File); err != nil {
		t.Fatal(err)
	}
	for _, on := range []bool{true, false} {
		if err := SetStrictHosts(p, on); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(p.File) // Load validates
		if err != nil {
			t.Fatal(err)
		}
		if cfg.StrictHosts != on || len(cfg.Projects) != 1 || len(cfg.AllowHosts) != 1 {
			t.Errorf("on %v: strict %v, %d projects, allow_hosts %q", on, cfg.StrictHosts, len(cfg.Projects), cfg.AllowHosts)
		}
	}
}

// SetStrictHosts writes a new config when there is none yet, as the CLI did.
func TestSetStrictHostsWithoutAConfig(t *testing.T) {
	p := paths(t)
	if err := SetStrictHosts(p, true); err != nil {
		t.Fatal(err)
	}
	if cfg, err := config.Load(p.File); err != nil || !cfg.StrictHosts {
		t.Errorf("Load = %+v, %v; want strict hosts on", cfg, err)
	}
}

// save marks a value Validate refuses as Refused, and writes nothing.
func TestSaveRefuses(t *testing.T) {
	p := paths(t)
	err := save(p, &config.Config{MaxParallelRuns: 99})
	if r := (Refused{}); !errors.As(err, &r) {
		t.Fatalf("save = %v, want Refused", err)
	}
	if cfg, err := Load(p); err != nil || cfg.MaxParallelRuns != 0 {
		t.Errorf("Load = %+v, %v; want the refused config not written", cfg, err)
	}
}

// CLR-297: SetTranscriptMaxSize sets the runner-wide transcript_max_size,
// lowercased, refuses one under the minimum, and clears it with "".
func TestSetTranscriptMaxSize(t *testing.T) {
	p := paths(t)
	if err := SetTranscriptMaxSize(p, "2G"); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := Load(p); cfg.TranscriptMaxSize != "2g" {
		t.Errorf("transcript_max_size %q, want 2g", cfg.TranscriptMaxSize)
	}
	for _, bad := range []string{"99m", "2gb", "-1g", "1.5g"} {
		if err := SetTranscriptMaxSize(p, bad); !errors.As(err, new(Refused)) {
			t.Errorf("%q: %v, want Refused", bad, err)
		}
	}
	if err := SetTranscriptMaxSize(p, "100m"); err != nil { // the minimum
		t.Fatal(err)
	}
	if err := SetTranscriptMaxSize(p, ""); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := Load(p); cfg.TranscriptMaxSize != "" {
		t.Errorf("transcript_max_size %q after clear, want unset", cfg.TranscriptMaxSize)
	}
}

// CLR-297: SetWeb turns the page on or off and sets its port, each left
// alone when nil, and refuses a port out of range.
func TestSetWeb(t *testing.T) {
	p := paths(t)
	on, off, port := true, false, 8080
	if err := SetWeb(p, &on, &port); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := Load(p); cfg.Web != "on" || cfg.WebPort != 8080 {
		t.Errorf("web %q, web_port %d; want on, 8080", cfg.Web, cfg.WebPort)
	}
	if err := SetWeb(p, &off, nil); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := Load(p); cfg.Web != "off" || cfg.WebPort != 8080 {
		t.Errorf("web %q, web_port %d; want off, 8080 kept", cfg.Web, cfg.WebPort)
	}
	for _, n := range []int{1023, 65536, -1} {
		if err := SetWeb(p, nil, &n); !errors.As(err, new(Refused)) {
			t.Errorf("port %d: %v, want Refused", n, err)
		}
	}
	for _, n := range []int{1024, 65535} {
		if err := SetWeb(p, nil, &n); err != nil {
			t.Errorf("port %d: %v", n, err)
		}
	}
	if cfg, _ := Load(p); cfg.Web != "off" || cfg.WebPort != 65535 {
		t.Errorf("web %q, web_port %d; want off, 65535", cfg.Web, cfg.WebPort)
	}
}

// twoWEBs is a config.json with two projects of key WEB, at two URLs, and
// one of key API.
func twoWEBs(t *testing.T) config.Paths {
	t.Helper()
	p := paths(t)
	cfg := &config.Config{Projects: []config.Project{
		{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"},
		{Workspace: "default", URL: "https://b.example.com", Key: "WEB", Repository: "https://git.example.com/o/web2"},
		{Workspace: "default", URL: "https://a.example.com", Key: "API", Repository: "https://git.example.com/o/api"},
	}}
	if err := cfg.Save(p.File); err != nil {
		t.Fatal(err)
	}
	return p
}

func load(t *testing.T, p config.Paths) *config.Config {
	t.Helper()
	cfg, err := config.Load(p.File)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// refused fails the test unless err is Refused and holds want.
func refused(t *testing.T, what string, err error, want string) {
	t.Helper()
	if !errors.As(err, new(Refused)) || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: %v, want Refused with %q", what, err, want)
	}
}

// CLR-297: a project is found by its key, and by its URL or workspace
// when several share the key; one not found or not told apart is refused.
func TestProjectLookup(t *testing.T) {
	p := twoWEBs(t)
	refused(t, "shared key", SetMemory(p, Project{Key: "WEB"}, "8g"), "several projects have key WEB; choose one with -url or -workspace")
	refused(t, "no such key", SetMemory(p, Project{Key: "NOPE"}, "8g"), "no project NOPE")
	refused(t, "no such URL", SetMemory(p, Project{Key: "WEB", URL: "https://c.example.com"}, "8g"), "no project WEB at https://c.example.com")
	if err := SetMemory(p, Project{Key: "WEB", URL: "https://b.example.com/"}, "8g"); err != nil {
		t.Fatal(err)
	}
	if ps := load(t, p).Projects; ps[0].Memory != "" || ps[1].Memory != "8g" {
		t.Errorf("memory %q, %q; want only b's set", ps[0].Memory, ps[1].Memory)
	}
}

// CLR-297: SetMemory sets a project's memory, lowercased and of at least
// the minimum, and clears it with ""; it needs a project.
func TestSetMemory(t *testing.T) {
	p := twoWEBs(t)
	api := Project{Key: "API"}
	if err := SetMemory(p, api, "256M"); err != nil { // the minimum
		t.Fatal(err)
	}
	if m := load(t, p).Projects[2].Memory; m != "256m" {
		t.Errorf("memory %q, want 256m", m)
	}
	for _, bad := range []string{"255m", "4GB", "4096", "0g", "1.5g"} {
		refused(t, bad, SetMemory(p, api, bad), "256m")
	}
	refused(t, "no project", SetMemory(p, Project{}, "8g"), "project")
	if err := SetMemory(p, api, ""); err != nil {
		t.Fatal(err)
	}
	if m := load(t, p).Projects[2].Memory; m != "" {
		t.Errorf("memory %q after clear, want unset", m)
	}
}

// CLR-297: SetParallel sets max_parallel_runs, runner-wide or a project's
// own, 1 to MaxParallel, and clears it with 0.
func TestSetParallel(t *testing.T) {
	p := twoWEBs(t)
	api := Project{Key: "API"}
	if err := SetParallel(p, Project{}, config.MaxParallel); err != nil {
		t.Fatal(err)
	}
	if err := SetParallel(p, api, 1); err != nil {
		t.Fatal(err)
	}
	if cfg := load(t, p); cfg.MaxParallelRuns != config.MaxParallel || cfg.Projects[2].MaxParallelRuns != 1 || cfg.Projects[0].MaxParallelRuns != 0 {
		t.Errorf("runner-wide %d, API %d, WEB %d; want 10, 1, 0", cfg.MaxParallelRuns, cfg.Projects[2].MaxParallelRuns, cfg.Projects[0].MaxParallelRuns)
	}
	for _, bad := range []int{-1, config.MaxParallel + 1} {
		refused(t, "runner-wide", SetParallel(p, Project{}, bad), "must be 1 to 10")
		refused(t, "API", SetParallel(p, api, bad), "must be 1 to 10")
	}
	refused(t, "URL without a project", SetParallel(p, Project{URL: "https://a.example.com"}, 2), "project")
	if err := SetParallel(p, api, 0); err != nil {
		t.Fatal(err)
	}
	if err := SetParallel(p, Project{}, 0); err != nil {
		t.Fatal(err)
	}
	if cfg := load(t, p); cfg.MaxParallelRuns != 0 || cfg.Projects[2].MaxParallelRuns != 0 {
		t.Errorf("runner-wide %d, API %d after clear; want 0, 0", cfg.MaxParallelRuns, cfg.Projects[2].MaxParallelRuns)
	}
}

// CLR-297: SetTranscriptDays sets transcript_days, runner-wide or a
// project's own, 0 to MaxTranscriptDays, and clears it with nil.
func TestSetTranscriptDays(t *testing.T) {
	p := twoWEBs(t)
	api := Project{Key: "API"}
	if err := SetTranscriptDays(p, Project{}, new(0)); err != nil {
		t.Fatal(err)
	}
	if err := SetTranscriptDays(p, api, new(config.MaxTranscriptDays)); err != nil {
		t.Fatal(err)
	}
	if cfg := load(t, p); cfg.TranscriptDays == nil || *cfg.TranscriptDays != 0 || cfg.TranscriptDaysFor(cfg.Projects[2]) != 365 || cfg.Projects[0].TranscriptDays != nil {
		t.Errorf("config %+v; want 0 runner-wide, 365 for API", cfg)
	}
	for _, bad := range []int{-1, config.MaxTranscriptDays + 1} {
		refused(t, "runner-wide", SetTranscriptDays(p, Project{}, &bad), "must be 0 to 365")
		refused(t, "API", SetTranscriptDays(p, api, &bad), "must be 0 to 365")
	}
	if err := SetTranscriptDays(p, api, nil); err != nil {
		t.Fatal(err)
	}
	if err := SetTranscriptDays(p, Project{}, nil); err != nil {
		t.Fatal(err)
	}
	if cfg := load(t, p); cfg.TranscriptDays != nil || cfg.Projects[2].TranscriptDays != nil {
		t.Errorf("config %+v after clear; want both unset", cfg)
	}
}
