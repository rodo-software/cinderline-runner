package cli

import (
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

func TestAddStoresModelKeys(t *testing.T) {
	u := instance(t, "WEB", repoURL)
	// Anthropic (too long, re-asked), OpenRouter (skipped), OpenAI, Gemini.
	c, out, errb := newCLI(t, lines(u, goodToken, "", "git-tok", strings.Repeat("x", secrets.MaxSecretBytes+1), "sk-ant-1", "", "sk-oai-1", "gem-1"))
	if code := c.Run([]string{"add"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	p := config.Project{URL: u, Key: "WEB"}
	for k, want := range map[secrets.Kind]string{
		secrets.AnthropicCredential: "sk-ant-1", secrets.OpenRouterKey: "", secrets.OpenAIKey: "sk-oai-1", secrets.GeminiKey: "gem-1",
	} {
		if v := secret(t, c, p, k); v != want {
			t.Errorf("%s = %q, want %q", k, v, want)
		}
	}
	for _, s := range []string{"Not stored", "OpenRouter key", "Stored Anthropic", "Stored OpenAI API key for codex in the keychain", "Stored Gemini API key for gemini in the keychain", "Added WEB"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("stdout lacks %q:\n%s", s, out.String())
		}
	}
	if strings.Contains(out.String(), "Stored OpenRouter") {
		t.Error("empty OpenRouter answer stored")
	}
	for _, v := range []string{"sk-ant-1", "sk-oai-1", "gem-1"} {
		if strings.Contains(out.String()+errb.String(), v) {
			t.Errorf("output echoes %q", v)
		}
	}
}

func TestKeySet(t *testing.T) {
	web := config.Project{URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"}
	api := config.Project{URL: "https://a.example.com", Key: "API", Repository: "https://git.example.com/o/api"}

	c, out, errb := newCLI(t, "sk-or-new\n")
	saveConfig(t, c, web, api)
	if _, err := c.Secrets.Set(web, secrets.OpenRouterKey, "sk-or-old"); err != nil {
		t.Fatal(err)
	}
	if code := c.Run([]string{"key", "set", "opencode", "--project", "WEB"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if v := secret(t, c, web, secrets.OpenRouterKey); v != "sk-or-new" {
		t.Errorf("WEB openrouter %q, want replaced", v)
	}
	if v := secret(t, c, api, secrets.OpenRouterKey); v != "" {
		t.Errorf("API openrouter %q, want untouched", v)
	}
	if !strings.Contains(out.String(), "Stored OpenRouter key for opencode in the keychain") {
		t.Errorf("stdout %q", out.String())
	}
	if strings.Contains(out.String()+errb.String(), "sk-or") {
		t.Errorf("output echoes the key: %q %q", out.String(), errb.String())
	}

	for client, k := range map[string]secrets.Kind{"claude": secrets.AnthropicCredential, "codex": secrets.OpenAIKey, "gemini": secrets.GeminiKey} {
		c, _, errb := newCLI(t, "v-"+client+"\n")
		saveConfig(t, c, web)
		if code := c.Run([]string{"key", "set", client, "--project", "WEB"}); code != 0 {
			t.Fatalf("%s: code %d, stderr %q", client, code, errb.String())
		}
		if v := secret(t, c, web, k); v != "v-"+client {
			t.Errorf("%s: %s = %q", client, k, v)
		}
	}

	// Failures store nothing.
	for _, tt := range []struct {
		stdin string
		args  []string
		code  int
	}{
		{"x\n", []string{"key"}, 2},
		{"x\n", []string{"key", "set"}, 2},
		{"x\n", []string{"key", "get", "claude", "--project", "WEB"}, 2},
		{"x\n", []string{"key", "set", "claude"}, 2},
		{"x\n", []string{"key", "set", "claude", "--project", "WEB", "extra"}, 2},
		{"x\n", []string{"key", "set", "aider", "--project", "WEB"}, 1},
		{"x\n", []string{"key", "set", "claude", "--project", "NOPE"}, 1},
		{"x\n", []string{"key", "set", "claude", "--project", "WEB", "-url", "https://b.example.com"}, 1},
		{"", []string{"key", "set", "claude", "--project", "WEB"}, 1},
		{"\n", []string{"key", "set", "claude", "--project", "WEB"}, 1},
	} {
		c, _, errb := newCLI(t, tt.stdin)
		saveConfig(t, c, web)
		if code := c.Run(tt.args); code != tt.code {
			t.Errorf("%v: code %d, want %d, stderr %q", tt.args, code, tt.code, errb.String())
		}
		if v := secret(t, c, web, secrets.AnthropicCredential); v != "" {
			t.Errorf("%v: stored %q", tt.args, v)
		}
	}

	// Two instances with key WEB: -url picks one.
	other := config.Project{URL: "https://b.example.com", Key: "WEB", Repository: "https://git.example.com/o/web2"}
	c, _, errb = newCLI(t, "k\n")
	saveConfig(t, c, web, other)
	if code := c.Run([]string{"key", "set", "claude", "--project", "WEB"}); code != 1 || !strings.Contains(errb.String(), "-url") {
		t.Errorf("ambiguous: code %d, stderr %q", code, errb.String())
	}
	c, _, errb = newCLI(t, "k\n")
	saveConfig(t, c, web, other)
	if code := c.Run([]string{"key", "set", "claude", "--project", "WEB", "-url", "https://b.example.com/"}); code != 0 {
		t.Fatalf("with -url: code %d, stderr %q", code, errb.String())
	}
	if secret(t, c, other, secrets.AnthropicCredential) != "k" || secret(t, c, web, secrets.AnthropicCredential) != "" {
		t.Error("stored on the wrong project")
	}
}
