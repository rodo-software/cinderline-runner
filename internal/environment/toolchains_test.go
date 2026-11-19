package environment

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
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
			src := t.TempDir()
			if err := os.CopyFS(filepath.Join(src, filepath.FromSlash(dir)), os.DirFS(filepath.Join(repo, filepath.FromSlash(dir)))); err != nil {
				t.Fatal(err)
			}
			if src, err = filepath.EvalSymlinks(src); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(src, filepath.FromSlash(dir), "compose.yml")
			if err := checkComposeFiles(src, file); err != nil {
				t.Fatal(err)
			}
			checkComposeRoundTrip(t, src, file)
		})
	}
}

// checkComposeRoundTrip exercises the same saved-model boundary as composeProject.
func checkComposeRoundTrip(t *testing.T, src, file string) {
	t.Helper()
	empty := filepath.Join(src, "empty.env")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"docker", "compose", "--project-name", "example", "--file", file, "--env-file", empty}
	d := &Docker{}
	out, err := d.composeExec(t.Context(), append(slices.Clip(args), "config", "--format", "json")...)
	if err != nil {
		t.Fatal(err)
	}
	var cfg composeConfig
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.check(src, "agent"); err != nil {
		t.Errorf("check: %v", err)
	}
	flat, err := d.composeExec(t.Context(), append(slices.Clip(args), "config", "--no-interpolate", "--no-env-resolution", "--format", "json")...)
	if err != nil {
		t.Fatal(err)
	}
	frozen := filepath.Join(src, "frozen.json")
	if err := os.WriteFile(frozen, []byte(flat), 0o600); err != nil {
		t.Fatal(err)
	}
	args[5] = frozen
	roundTrip, err := d.composeExec(t.Context(), append(slices.Clip(args), "config", "--format", "json")...)
	if err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	var before, after any
	if err := json.Unmarshal([]byte(out), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(roundTrip), &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("round-trip changed config:\nbefore: %s\nafter: %s", out, roundTrip)
	}
}

func TestComposeFixturesRoundTrip(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("needs docker compose")
	}
	for name, files := range map[string]map[string]string{
		"include": {
			"compose.yml":     "include: [parts/agent.yml]",
			"parts/agent.yml": "services: {agent: {image: alpine, environment: {VALUE: '${VALUE:-default}'}}}",
		},
		"extends": {
			"compose.yml":      "services: {agent: {extends: {file: parts/base.yml, service: base}}}",
			"parts/base.yml":   "services: {base: {extends: {file: nested.yml, service: shared}}}",
			"parts/nested.yml": "services: {shared: {image: alpine}}",
		},
		"service env file and dollars": {
			"compose.yml": "services: {agent: {image: alpine, env_file: local.env, command: ['echo', '$$VALUE'], environment: {DEFAULT: '${VALUE:-default}'}}}",
			"local.env":   "VALUE=local",
		},
		"database volume and build": {
			"compose.yml": "services:\n  agent:\n    build: .\n    environment: {DB_HOST: db}\n  db:\n    image: postgres:17\n    volumes: [data:/var/lib/postgresql/data]\nvolumes: {data: {}}",
			"Dockerfile":  "FROM alpine",
		},
	} {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			var err error
			if src, err = filepath.EvalSymlinks(src); err != nil {
				t.Fatal(err)
			}
			if err := untar(src, tarOf(t, files)); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(src, "compose.yml")
			if err := checkComposeFiles(src, file); err != nil {
				t.Fatal(err)
			}
			checkComposeRoundTrip(t, src, file)
		})
	}
}
