package client

// Acceptance tests for CSR-58: `cinderline login <client>` runs the CLI's own
// sign-in in an interactive container with the project's state volume at the
// client's config dir (review item 5: the docker argv per client).

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

var loginMount = tools.Mount{Args: []string{"-v", "tools:" + tools.Path + ":ro"}, Env: []string{"DISABLE_AUTOUPDATER=1"}}

func loginPrefix(project, client string) []string {
	return []string{
		"docker", "run", "--rm", "--interactive", "--tty",
		"--user", workspace.AgentUser,
		"-v", StateVolume(project, client) + ":" + StatePath,
		"-v", "tools:" + tools.Path + ":ro",
		"-e", "DISABLE_AUTOUPDATER=1",
	}
}

func TestLoginArgvClaude(t *testing.T) {
	got, err := LoginArgv("claude", "WEB", "img:1", loginMount)
	if err != nil {
		t.Fatal(err)
	}
	want := append(loginPrefix("WEB", "claude"),
		"-e", "CLAUDE_CONFIG_DIR="+StatePath+"/claude",
		"img:1", tools.Path+"/bin/claude", "auth", "login")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv\n got %q\nwant %q", got, want)
	}
}

func TestLoginArgvCodex(t *testing.T) {
	got, err := LoginArgv("codex", "WEB", "img:1", loginMount)
	if err != nil {
		t.Fatal(err)
	}
	want := append(loginPrefix("WEB", "codex"),
		"-e", "CODEX_HOME="+StatePath+"/codex",
		"img:1", "sh", "-c", `mkdir -p "$CODEX_HOME" && exec "$0" "$@"`,
		tools.Path+"/bin/codex", "login", "--device-auth")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv\n got %q\nwant %q", got, want)
	}
}

// OpenCode's login only stores provider API keys; its runs use the project's
// OpenRouter key from the keychain, so login refuses with a clear message.
func TestLoginArgvOpenCodeHasNoLogin(t *testing.T) {
	got, err := LoginArgv("opencode", "WEB", "img:1", loginMount)
	if err == nil || got != nil {
		t.Fatalf("argv %q, err %v; want nil argv and an error", got, err)
	}
	if !strings.Contains(err.Error(), "OpenRouter") {
		t.Errorf("error %q does not point at the OpenRouter key", err)
	}
}

func TestLoginArgvUnknownClient(t *testing.T) {
	if got, err := LoginArgv("cursor", "WEB", "img:1", loginMount); err == nil || got != nil {
		t.Fatalf("argv %q, err %v; want nil argv and an error", got, err)
	}
}

// Decision A on CSR-58: without a keychain credential the run sets no key, so
// Claude Code uses the login on the state volume.
func TestClaudeEnvWithoutCredentialUsesLogin(t *testing.T) {
	got := claudeEnv("", Turn{})
	if want := []string{"CLAUDE_CONFIG_DIR=" + StatePath + "/claude"}; !reflect.DeepEqual(got[:1], want) {
		t.Fatalf("env %q", got)
	}
	for _, kv := range got {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") || strings.HasPrefix(kv, "CLAUDE_CODE_OAUTH_TOKEN=") {
			t.Errorf("env has %q; want no key so the stored login is used", kv)
		}
	}
}

func TestCodexEnvWithoutKeyUsesLogin(t *testing.T) {
	if got, want := codexEnv("", Turn{Token: "tok"}), []string{"CODEX_HOME=/state/codex", "CINDERLINE_TOKEN=tok"}; !reflect.DeepEqual(got, want) {
		t.Errorf("env %q, want %q", got, want)
	}
}

// Decision B on CSR-58: Gemini CLI signs in with Google. Its headless mode
// refuses a manual sign-in, so the login starts the interactive CLI with
// oauth-personal preselected; with no display in the container it prints a
// link and asks for the code. On musl it needs the volume's libraries, as a run.
func TestLoginArgvGemini(t *testing.T) {
	m := loginMount
	m.OpenCodeEnv = []string{"LD_LIBRARY_PATH=" + tools.Path + "/lib"}
	got, err := LoginArgv("gemini", "WEB", "img:1", m)
	if err != nil {
		t.Fatal(err)
	}
	want := append(loginPrefix("WEB", "gemini"),
		"-e", "LD_LIBRARY_PATH="+tools.Path+"/lib",
		"-e", "GEMINI_CLI_HOME="+StatePath+"/gemini",
		"img:1", "sh", "-c",
		`mkdir -p "$GEMINI_CLI_HOME/.gemini" && printf '%s' '{"security":{"auth":{"selectedType":"oauth-personal"}}}' > "$GEMINI_CLI_HOME/.gemini/settings.json" && exec "$0" "$@"`,
		tools.Path+"/bin/node", tools.Path+"/gemini/bundle/gemini.js")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv\n got %q\nwant %q", got, want)
	}
}

// Without a keychain key a Gemini run sets no key and selects the Google
// login stored on the state volume; the rest of the settings stay as with a key.
func TestGeminiEnvWithoutKeyUsesLogin(t *testing.T) {
	for _, vertex := range []bool{false, true} {
		got := geminiEnv("", vertex, Turn{Token: "tok"}, "https://cs.example/mcp")
		if len(got) != 3 || got[0] != "GEMINI_CLI_HOME=/state/gemini" || got[2] != "CINDERLINE_TOKEN=tok" {
			t.Fatalf("vertex %v: env %q, want home, settings, token", vertex, got)
		}
		settings, ok := strings.CutPrefix(got[1], "CINDERLINE_GEMINI_SETTINGS=")
		if !ok {
			t.Fatalf("vertex %v: env[1] = %q, want the settings", vertex, got[1])
		}
		want := decodeSettings(t, geminiSettingsWithAuth(Turn{Token: "tok"}, "https://cs.example/mcp", geminiAuthType(false)))
		want["security"] = map[string]any{"auth": map[string]any{"selectedType": "oauth-personal"}}
		if s := decodeSettings(t, settings); !reflect.DeepEqual(s, want) {
			t.Errorf("vertex %v: settings =\n%v\nwant\n%v", vertex, s, want)
		}
	}
}
