package client

import (
	"context"
	"encoding/json"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// Codex is the Codex CLI Client (checked with 0.167.0): one container per turn
// through the Environment, the binary from the tools volume.
type Codex struct {
	Project string                  // project key, for its volumes
	Env     environment.Environment // where the turn runs
	Exec    workspace.Exec          // prepares the state volume; nil means workspace.RunCommand
	Key     string                  // the project's OpenAI API key
	BaseURL string                  // an OpenAI-compatible base URL; empty: OpenAI's API
	MCPURL  string                  // Cinderline's MCP endpoint, as the agent container reaches it
	Tools   tools.Mount             // from tools.Tools.Mount for the Environment's image
}

var _ Client = (*Codex)(nil)

// Run runs one turn. Codex has no turn or step limit; the team's time limit
// bounds the turn. HitTurnLimit stays false.
func (c *Codex) Run(ctx context.Context, t Turn) (Result, error) {
	if err := PrepareState(ctx, c.Exec, c.Project, "codex"); err != nil {
		return Result{}, err
	}
	out, err := c.Env.Run(ctx, environment.Spec{
		Issue:  path.Base(t.Dir),
		Dir:    t.Dir,
		Mounts: agentMounts(c.Project, "codex", c.Tools),
		Env:    append(append([]string(nil), c.Tools.Env...), codexEnv(c.Key, t)...),
		Cmd: append([]string{
			"sh", "-c", `mkdir -p "$CODEX_HOME" && exec "$0" "$@"`,
			tools.Path + "/bin/codex",
		}, codexArgs(t, c.MCPURL, c.BaseURL)...),
		Stdin:   t.Prompt,
		Timeout: time.Duration(t.MaxMinutes) * time.Minute,
		Allow:   c.Hosts(),
	})
	res := parseCodex(out)
	if res.SessionID == "" {
		res.SessionID = t.SessionID
	}
	res.Tail = environment.Tail(out)
	return res, err
}

// Hosts returns the provider host:port pairs a run needs, for the run's
// network allowlist: OpenAI's API, or the configured base URL's.
// Without a key the run uses the stored sign-in (CSR-58), which also needs
// the provider's sign-in hosts: decision 2026-11-09-network-allowlist point 5,
// unchecked against the CLI; a missing one shows up as a refused host.
func (c *Codex) Hosts() []string {
	if c.BaseURL != "" {
		if hp := environment.HostPort(c.BaseURL); hp != "" {
			return []string{hp}
		}
		return nil
	}
	if c.Key == "" {
		return []string{"api.openai.com:443", "chatgpt.com:443", "auth.openai.com:443"}
	}
	return []string{"api.openai.com:443"}
}

// codexArgs is Codex's headless argv for one turn; the prompt goes in on
// stdin through "-". Options before the resume subcommand.
func codexArgs(t Turn, mcpURL, baseURL string) []string {
	a := []string{"exec", "--json", "--dangerously-bypass-approvals-and-sandbox"}
	if t.Model != "" {
		a = append(a, "--model", t.Model)
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
	env := []string{
		"CODEX_HOME=" + StatePath + "/codex",
	}
	if key != "" {
		env = append(env, "CODEX_API_KEY="+key)
	}
	if t.Token != "" {
		return append(env, tokenVar+"="+t.Token)
	}
	return env
}

// parseCodex reads Codex's JSON events from mixed stdout/stderr. Events that
// start with "{" and are valid JSON are parsed; the rest are ignored.
func parseCodex(out string) Result {
	var res Result
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "{") || !json.Valid([]byte(line)) {
			continue
		}
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
