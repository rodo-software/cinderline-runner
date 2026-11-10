package client

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// Gemini is the Gemini CLI Client (checked with 0.67.0): one container per turn
// through the Environment, node and the CLI's bundle from the tools volume.
type Gemini struct {
	Project string                  // project key, for its volumes
	Env     environment.Environment // where the turn runs
	Exec    workspace.Exec          // prepares the state volume; nil means workspace.RunCommand
	Key     string                  // the project's Gemini API key
	Vertex  bool                    // Key is a Vertex AI express-mode key; false: the Gemini API
	MCPURL  string                  // Cinderline's MCP endpoint, as the agent container reaches it
	Tools   tools.Mount             // from tools.Tools.Mount for the Environment's image
}

var _ Client = (*Gemini)(nil)

// errGeminiTurnLimit is a turn that stopped at the team's turn limit.
var errGeminiTurnLimit = errors.New("gemini: stopped at the turn limit")

// geminiArgs is Gemini CLI's headless argv for one turn; the prompt goes in
// on stdin through "-p". Options before a possible --resume.
func geminiArgs(t Turn) []string {
	a := []string{"-p", "", "--output-format", "stream-json", "--yolo", "--skip-trust"}
	if t.Model != "" {
		a = append(a, "--model", t.Model)
	}
	if t.SessionID != "" {
		a = append(a, "--resume", t.SessionID)
	}
	return a
}

// geminiSettingsWithAuth is geminiSettings with an explicit auth type.
func geminiSettingsWithAuth(t Turn, mcpURL string, authType string) string {
	sec := map[string]any{"auth": map[string]string{"selectedType": authType}}
	cfg := map[string]any{"security": sec}
	if t.MaxTurns > 0 {
		cfg["model"] = map[string]int{"maxSessionTurns": t.MaxTurns}
	}
	cfg["general"] = map[string]bool{"enableAutoUpdate": false, "enableAutoUpdateNotification": false}
	cfg["privacy"] = map[string]bool{"usageStatisticsEnabled": false}
	if t.Token != "" {
		// shortcut: workers read the repository's GEMINI.md, which Gemini CLI
		// 0.67.0 always loads; upgrade when it offers a switch.
		cfg["context"] = map[string][]string{"fileName": []string{"AGENTS.md", "GEMINI.md"}}
		cfg["mcpServers"] = map[string]any{name.Name: map[string]any{
			"httpUrl": mcpURL,
			"headers": map[string]string{"Authorization": "Bearer $" + tokenVar},
		}}
	}
	s, _ := json.Marshal(cfg)
	return string(s)
}

func geminiAuthType(vertex bool) string {
	if vertex {
		return "vertex-ai"
	}
	return "gemini-api-key"
}

// geminiSettings builds the user settings file content. The settings go to
// the user settings file. A system settings file is skipped unless its folder
// is owned by root, and the repository's own .gemini/settings.json is ignored.
// Headless mode refuses an untrusted folder (exit 55); hence --skip-trust.
// The turn limit exits 53 with a result of type FatalTurnLimitedError.
func geminiSettings(t Turn, mcpURL string, vertex bool) string {
	return geminiSettingsWithAuth(t, mcpURL, geminiAuthType(vertex))
}

// geminiSettingsVar is the environment variable that holds the settings JSON.
var geminiSettingsVar = strings.ToUpper(name.Name) + "_GEMINI_SETTINGS"

// geminiEnv is the agent container's environment for a Gemini turn: the Gemini
// CLI home on the state volume, the project's key (either as GEMINI_API_KEY or,
// for Vertex, as GOOGLE_API_KEY with Vertex switched on), the settings and,
// for agents with Cinderline access, the scoped token.
// When key is empty no key variables are set and the auth type is oauth-personal:
// the stored Google login on the state volume is used instead.
func geminiEnv(key string, vertex bool, t Turn, mcpURL string) []string {
	env := []string{
		"GEMINI_CLI_HOME=" + StatePath + "/gemini",
	}
	auth := geminiAuthType(vertex)
	switch {
	case key == "":
		auth = "oauth-personal"
	case vertex:
		env = append(env, "GOOGLE_GENAI_USE_VERTEXAI=true", "GOOGLE_API_KEY="+key)
	default:
		env = append(env, "GEMINI_API_KEY="+key)
	}
	env = append(env, geminiSettingsVar+"="+geminiSettingsWithAuth(t, mcpURL, auth))
	if t.Token != "" {
		return append(env, tokenVar+"="+t.Token)
	}
	return env
}

// parseGemini reads Gemini CLI's JSON events from mixed stdout/stderr.
// Only lines that start with "{" and are valid JSON are parsed; the rest
// are ignored.
func parseGemini(out string) Result {
	var res Result
	var report []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "{") || !json.Valid([]byte(line)) {
			continue
		}
		var e struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			Role      string `json:"role"`
			Content   string `json:"content"`
			Status    string `json:"status"`
			Error     *struct {
				Type string `json:"type"`
			} `json:"error"`
			Stats *struct {
				Input       int `json:"input"`
				InputCached int `json:"cached"`
				Total       int `json:"total_tokens"`
				InputTotal  int `json:"input_tokens"`
			} `json:"stats"`
		}
		_ = json.Unmarshal([]byte(line), &e)
		switch e.Type {
		case "init":
			if res.SessionID == "" {
				res.SessionID = e.SessionID
			}
		case "message":
			if e.Role == "assistant" && e.Content != "" {
				report = append(report, e.Content)
			}
		case "tool_use":
			report = nil
		case "result":
			if e.Status == "error" && e.Error != nil && e.Error.Type == "FatalTurnLimitedError" {
				res.HitTurnLimit = true
			}
			if u := e.Stats; u != nil {
				res.Usage = &Usage{
					InputTokens:     u.Input,
					CacheReadTokens: u.InputCached,
					OutputTokens:    max(u.Total-u.InputTotal, 0),
				}
			}
		}
	}
	res.Report = strings.TrimSpace(strings.Join(report, ""))
	return res
}

// Run runs one turn.
func (c *Gemini) Run(ctx context.Context, t Turn) (Result, error) {
	if err := PrepareState(ctx, c.Exec, c.Project); err != nil {
		return Result{}, err
	}
	out, err := c.Env.Run(ctx, environment.Spec{
		Issue:  path.Base(t.Dir),
		Dir:    t.Dir,
		Mounts: agentMounts(c.Project, c.Tools),
		Env:    append(append(append([]string(nil), c.Tools.Env...), c.Tools.OpenCodeEnv...), geminiEnv(c.Key, c.Vertex, t, c.MCPURL)...),
		Cmd: append([]string{
			"sh", "-c", `mkdir -p "$GEMINI_CLI_HOME/.gemini" && printf '%s' "$` + geminiSettingsVar + `" > "$GEMINI_CLI_HOME/.gemini/settings.json" && exec "$0" "$@"`,
			tools.Path + "/bin/node", tools.Path + "/gemini/bundle/gemini.js",
		}, geminiArgs(t)...),
		Stdin:   t.Prompt,
		Timeout: time.Duration(t.MaxMinutes) * time.Minute,
		Allow:   c.Hosts(),
	})
	res := parseGemini(out)
	if res.SessionID == "" {
		res.SessionID = t.SessionID
	}
	res.Tail = environment.Tail(out)
	if err == nil && res.HitTurnLimit {
		err = errGeminiTurnLimit
	}
	return res, err
}

// Hosts returns the provider host:port pairs a run needs, for the run's
// network allowlist: the Gemini API, or Vertex AI's global endpoint for an
// express-mode key.
// Without a key the run uses the stored sign-in (CSR-58), which also needs
// the provider's sign-in hosts: decision 2026-11-09-network-allowlist point 5,
// unchecked against the CLI; a missing one shows up as a refused host.
func (c *Gemini) Hosts() []string {
	switch {
	case c.Key == "":
		return []string{"generativelanguage.googleapis.com:443", "cloudcode-pa.googleapis.com:443", "oauth2.googleapis.com:443"}
	case c.Vertex:
		return []string{"aiplatform.googleapis.com:443"}
	}
	return []string{"generativelanguage.googleapis.com:443"}
}
