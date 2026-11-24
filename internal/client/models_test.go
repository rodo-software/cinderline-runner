package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// listExec answers the listing's docker run with out and records its argv
// and environment; the state volume's preparation succeeds.
type listExec struct {
	out       string
	err       error
	argv, env []string
}

func (x *listExec) exec(_ context.Context, env []string, argv ...string) (string, error) {
	if slices.Contains(argv, "--user") && !slices.Contains(argv, "--network") {
		x.argv, x.env = argv, env
		return x.out, x.err
	}
	return "", nil // docker volume create, chown
}

// vendor serves a model list at any path and keeps the headers it saw.
func vendor(t *testing.T, status int, body string, url *string) *http.Header {
	t.Helper()
	var seen http.Header
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	old := *url
	*url = s.URL + "/models"
	t.Cleanup(func() { *url = old })
	return &seen
}

const claudeInit = `noise on stderr
{"type":"control_response","response":{"subtype":"success","request_id":"models","response":{"commands":[],"models":[{"value":"default","resolvedModel":"claude-opus-5-5","displayName":"Default"},{"value":"fable","resolvedModel":"claude-fable-5-2"},{"value":"opus","resolvedModel":"claude-opus-5-5"},{"value":"sonnet","resolvedModel":"claude-sonnet-5-5"},{"value":"claude-haiku-5-5"}],"current_model":"opus"}}}`

var listMount = tools.Mount{Args: []string{"-v", "tools:" + tools.Path + ":ro"}, Env: []string{"DISABLE_AUTOUPDATER=1"}}

// Decision 2026-11-24-model-lists: Claude Code lists its models when asked
// initialize, in a container with its state volume and no turn; each alias
// is reported as the full id it resolves to, the current model first.
func TestClaudeListsItsOwnModels(t *testing.T) {
	x := &listExec{out: claudeInit}
	c := &CLI{Name: "claude", Project: "WEB", Exec: x.exec, Tools: listMount}
	got, err := c.Models(context.Background(), "img:1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude-opus-5-5", "claude-fable-5-2", "claude-sonnet-5-5", "claude-haiku-5-5"}; !slices.Equal(got, want) {
		t.Errorf("models = %q, want %q", got, want)
	}
	argv := strings.Join(x.argv, " ")
	for _, part := range []string{"--user " + workspace.AgentUser, "-v " + StateVolume("WEB", "claude") + ":" + StatePath, "-v tools:" + tools.Path + ":ro", "img:1 " + tools.Path + "/bin/node -e", tools.Path + "/bin/claude -p --input-format stream-json --output-format stream-json --verbose"} {
		if !strings.Contains(argv, part) {
			t.Errorf("argv %q lacks %q", argv, part)
		}
	}
	if !slices.Contains(x.env, "REQ="+claudeAsk) || !slices.Contains(x.argv, "REQ") {
		t.Errorf("the request goes by name in REQ: env %q", x.env)
	}
}

// A setup-token or the stored login reports the CLI's list only.
func TestClaudeWithASetupTokenListsTheCLIsModels(t *testing.T) {
	seen := vendor(t, 200, `{"data":[{"id":"claude-3-haiku"}]}`, &anthropicModels)
	x := &listExec{out: claudeInit}
	c := &CLI{Name: "claude", Project: "WEB", Exec: x.exec, Credential: "sk-ant-oat01-x"}
	got, err := c.Models(context.Background(), "img:1")
	if err != nil || len(got) != 4 || *seen != nil {
		t.Errorf("models = %q, %v; Anthropic asked: %v", got, err, *seen != nil)
	}
	if strings.Contains(strings.Join(x.argv, " "), "sk-ant") || !slices.Contains(x.env, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-x") {
		t.Errorf("the token goes by name only: argv %q", x.argv)
	}
}

// Willem, CLR-218, answer B: with an API key, Claude Code offers every model
// of Anthropic's list, after the default the CLI names.
func TestClaudeWithAnAPIKeyListsAnthropicsModels(t *testing.T) {
	seen := vendor(t, 200, `{"data":[{"id":"claude-sonnet-5-5"},{"id":"claude-opus-5-5"},{"id":"claude-3-haiku"}],"has_more":false}`, &anthropicModels)
	c := &CLI{Name: "claude", Project: "WEB", Exec: (&listExec{out: claudeInit}).exec, Credential: "sk-ant-api03-x"}
	got, err := c.Models(context.Background(), "img:1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-3-haiku"}; !slices.Equal(got, want) {
		t.Errorf("models = %q, want %q", got, want)
	}
	if h := *seen; h.Get("x-api-key") != "sk-ant-api03-x" || h.Get("anthropic-version") == "" {
		t.Errorf("asked with headers %v", h)
	}
	vendor(t, 401, `{}`, &anthropicModels)
	if got, err := c.Models(context.Background(), "img:1"); err == nil {
		t.Errorf("a refused key listed %q", got)
	}
}

// Codex app-server's model/list: hidden models left out, the default first.
func TestCodexListsItsOwnModels(t *testing.T) {
	x := &listExec{out: `{"id":0,"result":{"userAgent":"codex"}}
{"method":"account/updated","params":{}}
{"id":1,"result":{"data":[{"id":"gpt-6-astra","model":"gpt-6-astra","isDefault":false},{"id":"gpt-6.1-sol","model":"gpt-6.1-sol","isDefault":true},{"id":"codex-auto-review","model":"codex-auto-review","hidden":true},{"id":"gpt-6-luna","model":"gpt-6-luna"}],"nextCursor":null}}`}
	c := &CLI{Name: "codex", Project: "WEB", Exec: x.exec, Credential: "sk-proj", BaseURL: "https://gw.example/v1"}
	got, err := c.Models(context.Background(), "img:1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"gpt-6.1-sol", "gpt-6-astra", "gpt-6-luna"}; !slices.Equal(got, want) {
		t.Errorf("models = %q, want %q", got, want)
	}
	argv := strings.Join(x.argv, " ")
	if !strings.Contains(argv, tools.Path+`/bin/codex -c openai_base_url="https://gw.example/v1" app-server`) || !slices.Contains(x.env, "CODEX_API_KEY=sk-proj") || strings.Contains(argv, "sk-proj") {
		t.Errorf("argv %q, env %q", argv, x.env)
	}
	x.out = `{"id":1,"error":{"code":-32600,"message":"not signed in"}}`
	if _, err := c.Models(context.Background(), "img:1"); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("err = %v", err)
	}
}

const geminiList = `Loaded cached credentials.
[
  {
    "modelId": "auto-gemini-3",
    "id": "auto-gemini-3",
    "name": "Auto (Gemini 3)"
  },
  {
    "modelId": "gemini-3-pro-preview",
    "name": "gemini-3-pro-preview"
  },
  {
    "modelId": "gemini-2.5-pro",
    "name": "gemini-2.5-pro"
  }
]
`

// Gemini CLI's own list for a Google login, its default first.
func TestGeminiWithALoginListsTheCLIsModels(t *testing.T) {
	seen := vendor(t, 200, `{"models":[]}`, &googleModels)
	x := &listExec{out: geminiList}
	c := &CLI{Name: "gemini", Project: "WEB", Exec: x.exec}
	got, err := c.Models(context.Background(), "img:1")
	if err != nil || *seen != nil {
		t.Fatalf("models %q, %v; Google asked: %v", got, err, *seen != nil)
	}
	if want := []string{"auto-gemini-3", "gemini-3-pro-preview", "gemini-2.5-pro"}; !slices.Equal(got, want) {
		t.Errorf("models = %q, want %q", got, want)
	}
	if argv := strings.Join(x.argv, " "); !strings.HasSuffix(argv, "/gemini/bundle/gemini.js models list --output-format json") {
		t.Errorf("argv %q", argv)
	}
	if !slices.ContainsFunc(x.env, func(kv string) bool { return strings.Contains(kv, `"oauth-personal"`) }) {
		t.Errorf("env %q selects no Google login", x.env)
	}
}

// With a Gemini API key, Google's generateContent models follow the CLI's
// list (app decision all-gemini-models); a Vertex key has the CLI's only.
func TestGeminiWithAnAPIKeyAddsGooglesModels(t *testing.T) {
	seen := vendor(t, 200, `{"models":[{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent","countTokens"]},{"name":"models/gemini-4-hydrogen","supportedGenerationMethods":["generateContent"]},{"name":"models/text-embedding-004","supportedGenerationMethods":["embedContent"]}]}`, &googleModels)
	c := &CLI{Name: "gemini", Project: "WEB", Exec: (&listExec{out: geminiList}).exec, Credential: "AIza-x"}
	got, err := c.Models(context.Background(), "img:1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"auto-gemini-3", "gemini-3-pro-preview", "gemini-2.5-pro", "gemini-4-hydrogen"}; !slices.Equal(got, want) {
		t.Errorf("models = %q, want %q", got, want)
	}
	if (*seen).Get("x-goog-api-key") != "AIza-x" {
		t.Errorf("asked with headers %v", *seen)
	}
	*seen = nil
	c.Vertex = true
	if got, err := c.Models(context.Background(), "img:1"); err != nil || len(got) != 3 || *seen != nil {
		t.Errorf("vertex: %q, %v; Google asked: %v", got, err, *seen != nil)
	}
}

// OpenCode has no list of its own; a CLI that fails or prints no answer has
// no list, an error.
func TestModelsWithoutAnAnswer(t *testing.T) {
	if got, err := (&CLI{Name: "opencode"}).Models(context.Background(), "img:1"); got != nil || err != nil {
		t.Errorf("opencode: %q, %v", got, err)
	}
	for name, x := range map[string]*listExec{
		"failed":    {out: claudeInit, err: errors.New("exit status 3")},
		"no answer": {out: "Invalid API key"},
		"refused":   {out: `{"type":"control_response","response":{"subtype":"error","request_id":"models","error":"not logged in"}}`},
	} {
		if got, err := (&CLI{Name: "claude", Project: "WEB", Exec: x.exec}).Models(context.Background(), "img:1"); err == nil {
			t.Errorf("%s: models %q", name, got)
		}
	}
	for _, out := range []string{"", "[not json", `{"models":[]}`} {
		if got, err := (&CLI{Name: "gemini", Project: "WEB", Exec: (&listExec{out: out}).exec}).Models(context.Background(), "img:1"); err == nil {
			t.Errorf("gemini %q: models %q", out, got)
		}
	}
	if got, err := (&CLI{Name: "codex", Project: "WEB", Exec: (&listExec{out: `{"id":1,"result":{"data":[]}}`}).exec}).Models(context.Background(), "img:1"); err != nil || got == nil || len(got) != 0 {
		t.Errorf("codex with no models: %q, %v, want an empty list", got, err)
	}
}
