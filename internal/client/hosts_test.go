package client

import (
	"reflect"
	"testing"
)

// CSR-63: each client declares the provider host:port pairs its run needs,
// for the run's network allowlist. A run reaches only its own provider:
// a Claude Code run cannot reach OpenRouter, and the reverse.
func TestHostsAreTheProviderHostAndPort(t *testing.T) {
	tests := []struct {
		name   string
		client interface{ Hosts() []string }
		want   []string
	}{
		// With a key: the provider's API host only.
		{"claude key", &CLI{Name: "claude", Credential: "sk-ant-api03-k"}, []string{"api.anthropic.com:443"}},
		{"claude setup-token", &CLI{Name: "claude", Credential: "sk-ant-oat01-t"}, []string{"api.anthropic.com:443"}},
		{"opencode", &CLI{Name: "opencode", Credential: "sk-or-k"}, []string{"openrouter.ai:443"}},
		{"codex key", &CLI{Name: "codex", Credential: "sk-oa"}, []string{"api.openai.com:443"}},
		{"codex https base URL", &CLI{Name: "codex", Credential: "sk-oa", BaseURL: "https://llm.example.com/v1"}, []string{"llm.example.com:443"}},
		{"codex http base URL", &CLI{Name: "codex", BaseURL: "http://llm.example.com/v1"}, []string{"llm.example.com:80"}},
		{"codex base URL with port", &CLI{Name: "codex", BaseURL: "http://LLM.example.com:8080/v1"}, []string{"llm.example.com:8080"}},
		{"codex bad base URL", &CLI{Name: "codex", BaseURL: "::"}, nil},
		{"gemini key", &CLI{Name: "gemini", Credential: "AIza-k"}, []string{"generativelanguage.googleapis.com:443"}},
		{"gemini vertex", &CLI{Name: "gemini", Credential: "AIza-k", Vertex: true}, []string{"aiplatform.googleapis.com:443"}},
		// Willem's decision on CSR-63 (option A): without a key the run uses the
		// sign-in `login` stored (CSR-58), which also needs the provider's
		// sign-in hosts. Unchecked against the CLIs; a missing one shows up as a
		// refused host in the run's log.
		{"claude sign-in", &CLI{Name: "claude"}, []string{"api.anthropic.com:443", "console.anthropic.com:443", "platform.claude.com:443"}},
		{"codex sign-in", &CLI{Name: "codex"}, []string{"api.openai.com:443", "chatgpt.com:443", "auth.openai.com:443"}},
		{"gemini sign-in", &CLI{Name: "gemini"}, []string{"generativelanguage.googleapis.com:443", "cloudcode-pa.googleapis.com:443", "oauth2.googleapis.com:443"}},
	}
	for _, tt := range tests {
		if got := tt.client.Hosts(); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: Hosts() = %q, want %q", tt.name, got, tt.want)
		}
	}
}
