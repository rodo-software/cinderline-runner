package client

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/tools"
)

// Ported from lib.test.mjs "claudeArgs gives a worker no MCP server at all".
func TestClaudeArgsWorkerHasNoMCPServer(t *testing.T) {
	orch := claudeArgs(Turn{Model: "m", MaxTurns: 5, Token: "tok"}, "s", "https://cs.example/mcp")
	want := []string{
		"-p", "--session-id", "s", "--model", "m", "--max-turns", "5", "--output-format", "json",
		"--dangerously-skip-permissions",
		"--mcp-config", `{"mcpServers":{"cinderline":{"headers":{"Authorization":"Bearer ${CINDERLINE_TOKEN}"},"type":"http","url":"https://cs.example/mcp"}}}`,
		"--strict-mcp-config",
	}
	if !reflect.DeepEqual(orch, want) {
		t.Errorf("orchestrator args =\n%q\nwant\n%q", orch, want)
	}
	if slices.Contains(orch, "tok") {
		t.Error("the token is in the argv")
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(orch[11]), &cfg); err != nil {
		t.Errorf("MCP config is not JSON: %v", err)
	}

	worker := claudeArgs(Turn{Model: "m", MaxTurns: 5, SessionID: "s"}, "s", "u")
	if !slices.Contains(worker, "--strict-mcp-config") || slices.Contains(worker, "--mcp-config") {
		t.Errorf("worker args %q: want --strict-mcp-config and no --mcp-config", worker)
	}
	if !reflect.DeepEqual(worker[:3], []string{"-p", "--resume", "s"}) {
		t.Errorf("worker args start %q, want a resume", worker[:3])
	}
	if slices.Contains(worker, "--plugin-dir") {
		t.Errorf("worker args %q load a plugin", worker)
	}
}

// Ported from lib.test.mjs "claudeArgs loads a plugin only when pluginDir is set".
func TestClaudeArgsPluginOnlyForPonytailTeams(t *testing.T) {
	if a := claudeArgs(Turn{Model: "m", MaxTurns: 5}, "s", "u"); slices.Contains(a, "--plugin-dir") {
		t.Errorf("args %q load a plugin", a)
	}
	a := claudeArgs(Turn{Model: "m", MaxTurns: 5, Ponytail: true}, "s", "u")
	if got, want := a[len(a)-2:], []string{"--plugin-dir", "/opt/cinderline/ponytail"}; !reflect.DeepEqual(got, want) {
		t.Errorf("args end %q, want %q", got, want)
	}
}

func TestClaudeArgsDefaultModel(t *testing.T) {
	if a := claudeArgs(Turn{MaxTurns: 5}, "s", "u"); slices.Contains(a, "--model") {
		t.Errorf("args %q pass --model without a model", a)
	}
}

// Ported from lib.test.mjs "agentEnv without project files skips CLAUDE.md
// and AGENTS.md" (Claude Code half) and "agentEnv gives the OpenRouter key
// only to OpenCode, and no Anthropic login" (Claude Code half: only the
// Anthropic credential, no other secret).
func TestClaudeEnv(t *testing.T) {
	orch := claudeEnv("sk-ant-api03-k", Turn{Token: "tok"})
	want := []string{
		"CLAUDE_CONFIG_DIR=/state/claude",
		// CSR-63: no telemetry or update checks, which the proxy would refuse and count.
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"ANTHROPIC_API_KEY=sk-ant-api03-k",
		"CINDERLINE_TOKEN=tok",
	}
	if !reflect.DeepEqual(orch, want) {
		t.Errorf("orchestrator env = %q, want %q", orch, want)
	}

	worker := claudeEnv("sk-ant-oat01-t", Turn{})
	want = []string{
		"CLAUDE_CONFIG_DIR=/state/claude",
		// CSR-63: no telemetry or update checks, which the proxy would refuse and count.
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-t",
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1",
	}
	if !reflect.DeepEqual(worker, want) {
		t.Errorf("worker env = %q, want %q", worker, want)
	}
}

// Ported from lib.test.mjs "parseRun reads the turn limit from the last JSON line".
func TestParseClaudeTurnLimit(t *testing.T) {
	result := func(subtype string) string {
		return "noise\n" + `{"type":"result","is_error":true,"subtype":"` + subtype + `"}`
	}
	for out, want := range map[string]bool{
		result("error_max_turns"):        true,
		result("error_during_execution"): false,
		"not json at all":                false,
		"":                               false,
	} {
		if got := parseClaude(out).HitTurnLimit; got != want {
			t.Errorf("parseClaude(%q).HitTurnLimit = %v, want %v", out, got, want)
		}
	}
}

// Ported from lib.test.mjs "parseRun reads the result of Claude Code JSON output".
func TestParseClaudeResult(t *testing.T) {
	out := "noise\n" + `{"type":"result","subtype":"success","result":" Report. ","session_id":"u-1"}`
	if got, want := parseClaude(out), (Result{SessionID: "u-1", Report: "Report."}); got != want {
		t.Errorf("parseClaude = %+v, want %+v", got, want)
	}
	if !parseClaude(`{"subtype":"error_max_turns"}`).HitTurnLimit {
		t.Error("a lone error_max_turns line is not the turn limit")
	}
}

// Ported from lib.test.mjs "parseRun reads the usage of a Claude Code run from its result".
func TestParseClaudeUsage(t *testing.T) {
	got := parseClaude(`{"type":"result","subtype":"success","total_cost_usd":0.25,"usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":30,"cache_read_input_tokens":40}}`).Usage
	cost := 0.25
	if want := (&Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 40, CacheWriteTokens: 30, CostUSD: &cost}); !reflect.DeepEqual(got, want) {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
	got = parseClaude(`{"type":"result","subtype":"success","usage":{"output_tokens":5}}`).Usage
	if want := (&Usage{OutputTokens: 5}); !reflect.DeepEqual(got, want) {
		t.Errorf("usage without cost = %+v, want %+v", got, want)
	}
	if got := parseClaude(`{"type":"result","subtype":"success"}`).Usage; got != nil {
		t.Errorf("usage of a result without it = %+v, want nil", got)
	}
}

// A real 2.1.316 result line (an API error, recorded on 2026-11-06), trimmed
// of fields the runner ignores: is_error does not make it a turn limit.
func TestParseClaudeRecordedError(t *testing.T) {
	out := `{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"duration_ms":483,"num_turns":1,"result":"Invalid API key · Fix external API key","session_id":"5f0c4ad6-1b0e-4c1e-9a43-0d1c2e1b7a10","total_cost_usd":0,"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}`
	got := parseClaude(out)
	if got.SessionID != "5f0c4ad6-1b0e-4c1e-9a43-0d1c2e1b7a10" || got.Report != "Invalid API key · Fix external API key" || got.HitTurnLimit || got.Usage == nil {
		t.Errorf("parseClaude = %+v", got)
	}
}

// Run: a fresh turn gets a session ID of its own, runs the tools volume's
// claude in the issue's clone with the prompt on stdin, only its own
// credential and token, the state volume prepared for the agent user, and the
// team's time limit; the result comes from the whole output.
func TestClaudeRunFresh(t *testing.T) {
	out := "noise\n" + `{"type":"result","subtype":"success","result":"Done.","session_id":"ignored","usage":{"output_tokens":5}}`
	env := &environment.Fake{Runs: []environment.RunReply{{Output: out}}}
	var docker [][]string
	c := &Claude{
		Project: "CSR", Env: env, Credential: "sk-ant-api03-k", MCPURL: "https://cs.example/mcp",
		Tools: tools.Mount{Args: []string{"-v", "tools:/opt/cinderline:ro"}, Env: []string{"DISABLE_AUTOUPDATER=1"}},
		Exec: func(_ context.Context, _ []string, argv ...string) (string, error) {
			docker = append(docker, argv)
			return "", nil
		},
	}
	res, err := c.Run(context.Background(), Turn{Dir: "/runs/CSR-7", Prompt: "do it", MaxTurns: 5, MaxMinutes: 30, Model: "m", Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	calls := env.Calls()
	if len(calls) != 1 {
		t.Fatalf("Environment calls = %+v", calls)
	}
	s := calls[0].Args[0].(environment.Spec)
	session := s.Cmd[3]
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(session) {
		t.Errorf("session ID %q is not a UUID v4", session)
	}
	want := environment.Spec{
		Issue:   "CSR-7",
		Dir:     "/runs/CSR-7",
		Mounts:  []string{"-v", "cinderline-CSR-repo:/repo:ro", "-v", "cinderline-CSR-runs:/runs", "-v", "cinderline-CSR-state-claude:/state", "-v", "tools:/opt/cinderline:ro"},
		Env:     append([]string{"DISABLE_AUTOUPDATER=1"}, claudeEnv("sk-ant-api03-k", Turn{Token: "tok"})...),
		Cmd:     append([]string{"/opt/cinderline/bin/claude"}, claudeArgs(Turn{Model: "m", MaxTurns: 5, Token: "tok"}, session, "https://cs.example/mcp")...),
		Stdin:   "do it",
		Timeout: 30 * time.Minute,
		Allow:   []string{"api.anthropic.com:443"},
	}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("Spec =\n%+v\nwant\n%+v", s, want)
	}
	if res.SessionID != "ignored" || res.Report != "Done." || res.Tail != environment.Tail(out) || res.Usage == nil {
		t.Errorf("Result = %+v", res)
	}
	if len(docker) != 2 || !slices.Contains(docker[0], "volume") || !slices.Contains(docker[1], "chown 1000:1000 /s") {
		t.Errorf("state volume preparation = %q", docker)
	}
}

// Run: a resumed turn keeps its session; the turn limit and a failure of the
// container are errors with the parsed result, so Attempts continues or
// retries (CSR-21).
func TestClaudeRunErrors(t *testing.T) {
	limit := `{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"s1"}`
	boom := errors.New("exit status 1")
	for _, tt := range []struct {
		name  string
		reply environment.RunReply
		check func(Result, error) bool
	}{
		{"turn limit", environment.RunReply{Output: limit}, func(r Result, err error) bool {
			return err != nil && r.HitTurnLimit && r.SessionID == "s1"
		}},
		{"turn limit with an exit code", environment.RunReply{Output: limit, Err: boom}, func(r Result, err error) bool {
			return errors.Is(err, boom) && r.HitTurnLimit
		}},
		{"no output", environment.RunReply{Err: environment.ErrTimeout}, func(r Result, err error) bool {
			return errors.Is(err, environment.ErrTimeout) && r.SessionID == "s1" && !r.HitTurnLimit
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := &environment.Fake{Runs: []environment.RunReply{tt.reply}}
			c := &Claude{Project: "CSR", Env: env, Exec: func(context.Context, []string, ...string) (string, error) { return "", nil }}
			res, err := c.Run(context.Background(), Turn{Dir: "/runs/CSR-7", SessionID: "s1", MaxTurns: 5})
			if !tt.check(res, err) {
				t.Errorf("Run = %+v, %v", res, err)
			}
			if cmd := env.Calls()[0].Args[0].(environment.Spec).Cmd; !reflect.DeepEqual(cmd[1:4], []string{"-p", "--resume", "s1"}) {
				t.Errorf("Cmd = %q, want a resume of s1", cmd)
			}
		})
	}
}

func TestClaudeRunStateVolumeFails(t *testing.T) {
	env := &environment.Fake{}
	boom := errors.New("docker broke")
	c := &Claude{Project: "CSR", Env: env, Exec: func(context.Context, []string, ...string) (string, error) { return "", boom }}
	if _, err := c.Run(context.Background(), Turn{Dir: "/runs/CSR-7"}); !errors.Is(err, boom) {
		t.Errorf("Run err = %v, want %v", err, boom)
	}
	if len(env.Calls()) != 0 {
		t.Error("the turn ran without its state volume")
	}
}
