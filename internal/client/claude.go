package client

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/tools"
)

// claude is Claude Code. The runner picks the session ID.
var claude = kind{
	ownSession: true,
	env:        func(c *CLI, t Turn) []string { return claudeEnv(c.Credential, t) },
	cmd: func(c *CLI, t Turn, session string) []string {
		return append([]string{tools.Path + "/bin/claude"}, claudeArgs(t, session, c.MCPURL)...)
	},
	parse: parseClaude,
	// Anthropic's API.
	hosts: func(c *CLI, _ Turn) []string {
		if c.Credential == "" {
			return []string{"api.anthropic.com:443", "console.anthropic.com:443", "platform.claude.com:443"}
		}
		return []string{"api.anthropic.com:443"}
	},
}

// claudeHome puts Claude Code's config dir on the state volume.
const claudeHome = "CLAUDE_CONFIG_DIR=" + StatePath + "/claude"

// newSessionID is a random UUID v4: Claude Code's --session-id needs one.
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// claudeArgs is Claude Code's headless argv for one turn in session; the
// prompt goes in on stdin. The runner picks the session ID, so a fresh turn
// is --session-id and a continued one --resume. Without a token,
// --strict-mcp-config leaves the agent with no MCP server at all, which is
// how workers and the Merge Resolver run (0122).
func claudeArgs(t Turn, session, mcpURL string) []string {
	a := []string{"-p"}
	if t.SessionID != "" {
		a = append(a, "--resume", session)
	} else {
		a = append(a, "--session-id", session)
	}
	if t.Model != "" {
		a = append(a, "--model", t.Model)
	}
	if t.Effort != "" {
		a = append(a, "--effort", t.Effort)
	}
	a = append(a, "--max-turns", strconv.Itoa(t.MaxTurns), "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions")
	if t.Token != "" {
		cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{name.Name: map[string]any{
			"type":    "http",
			"url":     mcpURL,
			"headers": map[string]string{"Authorization": "Bearer ${" + tokenVar + "}"},
		}}})
		a = append(a, "--mcp-config", string(cfg))
	}
	a = append(a, "--strict-mcp-config")
	if t.Ponytail {
		a = append(a, "--plugin-dir", tools.Path+"/ponytail")
	}
	return a
}

// claudeEnv is the agent container's environment for a Claude Code turn:
// the config dir on the state volume, the project's Anthropic credential
// and, for agents with Cinderline access, the scoped token. Agents without
// it skip the project's CLAUDE.md files: workers get what they need in the
// sub-issue (app repository decision 2026-11-05-workers-skip-project-files).
func claudeEnv(credential string, t Turn) []string {
	// No telemetry or update checks: the proxy would refuse and count them.
	// In 2.1.316 that is feature flags, bootstrap and event logging, all on
	// api.anthropic.com; with it a turn reaches only that host (CLR-252,
	// decision 2026-11-25-client-own-traffic).
	env := []string{claudeHome, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}
	if credential != "" {
		// A `claude setup-token` token is an OAuth token; anything else is an API key.
		if strings.HasPrefix(credential, "sk-ant-oat") {
			env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+credential)
		} else {
			env = append(env, "ANTHROPIC_API_KEY="+credential)
		}
	}
	return withToken(env, t, "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1")
}

// parseClaude reads Claude Code's JSON result: the last line of the output
// that parses as JSON, as stdout and stderr are mixed. With stream-json that
// is the result event, or the last message of a stopped turn, which carries
// only the session (decision 2026-11-14-run-failsafes). The latest rate-limit
// event supplies the usage-limit status and reset; session IDs survive events
// that omit them. The tool calls are the tool_use blocks of the assistant
// messages, a subagent's included.
func parseClaude(out string) Result {
	var r struct {
		Subtype   string   `json:"subtype"`
		APIError  string   `json:"api_error"`
		Result    string   `json:"result"`
		SessionID string   `json:"session_id"`
		Cost      *float64 `json:"total_cost_usd"`
		Usage     *struct {
			Input      int `json:"input_tokens"`
			Output     int `json:"output_tokens"`
			CacheRead  int `json:"cache_read_input_tokens"`
			CacheWrite int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	lines := jsonLines(out)
	if len(lines) > 0 {
		_ = json.Unmarshal([]byte(lines[len(lines)-1]), &r) // a field of another type stays empty
	}
	res := Result{SessionID: r.SessionID, Report: strings.TrimSpace(r.Result), HitTurnLimit: r.Subtype == "error_max_turns", HitUsageLimit: r.APIError == "usage_limit_reached"}
	var status string
	var resetsAt int64
	for _, line := range lines {
		var e struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			Message   struct {
				Content []struct{ Type, Name string } `json:"content"` // a user message's text fails and stays empty
			} `json:"message"`
			RateLimitInfo struct {
				Status   string `json:"status"`
				ResetsAt int64  `json:"resetsAt"`
			} `json:"rate_limit_info"`
		}
		_ = json.Unmarshal([]byte(line), &e)
		if e.SessionID != "" {
			res.SessionID = e.SessionID
		}
		switch e.Type {
		case "rate_limit_event":
			status, resetsAt = e.RateLimitInfo.Status, e.RateLimitInfo.ResetsAt
		case "assistant":
			for _, b := range e.Message.Content {
				if b.Type == "tool_use" {
					res.countTool(b.Name)
				}
			}
		}
	}
	res.HitUsageLimit = res.HitUsageLimit || status == "rejected"
	// Only timestamps representable as RFC3339 can cross the API boundary.
	if res.HitUsageLimit && resetsAt > 0 && resetsAt <= 253402300799 {
		res.ResetsAt = time.Unix(resetsAt, 0).UTC()
	}
	if u := r.Usage; u != nil {
		res.Usage = &Usage{InputTokens: u.Input, OutputTokens: u.Output, CacheReadTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite, CostUSD: r.Cost}
	}
	return res
}
