package environment

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestComposeFiles(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"local include", "include: [sub.yml]", ""},
		{"include env outside", "include: [{path: sub.yml, env_file: ../../outside.env}]", "outside the repository"},
		{"include project outside", "include: [{path: sub.yml, project_directory: ../../outside}]", "outside the repository"},
		{"include absolute", "include: [/etc/compose.yml]", "outside the repository"},
		{"dynamic include", "include: ['${FILE}']", "literal"},
		{"home include", "include: ['~/compose.yml']", "literal"},
		{"remote include", "include: ['https://example.com/compose.yml']", "literal"},
		{"local extends", "services: {agent: {extends: {file: sub.yml, service: base}}}", ""},
		{"numeric local service key", "services: {123: {image: alpine}, agent: {extends: {file: sub.yml, service: base}}}", ""},
		{"numeric service key", "services: {123: {image: alpine}, agent: {extends: {file: ../../outside.yml, service: base}}}", "outside the repository"},
		{"extends outside", "services: {agent: {extends: {file: ../../outside.yml, service: base}}}", "outside the repository"},
		{"dynamic extends", "services: {agent: {extends: {file: '${FILE}', service: base}}}", "literal"},
		{"local long include", "include: [{path: [sub.yml, override.yml], project_directory: local, env_file: local.env}]", ""},
		{"alias", "x-include: &i {path: sub.yml, env_file: ../../outside.env}\ninclude: [*i]", "outside the repository"},
		{"mixed include keys", "include: [{123: ignored, path: sub.yml, env_file: ../../outside.env}]", "outside the repository"},
		{"merge", "x-path: &p {env_file: ../../outside.env}\ninclude: [{<<: *p, path: sub.yml}]", "outside the repository"},
		{"duplicate key", "include: [sub.yml]\ninclude: [/etc/compose.yml]", "already defined"},
		{"multiple documents", "include: [sub.yml]\n---\ninclude: [/etc/compose.yml]", "multiple"},
		{"label file", "services: {agent: {image: alpine, label_file: /etc/labels}}", "label_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "compose")
			if err := os.MkdirAll(filepath.Join(dir, "local"), 0700); err != nil {
				t.Fatal(err)
			}
			for file, body := range map[string]string{"compose.yml": tc.body, "sub.yml": "services: {base: {image: alpine}}", "override.yml": "services: {}", "local.env": "VALUE=local"} {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := checkComposeFiles(root, filepath.Join(dir, "compose.yml"))
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
}

func TestComposeFilesFollowNestedReferences(t *testing.T) {
	for _, body := range []string{"include: [../../outside.yml]", "services: {base: {extends: {file: ../../outside.yml, service: base}}}", "include: [{path: leaf.yml, env_file: ../../outside.env}]"} {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "compose"), 0700); err != nil {
			t.Fatal(err)
		}
		for file, body := range map[string]string{"compose.yml": "include: [sub.yml]", "sub.yml": body} {
			if err := os.WriteFile(filepath.Join(root, "compose", file), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := checkComposeFiles(root, filepath.Join(root, "compose", "compose.yml")); err == nil || !strings.Contains(err.Error(), "outside the repository") {
			t.Fatalf("got %v", err)
		}
	}
}

func TestComposeRefusesIncludeBeforeDockerReadsHostFiles(t *testing.T) {
	for _, field := range []string{"env_file", "project_directory"} {
		t.Run(field, func(t *testing.T) {
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, ".env"), []byte("DB_PASSWORD=outside-secret"), 0600); err != nil {
				t.Fatal(err)
			}
			path := outside
			if field == "env_file" {
				path = filepath.Join(outside, ".env")
			}
			d, calls, _ := composeRunner(t, `{"services":{"agent":{"image":"alpine","environment":{"LEAK":"outside-secret"}}}}`)
			record := d.Exec
			d.Exec = func(ctx context.Context, env []string, argv ...string) (string, error) {
				if slices.Contains(argv, "archive") {
					return tarOf(t, map[string]string{DefaultComposeFile: "include:\n  - path: sub.yml\n    " + field + ": " + filepath.ToSlash(path), filepath.ToSlash(filepath.Join(filepath.Dir(DefaultComposeFile), "sub.yml")): "services: {agent: {image: alpine, environment: {LEAK: '${DB_PASSWORD}'}}}"}), nil
				}
				return record(ctx, env, argv...)
			}
			if _, _, err := d.AgentImage(t.Context()); err == nil || !strings.Contains(err.Error(), "outside the repository") {
				t.Fatalf("got %v", err)
			}
			for _, call := range calls() {
				if slices.Contains(call.Argv, "compose") {
					t.Fatalf("compose ran before refusal: %v", call.Argv)
				}
			}
		})
	}
}

func TestComposeUsesFrozenFile(t *testing.T) {
	const model = `{"services":{"agent":{"image":"alpine","environment":{"VALUE":"${VALUE}"}}}}`
	const resolved = `{"services":{"agent":{"image":"alpine","environment":{"VALUE":"repository"}}}}`
	for _, dotenv := range []bool{false, true} {
		t.Run(fmt.Sprint(dotenv), func(t *testing.T) {
			d, _, _ := composeRunner(t, resolved)
			record := d.Exec
			var original, frozen, envFile string
			var stages []string
			d.Exec = func(ctx context.Context, env []string, argv ...string) (string, error) {
				if slices.Contains(argv, "archive") {
					files := map[string]string{DefaultComposeFile: "services: {agent: {image: alpine, environment: {VALUE: '${VALUE}'}}}"}
					if dotenv {
						files[".env"] = "VALUE=repository"
					}
					return tarOf(t, files), nil
				}
				if argv[1] != "compose" {
					return record(ctx, env, argv...)
				}
				path := argv[5]
				i := slices.Index(argv, "--env-file")
				if i < 0 {
					t.Fatalf("no explicit env file: %v", argv)
				}
				if envFile == "" {
					envFile = argv[i+1]
				} else if envFile != argv[i+1] {
					t.Fatalf("env file changed: %v", argv)
				}
				if slices.Contains(argv, "--no-interpolate") {
					stages = append(stages, "flatten")
					original = path
					if !slices.Contains(argv, "--no-env-resolution") {
						t.Fatal("flatten can read service env files")
					}
					return model, nil
				}
				if frozen == "" {
					frozen = path
				}
				if path == original || path != frozen {
					t.Fatalf("did not keep using frozen file: %v", argv)
				}
				if filepath.Dir(frozen) != filepath.Dir(filepath.Dir(original)) {
					t.Fatalf("frozen file outside src: %s", frozen)
				}
				b, err := os.ReadFile(frozen)
				if err != nil || string(b) != model {
					t.Fatalf("frozen model = %q, %v", b, err)
				}
				info, err := os.Stat(frozen)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("frozen file permissions: %v, %v", info, err)
				}
				if slices.Contains(argv, "config") {
					if slices.Contains(argv, "--no-env-resolution") {
						stages = append(stages, "check paths")
					} else {
						stages = append(stages, "resolve env")
					}
					return resolved, nil
				}
				if slices.Contains(argv, "up") {
					stages = append(stages, "up")
				}
				if slices.Contains(argv, "down") {
					stages = append(stages, "down")
				}
				return "", nil
			}
			p, err := d.composeProject(t.Context(), "test", d.compose.Load())
			if p != nil {
				defer os.RemoveAll(p.dir)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(p.env, "VALUE=repository") {
				t.Fatalf("agent env = %v", p.env)
			}
			b, err := os.ReadFile(envFile)
			if err != nil {
				t.Fatal(err)
			}
			if dotenv && string(b) != "VALUE=repository" || !dotenv && len(b) != 0 {
				t.Fatalf("dotenv = %q", b)
			}
			p.services = []string{"db"}
			if err := d.composeUp(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			if err := d.composeDown(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			if want := []string{"flatten", "check paths", "resolve env", "up", "down"}; !slices.Equal(stages, want) {
				t.Fatalf("stages = %v, want %v", stages, want)
			}
		})
	}
}

func TestComposeChecksEnvFileBeforeReadingIt(t *testing.T) {
	d, _, _ := composeRunner(t, `{"services":{"agent":{"image":"alpine","env_file":[{"path":"/outside/secrets.env"}]}}}`)
	record := d.Exec
	d.Exec = func(ctx context.Context, env []string, argv ...string) (string, error) {
		if argv[1] == "compose" && slices.Contains(argv, "config") && !slices.Contains(argv, "--no-env-resolution") {
			t.Fatal("resolved service env_file before checking path")
		}
		return record(ctx, env, argv...)
	}
	if _, _, err := d.AgentImage(t.Context()); err == nil || !strings.Contains(err.Error(), "outside the repository") {
		t.Fatalf("got %v", err)
	}
}

func TestComposeFilesRefusesChangingDirectoryRecursion(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "compose.yml")
	body := "include: [{path: " + filepath.ToSlash(file) + ", project_directory: nested}]"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkComposeFiles(root, file); err == nil || !strings.Contains(err.Error(), "recursive") {
		t.Fatalf("got %v", err)
	}
}
