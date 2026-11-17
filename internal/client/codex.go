package client

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/tools"
)

// codex is the Codex CLI (checked with 0.167.0). It has no turn or step
// limit; the team's time limit bounds the turn.
var codex = kind{
	env: func(c *CLI, t Turn) []string { return codexEnv(c.Credential, t) },
	cmd: func(c *CLI, t Turn, _ string) []string {
		return append([]string{"sh", "-c", codexShell, tools.Path + "/bin/codex"}, codexArgs(t, c.MCPURL, c.BaseURL)...)
	},
	parse: parseCodex,
	// OpenAI's API, or the configured base URL's.
	hosts: func(c *CLI, _ Turn) []string {
		if c.BaseURL != "" {
			if hp := environment.HostPort(c.BaseURL); hp != "" {
				return []string{hp}
			}
			return nil
		}
		if c.Credential == "" {
			return []string{"api.openai.com:443", "chatgpt.com:443", "auth.openai.com:443"}
		}
		return []string{"api.openai.com:443"}
	},
}

// codexHome puts Codex's home dir on the state volume; codexShell makes it
// first, as Codex refuses a CODEX_HOME that does not exist.
const (
	codexHome  = "CODEX_HOME=" + StatePath + "/codex"
	codexShell = `mkdir -p "$CODEX_HOME" && exec "$0" "$@"`
)

// codexArgs is Codex's headless argv for one turn; the prompt goes in on
// stdin through "-". Options before the resume subcommand.
func codexArgs(t Turn, mcpURL, baseURL string) []string {
	a := []string{"exec", "--json", "--dangerously-bypass-approvals-and-sandbox"}
	if t.Model != "" {
		a = append(a, "--model", t.Model)
	}
	if t.Effort != "" {
		a = append(a, "-c", "model_reasoning_effort="+strconv.Quote(t.Effort))
	}
	if baseURL != "" {
		a = append(a, "-c", "openai_base_url="+strconv.Quote(baseURL))
	}
	if t.Token != "" {
		a = append(a,
			"-c", "mcp_servers."+name.Name+".url="+strconv.Quote(mcpURL),
			"-c", "mcp_servers."+name.Name+".bearer_token_env_var="+strconv.Quote(tokenVar),
		)
	} else {
		a = append(a, "-c", "project_doc_max_bytes=0")
	}
	if t.SessionID != "" {
		a = append(a, "resume", t.SessionID)
	}
	a = append(a, "-")
	return a
}

// codexEnv is the agent container's environment for a Codex turn: the codex
// home dir on the state volume, the OpenAI API key and, for agents with
// Cinderline access, the scoped token.
func codexEnv(key string, t Turn) []string {
	env := []string{codexHome}
	if key != "" {
		env = append(env, "CODEX_API_KEY="+key)
	}
	return withToken(env, t)
}

// parseCodex reads Codex's JSON events from mixed stdout/stderr. Events that
// start with "{" and are valid JSON are parsed; the rest are ignored.
func parseCodex(out string) Result {
	var res Result
	for _, line := range jsonLines(out) {
		var e struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Item     *struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Usage *struct {
				InputTokens           int `json:"input_tokens"`
				CachedInputTokens     int `json:"cached_input_tokens"`
				CacheWriteInputTokens int `json:"cache_write_input_tokens"`
				OutputTokens          int `json:"output_tokens"` // reasoning included
			} `json:"usage"`
		}
		_ = json.Unmarshal([]byte(line), &e)
		switch e.Type {
		case "thread.started":
			if res.SessionID == "" {
				res.SessionID = e.ThreadID
			}
		case "item.completed":
			if e.Item != nil && e.Item.Type == "agent_message" && e.Item.Text != "" {
				res.Report = strings.TrimSpace(e.Item.Text)
			}
		case "turn.completed": // the thread's total so far; the last one wins
			if u := e.Usage; u != nil {
				res.Usage = &Usage{
					InputTokens:      max(u.InputTokens-u.CachedInputTokens, 0),
					CacheReadTokens:  u.CachedInputTokens,
					CacheWriteTokens: u.CacheWriteInputTokens,
					OutputTokens:     u.OutputTokens,
				}
			}
		}
	}
	return res
}
