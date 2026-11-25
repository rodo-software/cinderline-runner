package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/tools"
)

// opencode is OpenCode. It has no turn limit to hit (see opencodeConfig).
// A turn reaches OpenRouter, or the runner's provider its model names.
var opencode = kind{
	libs: true,
	env: func(c *CLI, t Turn) []string {
		p, _ := c.provider(t)
		return opencodeEnv(c.Credential, t, c.MCPURL, p)
	},
	cmd: func(_ *CLI, t Turn, _ string) []string {
		return append([]string{tools.Path + "/bin/opencode"}, opencodeArgs(t)...)
	},
	parse: parseOpenCode,
	hosts: func(c *CLI, t Turn) []string {
		if p, _ := c.provider(t); p != nil {
			_, hp := environment.ProviderURL(p.BaseURL)
			return []string{hp}
		}
		return []string{"openrouter.ai:443"}
	},
	check: func(c *CLI, t Turn) error {
		_, err := c.provider(t)
		return err
	},
}

// providerKeyVar holds a provider's key in the agent container; the config
// names it, as OPENROUTER_API_KEY for OpenRouter.
var providerKeyVar = strings.ToUpper(name.Name) + "_PROVIDER_KEY"

// namedProvider is a turn's own provider: its name and model id, from the
// turn's "<provider>/<model id>".
type namedProvider struct {
	Provider
	name, model string
}

// provider is the runner's provider the turn's model names; nil for
// OpenRouter, which a model without a provider uses too. The error says
// why the turn cannot start: the runner lacks the provider or the project OpenRouter's key.
func (c *CLI) provider(t Turn) (*namedProvider, error) {
	n, model, ok := strings.Cut(t.Model, "/")
	if !ok || n == "openrouter" {
		if c.Credential == "" {
			return nil, errors.New("the project has no OpenRouter key for OpenCode")
		}
		return nil, nil
	}
	p, ok := c.Providers[n]
	if !ok {
		return nil, fmt.Errorf("model provider %q is not configured on this runner (add-provider)", n)
	}
	return &namedProvider{p, n, model}, nil
}

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
	if t.Effort != "" {
		a = append(a, "--variant", t.Effort)
	}
	if t.SessionID != "" {
		a = append(a, "--session", t.SessionID)
	}
	return a
}

// opencodeConfig is OpenCode's inline config (OPENCODE_CONFIG_CONTENT): the
// turn's provider, OpenRouter or p, its key and the scoped token read from
// the environment, the
// cinderline MCP server only for agents with a token, and the team's
// max_turns as the steps of the default build agent. After that many steps
// OpenCode asks the model for a text-only answer, so a turn ends with a
// report instead of an error.
//
// p is declared with OpenCode's bundled @ai-sdk/openai-compatible package,
// so nothing is installed at run time, and with the turn's model, as
// OpenCode drops a config provider without models (provider.ts, 1.19.5).
// This is the deprecated "provider" key, which 1.19.5 reads.
func opencodeConfig(t Turn, mcpURL string, p *namedProvider) string {
	mcp := map[string]any{}
	if t.Token != "" {
		mcp[name.Name] = map[string]any{
			"type":    "remote",
			"url":     mcpURL,
			"headers": map[string]string{"Authorization": "Bearer {env:" + tokenVar + "}"},
			"oauth":   false,
		}
	}
	provider := map[string]any{"openrouter": map[string]any{"options": map[string]string{"apiKey": "{env:OPENROUTER_API_KEY}"}}}
	if p != nil {
		base, _ := environment.ProviderURL(p.BaseURL)
		options := map[string]string{"baseURL": base}
		if p.Key != "" {
			options["apiKey"] = "{env:" + providerKeyVar + "}"
		}
		provider = map[string]any{p.name: map[string]any{
			"npm":     "@ai-sdk/openai-compatible",
			"name":    p.name,
			"options": options,
			"models":  map[string]any{p.model: map[string]any{}},
		}}
	}
	cfg, _ := json.Marshal(map[string]any{
		"$schema":    "https://opencode.ai/config.json",
		"autoupdate": false,
		"provider":   provider,
		"agent":      map[string]any{"build": map[string]int{"steps": t.MaxTurns}},
		"mcp":        mcp,
	})
	return string(cfg)
}

// opencodeEnv is the agent container's environment for an OpenCode turn:
// its data dir (the sessions) and config dir on the state volume, no fetch
// of the model catalog from models.opencode.ai (the egress proxy refuses it
// on every turn; OpenCode then uses the catalog built into its pinned
// version, and a custom provider declares its model in the config), the
// project's OpenRouter key or, for a turn on its own provider p, only that
// provider's key if it has one, the config and, for agents with Cinderline
// access, the scoped token. Agents without it skip the project's AGENTS.md
// and opencode.json (app repository decision
// 2026-11-05-workers-skip-project-files); the config above still applies.
func opencodeEnv(key string, t Turn, mcpURL string, p *namedProvider) []string {
	env := []string{"XDG_DATA_HOME=" + StatePath + "/opencode/data", "XDG_CONFIG_HOME=" + StatePath + "/opencode/config", "OPENCODE_DISABLE_MODELS_FETCH=1"}
	switch {
	case p == nil:
		env = append(env, "OPENROUTER_API_KEY="+key)
	case p.Key != "":
		env = append(env, providerKeyVar+"="+p.Key)
	}
	env = append(env, "OPENCODE_CONFIG_CONTENT="+opencodeConfig(t, mcpURL, p))
	return withToken(env, t, "OPENCODE_DISABLE_PROJECT_CONFIG=1")
}

// parseOpenCode reads OpenCode's JSON events: the session, the text of the
// last message with text, the usage summed over the steps (nil without
// steps) and the tool calls, its tool_use events. stdout and stderr are
// mixed, so only lines that parse as JSON count.
func parseOpenCode(out string) Result {
	var res Result
	var report []string
	last := ""
	for _, line := range jsonLines(out) {
		var e struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionID"`
			Part      *struct {
				MessageID string   `json:"messageID"`
				Text      string   `json:"text"`
				Tool      string   `json:"tool"`
				Cost      *float64 `json:"cost"`
				Tokens    struct {
					Input, Output, Reasoning int
					Cache                    struct{ Read, Write int }
				} `json:"tokens"`
			} `json:"part"`
		}
		_ = json.Unmarshal([]byte(line), &e) // a field of another type stays empty
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
		case e.Type == "tool_use":
			res.countTool(p.Tool)
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
