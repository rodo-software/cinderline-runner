package client

import (
	"encoding/json"
	"strings"

	"github.com/rodo-software/cinderstack-runner/internal/name"
)

// opencodeArgs is OpenCode's headless argv for one turn; the prompt goes in
// on stdin. OpenCode names the session itself, so only a continued turn
// passes --session. --auto approves every permission not denied, as Claude
// Code's --dangerously-skip-permissions, and a --title saves a call to a
// title model (checked with 1.19.5).
func opencodeArgs(t Turn) []string {
	// The title is only shown in OpenCode's own session list, which nobody reads.
	a := []string{"run", "--format", "json", "--auto", "--title", name.Name}
	if t.Model != "" {
		a = append(a, "--model", t.Model)
	}
	if t.SessionID != "" {
		a = append(a, "--session", t.SessionID)
	}
	return a
}

// opencodeConfig is OpenCode's inline config (OPENCODE_CONFIG_CONTENT): the
// OpenRouter key and the scoped token read from the environment, the
// cinderstack MCP server only for agents with a token, and the team's
// max_turns as the steps of the default build agent. After that many steps
// OpenCode asks the model for a text-only answer, so a turn ends with a
// report instead of an error.
func opencodeConfig(t Turn, mcpURL string) string {
	mcp := map[string]any{}
	if t.Token != "" {
		mcp[name.Name] = map[string]any{
			"type":    "remote",
			"url":     mcpURL,
			"headers": map[string]string{"Authorization": "Bearer {env:" + tokenVar + "}"},
			"oauth":   false,
		}
	}
	cfg, _ := json.Marshal(map[string]any{
		"$schema":    "https://opencode.ai/config.json",
		"autoupdate": false,
		"provider":   map[string]any{"openrouter": map[string]any{"options": map[string]string{"apiKey": "{env:OPENROUTER_API_KEY}"}}},
		"agent":      map[string]any{"build": map[string]int{"steps": t.MaxTurns}},
		"mcp":        mcp,
	})
	return string(cfg)
}

// opencodeEnv is the agent container's environment for an OpenCode turn:
// its data dir (the sessions) and config dir on the state volume, the
// project's OpenRouter key, the config and, for agents with CinderStack
// access, the scoped token. Agents without it skip the project's AGENTS.md
// and opencode.json (app repository decision
// 2026-11-05-workers-skip-project-files); the config above still applies.
func opencodeEnv(key string, t Turn, mcpURL string) []string {
	env := []string{
		"XDG_DATA_HOME=" + StatePath + "/opencode/data",
		"XDG_CONFIG_HOME=" + StatePath + "/opencode/config",
		"OPENROUTER_API_KEY=" + key,
		"OPENCODE_CONFIG_CONTENT=" + opencodeConfig(t, mcpURL),
	}
	if t.Token != "" {
		return append(env, tokenVar+"="+t.Token)
	}
	return append(env, "OPENCODE_DISABLE_PROJECT_CONFIG=1")
}

// parseOpenCode reads OpenCode's JSON events: the session, the text of the
// last message with text, and the usage summed over the steps (nil without
// steps). stdout and stderr are mixed, so only lines that parse as JSON count.
func parseOpenCode(out string) Result {
	var res Result
	var report []string
	last := ""
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "{") || !json.Valid([]byte(line)) {
			continue
		}
		var e struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionID"`
			Part      *struct {
				MessageID string   `json:"messageID"`
				Text      string   `json:"text"`
				Cost      *float64 `json:"cost"`
				Tokens    struct {
					Input, Output, Reasoning int
					Cache                    struct{ Read, Write int }
				} `json:"tokens"`
			} `json:"part"`
		}
		_ = json.Unmarshal([]byte(line), &e) // a field of another type stays empty, as in legacy
		if res.SessionID == "" {
			res.SessionID = e.SessionID
		}
		p := e.Part
		switch {
		case p == nil:
		case e.Type == "text" && p.Text != "":
			if p.MessageID != last {
				last, report = p.MessageID, nil
			}
			report = append(report, strings.TrimSpace(p.Text))
		case e.Type == "step_finish":
			if res.Usage == nil {
				res.Usage = &Usage{}
			}
			u, tk := res.Usage, p.Tokens
			u.InputTokens += tk.Input
			u.OutputTokens += tk.Output + tk.Reasoning // reasoning is billed as output
			u.CacheReadTokens += tk.Cache.Read
			u.CacheWriteTokens += tk.Cache.Write
			if c := p.Cost; c != nil {
				if u.CostUSD != nil {
					*c += *u.CostUSD
				}
				u.CostUSD = c
			}
		}
	}
	res.Report = strings.Join(report, "\n\n")
	return res
}
