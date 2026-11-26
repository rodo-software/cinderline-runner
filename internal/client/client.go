// Package client defines Client, the agent CLI (Claude Code, Codex, OpenCode or
// Gemini CLI) that runs one agent turn, and Fake, its in-memory stand-in for
// tests. It is not the REST client of the issue tracker, which is its own package.
package client

import (
	"context"
	"path"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// Client runs agent turns.
type Client interface {
	// Run runs one turn. On failure it returns the parsed result it has, such
	// as the output tail, together with the error.
	Run(ctx context.Context, t Turn) (Result, error)
}

// Turn is one agent turn. An empty SessionID starts a new session; otherwise
// that session is resumed.
type Turn struct {
	Dir       string
	Prompt    string
	SessionID string
	MaxTurns  int
	// MaxMinutes is the team's time limit for the turn, at which the
	// Environment stops it; 0: none.
	MaxMinutes int
	// Idle is how long the turn may print nothing before the Environment
	// stops it as stalled (CSR-159); 0: never.
	Idle   time.Duration
	Model  string // the agent's model; empty: the client's default
	Effort string // the agent's effort level; empty: the client's default. Not for Gemini CLI, which has no such option
	// Author is the agent's git identity, "<team> / <agent>", the name of its
	// Cinderline token; the clients set it with workspace.Identity (CSR-169).
	Author string
	// Token is the run's scoped Cinderline token for the MCP server; empty
	// gives the agent none, as workers and the Merge Resolver (0122). Never log it.
	Token string
	// Skills are the skill packages the run loaded, for every turn of it
	// (app decision 2026-11-26-skill-packages).
	Skills []SkillMount
	// Allow is the host:port pairs a person allowed in Cinderline for the
	// turn's issue, which the caller has checked, for the run's allowlist
	// next to the provider's (decision 2026-11-17-hosts-from-cinderline).
	Allow []string
	// Attachments is the host folder of the run's attachments, mounted
	// read-only at workspace.AttachmentsPath; "": the run has none.
	Attachments string
}

// SkillMount is a skill package a run loaded: its name, its read-only volume
// with the repository in workspace.SkillRepo, the package's root in the
// repository (Path; "": the repository's root) and what that root holds.
// Every turn mounts the repository at workspace.SkillsPath/<Name>, and each
// client loads the package its own way (docs/skills/client-loading.md).
type SkillMount struct {
	Name, Volume, Path string
	tools.SkillContents
}

// root is where the turn's container sees the package's root.
func (s SkillMount) root() string { return path.Join(workspace.SkillsPath, s.Name, s.Path) }

// mount is the docker flags that mount folder sub of the package's volume
// read-only at dst.
func (s SkillMount) mount(sub, dst string) []string {
	return []string{"--mount", "type=volume,src=" + s.Volume + ",dst=" + dst + ",readonly,volume-subpath=" + sub}
}

// in is folder sub of the package's root ("": the root) as a folder of its volume.
func (s SkillMount) in(sub string) string { return path.Join(workspace.SkillRepo, s.Path, sub) }

// Result is the parsed outcome of a turn.
type Result struct {
	SessionID     string
	Report        string // the agent's final text
	HitUsageLimit bool
	ResetsAt      time.Time // zero when the client did not report a reset
	HitTurnLimit  bool
	Tail          string // the end of the output, for comments
	Usage         *Usage // nil when the CLI printed none
	// UsageTotal says Usage is the session's total so far, earlier turns
	// included, not the turn's: Codex's (CLR-232).
	UsageTotal bool
	// Tools is the turn's tool calls per tool name, as the CLI names the
	// tool; nil when it made none (decision 2026-11-24-tool-use-counts).
	Tools map[string]int
}

// countTool counts one call of the tool name; a call without a name is not counted.
func (r *Result) countTool(name string) {
	if name == "" {
		return
	}
	if r.Tools == nil {
		r.Tools = map[string]int{}
	}
	r.Tools[name]++
}

// Usage is the tokens and cost of one turn (EMBER-112), or a session's total
// so far (Result.UsageTotal). Its JSON is a session note's.
type Usage struct {
	InputTokens      int      `json:"input_tokens"`
	OutputTokens     int      `json:"output_tokens"`
	CacheReadTokens  int      `json:"cache_read_tokens"`
	CacheWriteTokens int      `json:"cache_write_tokens"`
	CostUSD          *float64 `json:"cost_usd,omitempty"` // nil: the CLI printed none, so Cinderline estimates it
}

// Since is the usage a session total u added to the earlier total prev,
// each count at least 0. Its cost is u's: only Codex reports totals, and it
// prints none.
func (u Usage) Since(prev Usage) Usage {
	return Usage{
		InputTokens:      max(u.InputTokens-prev.InputTokens, 0),
		OutputTokens:     max(u.OutputTokens-prev.OutputTokens, 0),
		CacheReadTokens:  max(u.CacheReadTokens-prev.CacheReadTokens, 0),
		CacheWriteTokens: max(u.CacheWriteTokens-prev.CacheWriteTokens, 0),
		CostUSD:          u.CostUSD,
	}
}
