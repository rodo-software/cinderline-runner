package client

import (
	"encoding/json"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/tools"
)

// gemini is the Gemini CLI (checked with 0.67.0), run by the tools volume's
// node. The shell writes the settings file first.
var gemini = kind{
	libs: true,
	env:  func(c *CLI, t Turn) []string { return geminiEnv(c.Credential, c.Vertex, t, c.MCPURL) },
	cmd: func(_ *CLI, t Turn, _ string) []string {
		return append([]string{"sh", "-c", geminiShell(`"$` + geminiSettingsVar + `"`), tools.Path + "/bin/node", tools.Path + "/gemini/bundle/gemini.js"}, geminiArgs(t)...)
	},
	parse: parseGemini,
	// The Gemini API, or Vertex AI's global endpoint for an express-mode key.
	hosts: func(c *CLI, _ Turn) []string {
		switch {
		case c.Credential == "":
			return []string{"generativelanguage.googleapis.com:443", "cloudcode-pa.googleapis.com:443", "oauth2.googleapis.com:443"}
		case c.Vertex:
			return []string{"aiplatform.googleapis.com:443"}
		}
		return []string{"generativelanguage.googleapis.com:443"}
	},
}

// geminiHome puts Gemini CLI's home on the state volume.
const geminiHome = "GEMINI_CLI_HOME=" + StatePath + "/gemini"

// geminiShell writes settings, a shell word, as the user settings file and
// runs the CLI.
func geminiShell(settings string) string {
	return `mkdir -p "$GEMINI_CLI_HOME/.gemini" && printf '%s' ` + settings + ` > "$GEMINI_CLI_HOME/.gemini/settings.json" && exec "$0" "$@"`
}

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

// geminiSettingsWithAuth builds the user settings file content with an
// explicit auth type. The settings go to the user settings file. A system
// settings file is skipped unless its folder is owned by root, and the
// repository's own .gemini/settings.json is ignored. Headless mode refuses
// an untrusted folder (exit 55); hence --skip-trust. The turn limit exits 53
// with a result of type FatalTurnLimitedError.
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

// geminiSettingsVar is the environment variable that holds the settings JSON.
var geminiSettingsVar = strings.ToUpper(name.Name) + "_GEMINI_SETTINGS"

// geminiEnv is the agent container's environment for a Gemini turn: the Gemini
// CLI home on the state volume, the project's key (either as GEMINI_API_KEY or,
// for Vertex, as GOOGLE_API_KEY with Vertex switched on), the settings and,
// for agents with Cinderline access, the scoped token.
// When key is empty no key variables are set and the auth type is oauth-personal:
// the stored Google login on the state volume is used instead.
func geminiEnv(key string, vertex bool, t Turn, mcpURL string) []string {
	env := []string{geminiHome}
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
	return withToken(env, t)
}

// parseGemini reads Gemini CLI's JSON events from mixed stdout/stderr.
// Only lines that start with "{" and are valid JSON are parsed; the rest
// are ignored. The tool calls are the tool_use events.
func parseGemini(out string) Result {
	var res Result
	var report []string
	for _, line := range jsonLines(out) {
		var e struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			ToolName  string `json:"tool_name"`
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
			res.countTool(e.ToolName)
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
