package cli

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

var (
	uninstallA     = config.Project{URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/a"}
	uninstallB     = config.Project{URL: "https://b.example.com", Key: "API", Repository: "https://git.example.com/o/b"}
	uninstallKinds = []secrets.Kind{secrets.RunnerToken, secrets.GitToken, secrets.AnthropicCredential, secrets.OpenRouterKey, secrets.OpenAIKey, secrets.GeminiKey}
)

// fakeDocker records every docker argv and answers the list commands with ids.
type fakeDocker struct {
	calls [][]string
	lists map[string]string // argv joined by spaces -> output
	fail  error             // returned for every call when set
}

func (f *fakeDocker) exec(_ context.Context, _ []string, argv ...string) (string, error) {
	f.calls = append(f.calls, argv)
	if f.fail != nil {
		return "", f.fail
	}
	return f.lists[strings.Join(argv, " ")], nil
}

// uninstallCLI has A's secrets in the keychain and B's in the fallback file,
// a fake docker and a fake service remover.
func uninstallCLI(t *testing.T, stdin string) (*CLI, *fakeDocker, *[]string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	c, out, errb := newCLI(t, stdin)
	saveConfig(t, c, uninstallA, uninstallB)
	keyring.MockInitWithError(errors.New("no keychain"))
	for _, k := range uninstallKinds {
		if s, err := c.Secrets.Set(uninstallB, k, "b"); err != nil || s != secrets.File {
			t.Fatalf("set B %s: %v, %v", k, s, err)
		}
	}
	keyring.MockInit()
	for _, k := range uninstallKinds {
		if s, err := c.Secrets.Set(uninstallA, k, "a"); err != nil || s != secrets.Keychain {
			t.Fatalf("set A %s: %v, %v", k, s, err)
		}
	}
	d := &fakeDocker{lists: map[string]string{}}
	c.exec = d.exec
	var removed []string
	c.removeService = func(exe string) error { removed = append(removed, exe); return nil }
	return c, d, &removed, out, errb
}

func untouched(t *testing.T, c *CLI, d *fakeDocker, removed []string) {
	t.Helper()
	if len(d.calls) != 0 || len(removed) != 0 {
		t.Errorf("docker %v, service %v; want nothing", d.calls, removed)
	}
	if len(projects(t, c)) != 2 || secret(t, c, uninstallA, secrets.RunnerToken) == "" || secret(t, c, uninstallB, secrets.RunnerToken) == "" {
		t.Error("config or secrets changed")
	}
}

// TestUninstallDockerByLabel: containers, networks and volumes by the project
// label key, the tools volumes by name, and the runner's own images.
func TestUninstallDockerByLabel(t *testing.T) {
	c, d, _, _, errb := uninstallCLI(t, "")
	label := "label=" + name.Name + ".project"
	tools := "name=" + name.Name + "-tools-"
	images := "reference=" + name.Name + "-default"
	d.lists = map[string]string{
		"docker ps --all --quiet --filter " + label:                            "c1\nc2\n",
		"docker network ls --quiet --filter " + label:                          "n1\n",
		"docker volume ls --quiet --filter " + label:                           "v1\nv2\n",
		"docker volume ls --quiet --filter " + tools:                           name.Name + "-tools-abc\n",
		"docker image ls --format {{.Repository}}:{{.Tag}} --filter " + images: name.Name + "-default:123abc\n",
	}
	if code := c.Run([]string{"uninstall", "--yes"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	want := [][]string{
		{"docker", "ps", "--all", "--quiet", "--filter", label},
		{"docker", "rm", "--force", "--volumes", "c1", "c2"},
		{"docker", "network", "ls", "--quiet", "--filter", label},
		{"docker", "network", "rm", "n1"},
		{"docker", "volume", "ls", "--quiet", "--filter", label},
		{"docker", "volume", "rm", "--force", "v1", "v2"},
		{"docker", "volume", "ls", "--quiet", "--filter", tools},
		{"docker", "volume", "rm", "--force", name.Name + "-tools-abc"},
		{"docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", images},
		{"docker", "image", "rm", "--force", name.Name + "-default:123abc"},
	}
	if !reflect.DeepEqual(d.calls, want) {
		t.Errorf("docker calls\n got %q\nwant %q", d.calls, want)
	}
}

// TestUninstallDockerNothingToRemove: empty lists run no rm command.
func TestUninstallDockerNothingToRemove(t *testing.T) {
	c, d, _, _, errb := uninstallCLI(t, "")
	if code := c.Run([]string{"uninstall", "--yes"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	for _, argv := range d.calls {
		if argv[2] == "rm" || argv[1] == "rm" {
			t.Errorf("rm with nothing listed: %q", argv)
		}
	}
	if len(d.calls) != 5 {
		t.Errorf("%d docker calls, want the 5 lists", len(d.calls))
	}
}

// TestUninstallDeletesSecretsConfigAndService: both stores emptied, config
// and state folders gone, the service removed for this executable.
func TestUninstallDeletesSecretsConfigAndService(t *testing.T) {
	c, _, removed, _, errb := uninstallCLI(t, "")
	if code := c.Run([]string{"uninstall", "--yes"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	for _, p := range []config.Project{uninstallA, uninstallB} {
		for _, k := range uninstallKinds {
			if _, _, err := c.Secrets.Get(p, k); !errors.Is(err, secrets.ErrNotFound) {
				t.Errorf("%s of %s still there: %v", k, p.Key, err)
			}
		}
	}
	for _, dir := range []string{c.Paths.Dir, c.Paths.State} {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s still exists: %v", dir, err)
		}
	}
	exe, _ := os.Executable()
	if !reflect.DeepEqual(*removed, []string{exe}) {
		t.Errorf("service removed for %v, want [%s]", *removed, exe)
	}
}

// TestUninstallRevokeList: each project's URL and key, the binary's path and
// its folder for the PATH line the installer may have added.
func TestUninstallRevokeList(t *testing.T) {
	c, _, _, out, errb := uninstallCLI(t, "")
	if code := c.Run([]string{"uninstall", "--yes"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	exe, _ := os.Executable()
	for _, want := range []string{
		"Revoke the runner token of each project in Cinderline:\n",
		"  WEB at https://a.example.com\n",
		"  API at https://b.example.com\n",
		"Delete the binary yourself: " + exe + "\n",
		"If the installer added " + filepath.Dir(exe) + " to your PATH, remove it from your shell profile or user PATH yourself\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q lacks %q", out.String(), want)
		}
	}
}

// TestUninstallConfirmation: only yes (or y) goes ahead; anything else,
// including end of input, changes nothing and exits 1.
func TestUninstallConfirmation(t *testing.T) {
	for _, in := range []string{"", "\n", "no\n", "yess\n"} {
		c, d, removed, _, _ := uninstallCLI(t, in)
		if code := c.Run([]string{"uninstall"}); code != 1 {
			t.Errorf("answer %q: code %d, want 1", in, code)
		}
		untouched(t, c, d, *removed)
	}
	for _, in := range []string{"yes\n", "Y\n", " YES \n"} {
		c, _, removed, out, errb := uninstallCLI(t, in)
		if code := c.Run([]string{"uninstall"}); code != 0 {
			t.Fatalf("answer %q: code %d, stderr %q", in, code, errb.String())
		}
		if len(*removed) != 1 || len(projects(t, c)) != 0 {
			t.Errorf("answer %q did not uninstall", in)
		}
		if !strings.Contains(out.String(), "yes") {
			t.Errorf("prompt %q does not say to type yes", out.String())
		}
	}
}

// TestUninstallServiceErrorIsAWarning: no installed service is the common
// case, so a failing service removal is reported and the rest goes on.
func TestUninstallServiceErrorIsAWarning(t *testing.T) {
	c, _, _, _, errb := uninstallCLI(t, "")
	c.removeService = func(string) error { return errors.New("not installed") }
	if code := c.Run([]string{"uninstall", "--yes"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "not installed") {
		t.Errorf("stderr %q lacks the service error", errb.String())
	}
	if len(projects(t, c)) != 0 {
		t.Error("config not removed")
	}
}

// TestUninstallDockerErrorKeepsConfig: when docker fails (not running),
// config and secrets stay, so a second uninstall can finish the job.
func TestUninstallDockerErrorKeepsConfig(t *testing.T) {
	c, d, _, _, errb := uninstallCLI(t, "")
	d.fail = errors.New("Cannot connect to the Docker daemon")
	if code := c.Run([]string{"uninstall", "--yes"}); code != 1 {
		t.Fatalf("code %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "Cannot connect") {
		t.Errorf("stderr %q lacks the docker error", errb.String())
	}
	if len(projects(t, c)) != 2 || secret(t, c, uninstallA, secrets.RunnerToken) == "" || secret(t, c, uninstallB, secrets.RunnerToken) == "" {
		t.Error("config or secrets removed despite the docker error")
	}
}

// TestUninstallUsage: extra arguments are a usage error.
func TestUninstallUsage(t *testing.T) {
	c, d, removed, _, errb := uninstallCLI(t, "yes\n")
	if code := c.Run([]string{"uninstall", "x"}); code != 2 || !strings.Contains(errb.String(), "usage:") {
		t.Errorf("code %d, stderr %q", code, errb.String())
	}
	untouched(t, c, d, *removed)
}
