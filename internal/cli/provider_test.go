package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

func providers(t *testing.T, c *CLI) []config.Provider {
	t.Helper()
	cfg, err := config.Load(c.Paths.File)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Providers
}

// Decision 2026-11-17-local-model-providers: providers belong to the runner,
// not a project. add-provider stores one and its optional key, runner-wide,
// list-providers shows them, key list of a project does not, remove-provider
// removes both.
func TestAddListRemoveProvider(t *testing.T) {
	web := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"}
	c, out, errb := newCLI(t, "")
	saveConfig(t, c, web)

	for _, bad := range [][]string{
		{"cerebellum"}, // no URL
		{"openrouter", "http://h:1/v1"},
		{"Cere", "http://h:1/v1"},
		{"cerebellum", "http://u:p@h:1/v1"},
		{"--project", "WEB", "cerebellum", "http://h:1/v1"}, // runner-wide: no --project
	} {
		if code := c.Run(append([]string{"add-provider"}, bad...)); code == 0 {
			t.Errorf("add-provider %q: code 0", bad)
		}
	}
	c.Stdin = strings.NewReader(lines("sk-local"))
	if code := c.Run([]string{"add-provider", "cerebellum", "http://localhost:8080/v1"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	c.Stdin = strings.NewReader(lines(""))
	if code := c.Run([]string{"add-provider", "lan", "http://provider.example:8080/v1"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	c.Stdin = strings.NewReader(lines("x"))
	if code := c.Run([]string{"add-provider", "lan", "http://other:1/v1"}); code == 0 {
		t.Error("a second provider with one name was added")
	}
	want := []config.Provider{{Name: "cerebellum", BaseURL: "http://localhost:8080/v1"}, {Name: "lan", BaseURL: "http://provider.example:8080/v1"}}
	if got := providers(t, c); !slices.Equal(got, want) {
		t.Fatalf("providers %v, want %v", got, want)
	}
	if len(projects(t, c)) != 1 {
		t.Error("the project was lost")
	}
	if v := secret(t, c, secrets.Runner, secrets.ProviderKey("cerebellum")); v != "sk-local" {
		t.Errorf("cerebellum key %q", v)
	}
	if v := secret(t, c, web, secrets.ProviderKey("cerebellum")); v != "" {
		t.Errorf("the key is stored for the project: %q", v)
	}
	if v := secret(t, c, secrets.Runner, secrets.ProviderKey("lan")); v != "" {
		t.Errorf("lan key %q, want none", v)
	}
	if strings.Contains(out.String()+errb.String(), "sk-local") {
		t.Error("output shows the key")
	}

	out.Reset()
	if code := c.Run([]string{"list-providers"}); code != 0 ||
		out.String() != "cerebellum\thttp://localhost:8080/v1\tkey set\nlan\thttp://provider.example:8080/v1\tno key\n" {
		t.Errorf("list-providers: code %d, %q", code, out.String())
	}
	out.Reset()
	if code := c.Run([]string{"key", "list", "--project", "WEB"}); code != 0 || strings.Contains(out.String(), "provider") {
		t.Errorf("key list: code %d\n%s", code, out.String())
	}

	if code := c.Run([]string{"remove-provider", "nope"}); code != 1 {
		t.Errorf("remove-provider nope: code %d", code)
	}
	if code := c.Run([]string{"remove-provider", "cerebellum"}); code != 0 {
		t.Fatalf("remove-provider: code %d, stderr %q", code, errb.String())
	}
	if got := providers(t, c); !slices.Equal(got, want[1:]) {
		t.Errorf("after remove: %v", got)
	}
	if v := secret(t, c, secrets.Runner, secrets.ProviderKey("cerebellum")); v != "" {
		t.Errorf("key left after remove: %q", v)
	}
}

// A runner without projects can still have providers.
func TestAddProviderWithoutProjects(t *testing.T) {
	c, _, errb := newCLI(t, lines(""))
	if code := c.Run([]string{"add-provider", "lan", "http://provider.example:8080/v1"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if got := providers(t, c); len(got) != 1 {
		t.Errorf("providers %v", got)
	}
}

func TestSetProviderFreePreservesKey(t *testing.T) {
	c, out, errb := newCLI(t, lines("sk-local"))
	run := func(code int, args ...string) {
		t.Helper()
		if got := c.Run(args); got != code {
			t.Fatalf("%v: code %d, want %d; stderr %q", args, got, code, errb.String())
		}
	}
	const url = "http://localhost:8080/v1"
	run(0, "add-provider", "--free", "lan", url)
	c.Stdin = strings.NewReader("")
	check := func(free bool) {
		t.Helper()
		want := []config.Provider{{Name: "lan", BaseURL: url, Free: free}}
		if got := providers(t, c); !slices.Equal(got, want) {
			t.Fatalf("providers %v, want %v", got, want)
		}
		out.Reset()
		run(0, "list-providers")
		suffix := ""
		if free {
			suffix = "\tfree"
		}
		if want := "lan\t" + url + "\tkey set" + suffix + "\n"; out.String() != want {
			t.Errorf("list-providers = %q, want %q", out.String(), want)
		}
		if got := secret(t, c, secrets.Runner, secrets.ProviderKey("lan")); got != "sk-local" {
			t.Error("provider's key changed")
		}
	}
	check(true)
	run(2, "set-provider")
	run(2, "set-provider", "--free=true", "lan")
	run(2, "set-provider", "lan")
	run(2, "set-provider", "lan", "--free=maybe")
	run(1, "set-provider", "unknown", "--free=false")
	check(true)
	run(0, "set-provider", "lan", "--free=false")
	check(false)
	run(0, "set-provider", "lan", "--free=true")
	check(true)
	out.Reset()
	run(0, "set-provider", "lan", "--free=true")
	if want := "lan is already free\n"; out.String() != want {
		t.Errorf("unchanged set-provider = %q, want %q", out.String(), want)
	}
}

// A stale key under a provider's name never reaches a provider added later
// with that name and no key.
func TestAddProviderWithoutKeyDropsAStaleOne(t *testing.T) {
	c, _, errb := newCLI(t, lines(""))
	if _, err := c.Secrets.Set(secrets.Runner, secrets.ProviderKey("lan"), "old"); err != nil {
		t.Fatal(err)
	}
	if code := c.Run([]string{"add-provider", "lan", "http://provider.example:8080/v1"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if v := secret(t, c, secrets.Runner, secrets.ProviderKey("lan")); v != "" {
		t.Errorf("stale key %q kept", v)
	}
}

// key set|delete provider:NAME changes a provider's key without --project;
// an unknown provider is refused, and the other names still need --project.
func TestKeyProvider(t *testing.T) {
	c, out, errb := newCLI(t, lines(""))
	if code := c.Run([]string{"add-provider", "lan", "http://provider.example:8080/v1"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	c.Stdin = strings.NewReader(lines("sk-new"))
	if code := c.Run([]string{"key", "set", "provider:lan"}); code != 0 {
		t.Fatalf("key set: code %d, stderr %q", code, errb.String())
	}
	if v := secret(t, c, secrets.Runner, secrets.ProviderKey("lan")); v != "sk-new" {
		t.Errorf("key %q", v)
	}
	if strings.Contains(out.String(), "sk-new") {
		t.Error("output shows the key")
	}
	c.Stdin = strings.NewReader(lines("x"))
	if code := c.Run([]string{"key", "set", "provider:nope"}); code != 1 || !strings.Contains(errb.String(), "no provider nope") {
		t.Errorf("unknown provider: code %d, stderr %q", code, errb.String())
	}
	if code := c.Run([]string{"key", "set", "provider:lan", "--project", "WEB"}); code != 2 {
		t.Errorf("provider key with --project: code %d", code)
	}
	if code := c.Run([]string{"key", "delete", "provider:lan"}); code != 0 {
		t.Fatalf("key delete: code %d, stderr %q", code, errb.String())
	}
	if v := secret(t, c, secrets.Runner, secrets.ProviderKey("lan")); v != "" {
		t.Errorf("key left after delete: %q", v)
	}
	if code := c.Run([]string{"key", "set", "claude"}); code != 2 {
		t.Errorf("key set claude without --project: code %d", code)
	}
}

// uninstall removes the providers' keys.
func TestUninstallRemovesProviderKeys(t *testing.T) {
	c, _, _, _, errb := uninstallCLI(t, "")
	cfg, err := config.Load(c.Paths.File)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers = []config.Provider{{Name: "lan", BaseURL: "http://provider.example:8080/v1"}}
	if err := cfg.Save(c.Paths.File); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Secrets.Set(secrets.Runner, secrets.ProviderKey("lan"), "sk"); err != nil {
		t.Fatal(err)
	}
	if code := c.Run([]string{"uninstall", "--yes"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if v := secret(t, c, secrets.Runner, secrets.ProviderKey("lan")); v != "" {
		t.Errorf("provider key left: %q", v)
	}
}
