package run

// NewProject builds a configured project's dependencies.

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
	"github.com/zalando/go-keyring"
)

// A client is built only when its model credential is in the keychain
// (decision 0122), except Claude Code, Codex and Gemini CLI: without a key
// they use the login on the state volume (CSR-58, decisions A and B).
func TestNewProjectBuildsClientsForKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []secrets.Kind
		want []string
	}{
		{"both keys", []secrets.Kind{secrets.AnthropicCredential, secrets.OpenRouterKey}, []string{"claude", "codex", "gemini", "opencode"}},
		{"anthropic only", []secrets.Kind{secrets.AnthropicCredential}, []string{"claude", "codex", "gemini"}},
		{"openrouter only", []secrets.Kind{secrets.OpenRouterKey}, []string{"claude", "codex", "gemini", "opencode"}},
		{"openai only", []secrets.Kind{secrets.OpenAIKey}, []string{"claude", "codex", "gemini"}},
		{"all three keys", []secrets.Kind{secrets.AnthropicCredential, secrets.OpenAIKey, secrets.OpenRouterKey}, []string{"claude", "codex", "gemini", "opencode"}},
		{"gemini only", []secrets.Kind{secrets.GeminiKey}, []string{"claude", "codex", "gemini"}},
		{"all four keys", []secrets.Kind{secrets.AnthropicCredential, secrets.OpenAIKey, secrets.GeminiKey, secrets.OpenRouterKey}, []string{"claude", "codex", "gemini", "opencode"}},
		{"no model key", nil, []string{"claude", "codex", "gemini"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			state := t.TempDir()
			sec := secrets.New(state)
			for _, k := range append([]secrets.Kind{secrets.GitToken}, tc.keys...) {
				if _, err := sec.Set(cfg, k, "secret-"+string(k)); err != nil {
					t.Fatal(err)
				}
			}
			p, err := NewProject(cfg, nil, sec, state, &environment.Docker{Project: cfg.Key, Log: discard()}, discard())
			if err != nil {
				t.Fatalf("NewProject: %v", err)
			}
			var got []string
			for name, cl := range p.Clients {
				if cl == nil {
					t.Errorf("client %s is nil", name)
				}
				got = append(got, name)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("clients = %v, want %v", got, tc.want)
			}
			if p.Workspace == nil || p.Forge == nil {
				t.Errorf("Workspace %v, Forge set %v; want both", p.Workspace, p.Forge != nil)
			}
		})
	}
}

// The Codex client gets the project's OpenAI key, Cinderline's MCP endpoint
// and the project's optional OpenAI-compatible base URL from the local config.
func TestNewProjectCodexFromConfig(t *testing.T) {
	keyring.MockInit()
	state := t.TempDir()
	sec := secrets.New(state)
	pc := cfg
	pc.OpenAIBaseURL = "http://host.docker.internal:11434/v1"
	for _, k := range []secrets.Kind{secrets.GitToken, secrets.OpenAIKey} {
		if _, err := sec.Set(pc, k, "secret-"+string(k)); err != nil {
			t.Fatal(err)
		}
	}
	p, err := NewProject(pc, nil, sec, state, &environment.Docker{Project: pc.Key, Log: discard()}, discard())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	c, ok := p.Clients["codex"].(*client.CLI)
	if !ok {
		t.Fatalf("codex client = %T", p.Clients["codex"])
	}
	if c.Name != "codex" || c.Credential != "secret-openai" || c.BaseURL != pc.OpenAIBaseURL || c.Project != pc.Key || !strings.HasSuffix(c.MCPURL, "/mcp") {
		t.Errorf("codex client = %+v", c)
	}
}

// The Gemini client gets the project's Gemini key, Cinderline's MCP endpoint
// and, from the local config, whether that key is a Vertex AI express-mode key.
func TestNewProjectGeminiFromConfig(t *testing.T) {
	keyring.MockInit()
	state := t.TempDir()
	sec := secrets.New(state)
	pc := cfg
	pc.GeminiVertex = true
	for _, k := range []secrets.Kind{secrets.GitToken, secrets.GeminiKey} {
		if _, err := sec.Set(pc, k, "secret-"+string(k)); err != nil {
			t.Fatal(err)
		}
	}
	p, err := NewProject(pc, nil, sec, state, &environment.Docker{Project: pc.Key, Log: discard()}, discard())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	c, ok := p.Clients["gemini"].(*client.CLI)
	if !ok {
		t.Fatalf("gemini client = %T", p.Clients["gemini"])
	}
	if c.Name != "gemini" || c.Credential != "secret-gemini" || !c.Vertex || c.Project != pc.Key || !strings.HasSuffix(c.MCPURL, "/mcp") {
		t.Errorf("gemini client = %+v", c)
	}
}

// Without the git token the project cannot fetch or push: NewProject says so
// instead of starting a slot that fails at its first run.
func TestNewProjectNeedsGitToken(t *testing.T) {
	keyring.MockInit()
	state := t.TempDir()
	sec := secrets.New(state)
	if _, err := sec.Set(cfg, secrets.AnthropicCredential, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewProject(cfg, nil, sec, state, &environment.Docker{Project: cfg.Key, Log: discard()}, discard()); err == nil {
		t.Fatal("NewProject without a git token: no error")
	}
}

// The forge is the one the project names, on the configured repository; none
// named is told from the host.
func TestNewProjectForgeByKind(t *testing.T) {
	for _, tc := range []struct {
		kind, repository string
		want             any
	}{
		{"github", "https://github.com/o/r", &forge.GitHub{}},
		{"gitlab", "https://gitlab.com/o/r", &forge.GitLab{}},
		{"forgejo", "https://git.example/o/r", &forge.Forgejo{}},
		{"bitbucket", "https://bitbucket.org/o/r", &forge.Bitbucket{}}, // Bitbucket Cloud only (decision 2026-11-04-bitbucket-cloud)
		{"", "https://github.com/o/r", &forge.GitHub{}},                // none named: by host
		{"", "https://git.example/o/r", &forge.Forgejo{}},
	} {
		keyring.MockInit()
		state := t.TempDir()
		sec := secrets.New(state)
		pc := cfg
		pc.Repository = tc.repository
		if _, err := sec.Set(pc, secrets.GitToken, "git"); err != nil {
			t.Fatal(err)
		}
		p, err := NewProject(pc, nil, sec, state, &environment.Docker{Project: pc.Key, Log: discard()}, discard())
		if err != nil {
			t.Fatal(err)
		}
		fg, err := p.Forge(tc.kind)
		if err != nil {
			t.Errorf("Forge(%q): %v", tc.kind, err)
			continue
		}
		if got, w := typeName(fg), typeName(tc.want); got != w {
			t.Errorf("Forge(%q) = %s, want %s", tc.kind, got, w)
		}
		if tc.kind == "forgejo" {
			if _, err := p.Forge("svn"); err == nil {
				t.Error("Forge(\"svn\"): no error")
			}
		}
	}
}

func typeName(v any) string { return fmt.Sprintf("%T", v) }

// CSR-97: a project in a workspace names its volumes, containers and labels
// by workspace and key, so its namesake in another workspace on the same URL
// does not share them.
func TestNewProjectNamesByWorkspace(t *testing.T) {
	keyring.MockInit()
	state := t.TempDir()
	sec := secrets.New(state)
	pc := cfg
	pc.Workspace = "acme"
	if _, err := sec.Set(pc, secrets.GitToken, "git"); err != nil {
		t.Fatal(err)
	}
	p, err := NewProject(pc, nil, sec, state, &environment.Docker{Project: pc.ID(), Log: discard()}, discard())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	id := pc.Workspace + "-" + pc.Key
	if ws := p.Workspace.(*workspace.Docker); ws.Project != id {
		t.Errorf("workspace project %q, want %q", ws.Project, id)
	}
	if c := p.Clients["claude"].(*client.CLI); c.Project != id {
		t.Errorf("claude project %q, want %q", c.Project, id)
	}
	if got, want := workspace.Volume(pc.ID(), "repo"), "cinderline-"+id+"-repo"; got != want {
		t.Errorf("repo volume %q, want %q", got, want)
	}
	if got, want := workspace.ProjectLabel(pc.ID()), "cinderline.project="+id; got != want {
		t.Errorf("label %q, want %q", got, want)
	}
}

// Decision 2026-11-17-local-model-providers: on a runner with providers
// every project runs OpenCode without an OpenRouter key, and its client gets
// each provider's URL and its runner-wide key when one is stored.
func TestNewProjectOpenCodeProviders(t *testing.T) {
	keyring.MockInit()
	state := t.TempDir()
	sec := secrets.New(state)
	pc := cfg
	providers := []config.Provider{{Name: "cerebellum", BaseURL: "http://localhost:8080/v1"}, {Name: "lan", BaseURL: "http://studio.lan:1234/v1"}}
	if _, err := sec.Set(pc, secrets.GitToken, "g"); err != nil {
		t.Fatal(err)
	}
	if _, err := sec.Set(secrets.Runner, secrets.ProviderKey("cerebellum"), "sk-local"); err != nil {
		t.Fatal(err)
	}
	p, err := NewProject(pc, providers, sec, state, &environment.Docker{Project: pc.Key, Log: discard()}, discard())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	c, ok := p.Clients["opencode"].(*client.CLI)
	if !ok {
		t.Fatalf("opencode client = %T", p.Clients["opencode"])
	}
	want := map[string]client.Provider{"cerebellum": {BaseURL: "http://localhost:8080/v1", Key: "sk-local"}, "lan": {BaseURL: "http://studio.lan:1234/v1"}}
	if c.Credential != "" || !reflect.DeepEqual(c.Providers, want) {
		t.Errorf("opencode client = %+v", c)
	}
	if !slices.Equal(p.Providers, providers) {
		t.Errorf("project providers = %v", p.Providers)
	}
}
