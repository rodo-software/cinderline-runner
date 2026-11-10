package client

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// Claude is the Claude Code Client: one container per turn through the
// Environment (decision 2026-11-06-container-per-turn), the binary from the
// tools volume.
type Claude struct {
	Project    string                  // project key, for its volumes
	Env        environment.Environment // where the turn runs
	Exec       workspace.Exec          // prepares the state volume; nil means workspace.RunCommand
	Credential string                  // the project's Anthropic API key or `claude setup-token` token
	MCPURL     string                  // Cinderline's MCP endpoint, as the agent container reaches it
	Tools      tools.Mount             // from tools.Tools.Mount for the Environment's image
}

var _ Client = (*Claude)(nil)

// Hosts returns the provider host:port pairs a run needs, for the run's
// network allowlist: Anthropic's API.
// Without a key the run uses the stored sign-in (CSR-58), which also needs
// the provider's sign-in hosts: decision 2026-11-09-network-allowlist point 5,
// unchecked against the CLI; a missing one shows up as a refused host.
func (c *Claude) Hosts() []string {
	if c.Credential == "" {
		return []string{"api.anthropic.com:443", "console.anthropic.com:443", "platform.claude.com:443"}
	}
	return []string{"api.anthropic.com:443"}
}

// errTurnLimit is a turn that stopped at the team's turn limit.
var errTurnLimit = errors.New("claude: stopped at the turn limit")

// Run runs one turn. The turn limit is an error with HitTurnLimit set, so
// Attempts continues the session (CSR-21).
func (c *Claude) Run(ctx context.Context, t Turn) (Result, error) {
	if err := PrepareState(ctx, c.Exec, c.Project); err != nil {
		return Result{}, err
	}
	session := t.SessionID
	if session == "" {
		session = newSessionID()
	}
	out, err := c.Env.Run(ctx, environment.Spec{
		Issue:   path.Base(t.Dir), // the clone is RunsPath/<REF>
		Dir:     t.Dir,
		Mounts:  agentMounts(c.Project, c.Tools),
		Env:     append(append([]string(nil), c.Tools.Env...), claudeEnv(c.Credential, t)...),
		Cmd:     append([]string{tools.Path + "/bin/claude"}, claudeArgs(t, session, c.MCPURL)...),
		Stdin:   t.Prompt,
		Timeout: time.Duration(t.MaxMinutes) * time.Minute,
		Allow:   c.Hosts(),
	})
	res := parseClaude(out)
	if res.SessionID == "" {
		res.SessionID = session
	}
	res.Tail = environment.Tail(out)
	if err == nil && res.HitTurnLimit {
		err = errTurnLimit
	}
	return res, err
}

// agentMounts mounts the issue's clone, the repo volume that holds the
// objects the clone's alternates point at, the state volume and the tools.
func agentMounts(project string, t tools.Mount) []string {
	return append([]string{
		"-v", workspace.Volume(project, "repo") + ":" + workspace.RepoPath + ":ro",
		"-v", workspace.Volume(project, "runs") + ":" + workspace.RunsPath,
		"-v", workspace.Volume(project, "state") + ":" + StatePath,
	}, t.Args...)
}

// PrepareState makes the project's state volume and hands its root to the
// agent user: a new volume belongs to root, and the CLIs write their config
// dirs and sessions in it. Every turn, as it costs a second and repairs
// itself. A nil x means workspace.RunCommand.
func PrepareState(ctx context.Context, x workspace.Exec, project string) error {
	if x == nil {
		x = workspace.RunCommand
	}
	vol := workspace.Volume(project, "state")
	if _, err := x(ctx, nil, append(append([]string{"docker", "volume", "create"}, workspace.Labels(project, "")...), vol)...); err != nil {
		return fmt.Errorf("state volume: %w", err)
	}
	if _, err := x(ctx, nil, "docker", "run", "--rm", "--network", "none", "-v", vol+":/s", "--entrypoint", "sh", workspace.GitImage, "-c", "chown "+workspace.AgentUser+" /s"); err != nil {
		return fmt.Errorf("state volume: %w", err)
	}
	return nil
}

// newSessionID is a random UUID v4: Claude Code's --session-id needs one.
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// StatePath is where the agent container mounts the project's state volume,
// which holds the clients' sessions and config dirs (decision
// 2026-11-05-runner-state-location, 0116).
const StatePath = "/state"

// tokenVar holds the run's scoped token in the agent container. The MCP
// config names it, so the token never appears in an argv (Claude Code
// expands ${VAR} in --mcp-config, checked with 2.1.316).
var tokenVar = strings.ToUpper(name.Name) + "_TOKEN"

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
	a = append(a, "--max-turns", strconv.Itoa(t.MaxTurns), "--output-format", "json", "--dangerously-skip-permissions")
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
	env := []string{"CLAUDE_CONFIG_DIR=" + StatePath + "/claude", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}
	if credential != "" {
		// A `claude setup-token` token is an OAuth token; anything else is an API key.
		if strings.HasPrefix(credential, "sk-ant-oat") {
			env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+credential)
		} else {
			env = append(env, "ANTHROPIC_API_KEY="+credential)
		}
	}
	if t.Token != "" {
		return append(env, tokenVar+"="+t.Token)
	}
	return append(env, "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1")
}

// parseClaude reads Claude Code's JSON result: the last line of the output
// that parses as JSON, as stdout and stderr are mixed.
func parseClaude(out string) Result {
	var r struct {
		Subtype   string   `json:"subtype"`
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
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "{") && json.Valid([]byte(lines[i])) {
			_ = json.Unmarshal([]byte(lines[i]), &r) // a field of another type stays empty, as in legacy
			break
		}
	}
	res := Result{SessionID: r.SessionID, Report: strings.TrimSpace(r.Result), HitTurnLimit: r.Subtype == "error_max_turns"}
	if u := r.Usage; u != nil {
		res.Usage = &Usage{InputTokens: u.Input, OutputTokens: u.Output, CacheReadTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite, CostUSD: r.Cost}
	}
	return res
}
