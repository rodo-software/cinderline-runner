package client

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/tools"
)

// Ported from lib.test.mjs "opencodeArgs continues a session only when resuming".
func TestOpenCodeArgs(t *testing.T) {
	fresh := opencodeArgs(Turn{Model: "openrouter/q/c"})
	want := []string{"run", "--format", "json", "--auto", "--title", "cinderline", "--model", "openrouter/q/c"}
	if !reflect.DeepEqual(fresh, want) {
		t.Errorf("fresh args = %q, want %q", fresh, want)
	}
	resumed := opencodeArgs(Turn{Model: "x", SessionID: "ses_1"})
	if got := resumed[len(resumed)-2:]; !reflect.DeepEqual(got, []string{"--session", "ses_1"}) {
		t.Errorf("resumed args end %q, want --session ses_1", got)
	}
	if a := opencodeArgs(Turn{}); slices.Contains(a, "--model") {
		t.Errorf("args %q pass --model without a model", a)
	}
}

// Ported from lib.test.mjs "opencodeConfig has Ember only for the
// orchestrator, and the turn limit as steps". The agent label no longer
// travels in a header: it is in the scoped token (plan M3).
func TestOpenCodeConfig(t *testing.T) {
	var cfg struct {
		Schema     string `json:"$schema"`
		Autoupdate *bool  `json:"autoupdate"`
		Provider   struct {
			OpenRouter struct {
				Options struct {
					APIKey string `json:"apiKey"`
				} `json:"options"`
			} `json:"openrouter"`
		} `json:"provider"`
		Agent struct {
			Build struct {
				Steps int `json:"steps"`
			} `json:"build"`
		} `json:"agent"`
		MCP map[string]map[string]any `json:"mcp"`
	}
	orch := opencodeConfig(Turn{MaxTurns: 40, Token: "tok"}, "https://cs.example/mcp")
	if err := json.Unmarshal([]byte(orch), &cfg); err != nil {
		t.Fatalf("config is not JSON: %v\n%s", err, orch)
	}
	if cfg.Schema != "https://opencode.ai/config.json" || cfg.Autoupdate == nil || *cfg.Autoupdate {
		t.Errorf("schema %q, autoupdate %v", cfg.Schema, cfg.Autoupdate)
	}
	if cfg.Agent.Build.Steps != 40 {
		t.Errorf("steps = %d, want 40", cfg.Agent.Build.Steps)
	}
	if k := cfg.Provider.OpenRouter.Options.APIKey; k != "{env:OPENROUTER_API_KEY}" {
		t.Errorf("apiKey = %q", k)
	}
	want := map[string]map[string]any{"cinderline": {
		"type":    "remote",
		"url":     "https://cs.example/mcp",
		"headers": map[string]any{"Authorization": "Bearer {env:CINDERLINE_TOKEN}"},
		"oauth":   false,
	}}
	if !reflect.DeepEqual(cfg.MCP, want) {
		t.Errorf("mcp = %v, want %v", cfg.MCP, want)
	}
	// The secrets are read from the environment, never written into the config.
	if strings.Contains(orch, "tok\"") || strings.Contains(orch, "sk-") {
		t.Errorf("config holds a secret: %s", orch)
	}

	cfg.MCP = nil
	if err := json.Unmarshal([]byte(opencodeConfig(Turn{MaxTurns: 1}, "u")), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.MCP == nil || len(cfg.MCP) != 0 {
		t.Errorf("worker mcp = %v, want {}", cfg.MCP)
	}
}

// Ported from lib.test.mjs "agentEnv without project files skips CLAUDE.md
// and AGENTS.md" (OpenCode half) and "agentEnv gives the OpenRouter key only
// to OpenCode, and no Anthropic login" (OpenCode half: only the OpenRouter
// key, no other secret).
func TestOpenCodeEnv(t *testing.T) {
	orch := opencodeEnv("sk-or-k", Turn{MaxTurns: 3, Token: "tok"}, "u")
	want := []string{
		"XDG_DATA_HOME=/state/opencode/data",
		"XDG_CONFIG_HOME=/state/opencode/config",
		"OPENROUTER_API_KEY=sk-or-k",
		"OPENCODE_CONFIG_CONTENT=" + opencodeConfig(Turn{MaxTurns: 3, Token: "tok"}, "u"),
		"CINDERLINE_TOKEN=tok",
	}
	if !reflect.DeepEqual(orch, want) {
		t.Errorf("orchestrator env =\n%q\nwant\n%q", orch, want)
	}

	worker := opencodeEnv("sk-or-k", Turn{MaxTurns: 3}, "u")
	want = []string{
		"XDG_DATA_HOME=/state/opencode/data",
		"XDG_CONFIG_HOME=/state/opencode/config",
		"OPENROUTER_API_KEY=sk-or-k",
		"OPENCODE_CONFIG_CONTENT=" + opencodeConfig(Turn{MaxTurns: 3}, "u"),
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
	}
	if !reflect.DeepEqual(worker, want) {
		t.Errorf("worker env =\n%q\nwant\n%q", worker, want)
	}
}

// Ported from lib.test.mjs "parseRun reads the session and the last message
// of OpenCode JSON events". OpenCode has no turn limit to hit: after its
// steps it asks the model for a text-only answer.
func TestParseOpenCode(t *testing.T) {
	event := func(typ, part string) string {
		return `{"type":"` + typ + `","timestamp":1,"sessionID":"ses_abc","part":` + part + `}`
	}
	out := strings.Join([]string{
		"some log line on stderr",
		event("step_start", `{"messageID":"msg_1","type":"step-start"}`),
		event("text", `{"messageID":"msg_1","type":"text","text":"Looking."}`),
		event("tool_use", `{"messageID":"msg_1","type":"tool","tool":"bash"}`),
		event("step_finish", `{"messageID":"msg_1","reason":"tool-calls"}`),
		event("text", `{"messageID":"msg_2","type":"text","text":"Done: added X."}`),
		event("text", `{"messageID":"msg_2","type":"text","text":"Tests pass.\n"}`),
		event("step_finish", `{"messageID":"msg_2","reason":"stop"}`),
	}, "\n")
	// Neither step_finish carries tokens or a cost.
	want := Result{SessionID: "ses_abc", Report: "Done: added X.\n\nTests pass.", Usage: &Usage{}}
	if got := parseOpenCode(out); !reflect.DeepEqual(got, want) {
		t.Errorf("parseOpenCode = %+v, want %+v", got, want)
	}
	if got := parseOpenCode("no json\n{broken"); !reflect.DeepEqual(got, Result{}) {
		t.Errorf("parseOpenCode(no json) = %+v, want zero", got)
	}
}

// Ported from lib.test.mjs "parseRun sums the usage of the OpenCode steps".
func TestParseOpenCodeUsage(t *testing.T) {
	step := func(part string) string { return `{"type":"step_finish","sessionID":"s","part":` + part + `}` }
	out := step(`{"reason":"tool-calls","cost":0.01,"tokens":{"input":100,"output":10,"reasoning":5,"cache":{"read":50,"write":20}}}`) + "\n" +
		step(`{"reason":"stop","cost":0.02,"tokens":{"input":200,"output":30,"cache":{"read":70,"write":0}}}`)
	u := parseOpenCode(out).Usage
	if u == nil || u.CostUSD == nil || math.Round(*u.CostUSD*100) != 3 {
		t.Fatalf("usage = %+v, want a cost of 0.03", u)
	}
	// Reasoning tokens are billed as output.
	if got, want := (Usage{u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, nil}), (Usage{300, 45, 120, 20, nil}); got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
	if u := parseOpenCode(step(`{"tokens":{"input":1}}`)).Usage; u == nil || u.CostUSD != nil || u.InputTokens != 1 {
		t.Errorf("usage without cost = %+v, want 1 input token and no cost", u)
	}
}

// A real 1.19.5 run with an invalid OpenRouter key, recorded on 2026-11-06
// and trimmed of response headers: the session is kept, there is no report.
func TestParseOpenCodeRecordedError(t *testing.T) {
	out := `{"type":"error","timestamp":1793984449344,"sessionID":"ses_e4e31bb00ffeDTtaaRXCNF5InN","error":{"name":"APIError","data":{"message":"User not found.","statusCode":401,"isRetryable":false,"responseBody":"{\"error\":{\"message\":\"User not found.\",\"code\":401}}","metadata":{"url":"https://openrouter.ai/api/v1/chat/completions"}}}}`
	if got, want := parseOpenCode(out), (Result{SessionID: "ses_e4e31bb00ffeDTtaaRXCNF5InN"}); !reflect.DeepEqual(got, want) {
		t.Errorf("parseOpenCode = %+v, want %+v", got, want)
	}
}

// Run: a fresh turn runs the tools volume's opencode in the issue's clone
// with the prompt on stdin, only the OpenRouter key and the token, the musl
// libraries, the state volume prepared for the agent user, and the team's
// time limit; OpenCode names the session.
func TestOpenCodeRunFresh(t *testing.T) {
	out := "noise\n" + `{"type":"text","sessionID":"ses_1","part":{"messageID":"m","text":"Done."}}` + "\n" +
		`{"type":"step_finish","sessionID":"ses_1","part":{"tokens":{"output":5}}}`
	env := &environment.Fake{Runs: []environment.RunReply{{Output: out}}}
	var docker [][]string
	c := &OpenCode{
		Project: "CSR", Env: env, Key: "sk-or-k", MCPURL: "https://cs.example/mcp",
		Tools: tools.Mount{
			Args:        []string{"-v", "tools:/opt/cinderline:ro"},
			Env:         []string{"DISABLE_AUTOUPDATER=1"},
			OpenCodeEnv: []string{"LD_LIBRARY_PATH=/opt/cinderline/lib"},
		},
		Exec: func(_ context.Context, _ []string, argv ...string) (string, error) {
			docker = append(docker, argv)
			return "", nil
		},
	}
	turn := Turn{Dir: "/runs/CSR-7", Prompt: "do it", MaxTurns: 5, MaxMinutes: 30, Model: "openrouter/q/c", Token: "tok"}
	res, err := c.Run(context.Background(), turn)
	if err != nil {
		t.Fatal(err)
	}
	calls := env.Calls()
	if len(calls) != 1 {
		t.Fatalf("Environment calls = %+v", calls)
	}
	want := environment.Spec{
		Issue:   "CSR-7",
		Dir:     "/runs/CSR-7",
		Mounts:  []string{"-v", "cinderline-CSR-repo:/repo:ro", "-v", "cinderline-CSR-runs:/runs", "-v", "cinderline-CSR-state-opencode:/state", "-v", "tools:/opt/cinderline:ro"},
		Env:     append([]string{"DISABLE_AUTOUPDATER=1", "LD_LIBRARY_PATH=/opt/cinderline/lib"}, opencodeEnv("sk-or-k", turn, "https://cs.example/mcp")...),
		Cmd:     append([]string{"/opt/cinderline/bin/opencode"}, opencodeArgs(turn)...),
		Stdin:   "do it",
		Timeout: 30 * time.Minute,
		Allow:   []string{"openrouter.ai:443"},
	}
	if s := calls[0].Args[0].(environment.Spec); !reflect.DeepEqual(s, want) {
		t.Errorf("Spec =\n%+v\nwant\n%+v", s, want)
	}
	if res.SessionID != "ses_1" || res.Report != "Done." || res.Tail != environment.Tail(out) || res.Usage == nil || res.HitTurnLimit {
		t.Errorf("Result = %+v", res)
	}
	if len(docker) != 2 || !slices.Contains(docker[0], "volume") || !slices.Contains(docker[1], "chown 1000:1000 /s") {
		t.Errorf("state volume preparation = %q", docker)
	}
}

// Run: a resumed turn passes its session and keeps it when the container
// fails before OpenCode printed one; the failure is returned with the result.
func TestOpenCodeRunResumeFails(t *testing.T) {
	env := &environment.Fake{Runs: []environment.RunReply{{Output: "crash", Err: environment.ErrTimeout}}}
	c := &OpenCode{Project: "CSR", Env: env, Exec: func(context.Context, []string, ...string) (string, error) { return "", nil }}
	res, err := c.Run(context.Background(), Turn{Dir: "/runs/CSR-7", SessionID: "ses_1", MaxTurns: 5})
	if !errors.Is(err, environment.ErrTimeout) || res.SessionID != "ses_1" || res.Tail != "crash" {
		t.Errorf("Run = %+v, %v", res, err)
	}
	if cmd := env.Calls()[0].Args[0].(environment.Spec).Cmd; !reflect.DeepEqual(cmd[len(cmd)-2:], []string{"--session", "ses_1"}) {
		t.Errorf("Cmd = %q, want a resume of ses_1", cmd)
	}
}

func TestOpenCodeRunStateVolumeFails(t *testing.T) {
	env := &environment.Fake{}
	boom := errors.New("docker broke")
	c := &OpenCode{Project: "CSR", Env: env, Exec: func(context.Context, []string, ...string) (string, error) { return "", boom }}
	if _, err := c.Run(context.Background(), Turn{Dir: "/runs/CSR-7"}); !errors.Is(err, boom) {
		t.Errorf("Run err = %v, want %v", err, boom)
	}
	if len(env.Calls()) != 0 {
		t.Error("the turn ran without its state volume")
	}
}
