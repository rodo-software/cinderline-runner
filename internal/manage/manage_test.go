package manage

import (
	"errors"
	"path/filepath"
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
