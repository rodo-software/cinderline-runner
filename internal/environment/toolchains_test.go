package environment

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/name"
)

// CLR-186: the README's toolchain examples, this repository's own
// .cinderline/ among them, pass the compose check. It needs docker compose,
// which an agent's container lacks, so it skips there.
func TestToolchainExamplesPassTheCheck(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("needs docker compose")
	}
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// Paths in the normalised config are compared with src, so no symlink in it.
	if repo, err = filepath.EvalSymlinks(repo); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"." + name.Name, "docs/toolchains/python", "docs/toolchains/node"} {
		t.Run(dir, func(t *testing.T) {
			file := filepath.Join(repo, filepath.FromSlash(dir), "compose.yml")
			out, err := (&Docker{}).composeExec(t.Context(), "docker", "compose", "--project-name", "example", "--file", file, "config", "--format", "json")
			if err != nil {
				t.Fatal(err)
			}
			var cfg composeConfig
			if err := json.Unmarshal([]byte(out), &cfg); err != nil {
				t.Fatal(err)
			}
			if err := cfg.check(repo, "agent"); err != nil {
				t.Errorf("check: %v", err)
			}
		})
	}
}
