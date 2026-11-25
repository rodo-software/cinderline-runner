package client

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/tools"
)

// codex exec's headless argv (checked against the 0.167.0 source): JSONL
// events, no approvals or sandbox (the container is the sandbox), the
// prompt on stdin through "-", options before the resume subcommand.
func TestCodexArgs(t *testing.T) {
	orch := codexArgs(Turn{Model: "gpt-5.5", Token: "tok"}, "https://cs.example/mcp", "")
	want := []string{"exec", "--json", "--dangerously-bypass-approvals-and-sandbox",
		"-c", "features.plugins=false", "-c", "features.api_key_model_discovery=false",
		"--model", "gpt-5.5",
		"-c", `mcp_servers.cinderline.url="https://cs.example/mcp"`,
		"-c", `mcp_servers.cinderline.bearer_token_env_var="CINDERLINE_TOKEN"`,
		"-"}
	if !reflect.DeepEqual(orch, want) {
		t.Errorf("orchestrator args =\n%q\nwant\n%q", orch, want)
	}

	// A worker gets no MCP server and skips the project's AGENTS.md (app
	// repository decision 2026-11-05-workers-skip-project-files).
	worker := codexArgs(Turn{}, "https://cs.example/mcp", "")
	want = []string{"exec", "--json", "--dangerously-bypass-approvals-and-sandbox",
		"-c", "features.plugins=false", "-c", "features.api_key_model_discovery=false",
		"-c", "project_doc_max_bytes=0", "-"}
	if !reflect.DeepEqual(worker, want) {
		t.Errorf("worker args =\n%q\nwant\n%q", worker, want)
	}

	resumed := codexArgs(Turn{SessionID: "019a5c3e-0000-7000-8000-000000000001"}, "u", "")
	if got := resumed[len(resumed)-3:]; !reflect.DeepEqual(got, []string{"resume", "019a5c3e-0000-7000-8000-000000000001", "-"}) {
		t.Errorf("resumed args end %q, want resume <id> -", got)
	}

	// No startup fetch from hosts the turn does not need (CLR-242): the
	// curated plugin sync (github.com) and an API key run's model list
	// (chatgpt.com), on every turn, a resumed one included.
	for _, a := range [][]string{orch, worker, resumed, codexArgs(Turn{}, "u", "http://host.docker.internal:11434/v1")} {
		opts := a
		if r := slices.Index(a, "resume"); r >= 0 {
			opts = a[:r]
		}
		for _, kv := range []string{"features.plugins=false", "features.api_key_model_discovery=false"} {
			if i := slices.Index(opts, kv); i < 1 || opts[i-1] != "-c" {
				t.Errorf("args %q do not set %s before any resume", a, kv)
			}
		}
	}

	// The slot's effort as model_reasoning_effort (app AgentClient::efforts, EMBER-132).
	effort := codexArgs(Turn{Effort: "high"}, "u", "")
	if i := slices.Index(effort, `model_reasoning_effort="high"`); i < 1 || effort[i-1] != "-c" {
		t.Errorf("args %q do not set the effort", effort)
	}
	if slices.ContainsFunc(worker, func(s string) bool { return strings.HasPrefix(s, "model_reasoning_effort") }) {
		t.Errorf("args %q set an effort without one", worker)
	}

	based := codexArgs(Turn{}, "u", "http://host.docker.internal:11434/v1")
	if i := slices.Index(based, `openai_base_url="http://host.docker.internal:11434/v1"`); i < 1 || based[i-1] != "-c" {
		t.Errorf("args %q do not set the base URL", based)
	}
	for _, a := range append(orch, worker...) {
		if strings.Contains(a, "openai_base_url") || strings.Contains(a, "tok\"") {
			t.Errorf("arg %q: base URL without one configured, or a secret in the argv", a)
		}
	}
}

// Only the OpenAI key and, for agents with Cinderline access, the scoped
// token enter the container; sessions and config live on the state volume.
func TestCodexEnv(t *testing.T) {
	if got, want := codexEnv("sk-oa", Turn{Token: "tok"}), []string{"CODEX_HOME=/state/codex", "CODEX_API_KEY=sk-oa", "CINDERLINE_TOKEN=tok"}; !reflect.DeepEqual(got, want) {
		t.Errorf("orchestrator env = %q, want %q", got, want)
	}
	if got, want := codexEnv("sk-oa", Turn{}), []string{"CODEX_HOME=/state/codex", "CODEX_API_KEY=sk-oa"}; !reflect.DeepEqual(got, want) {
		t.Errorf("worker env = %q, want %q", got, want)
	}
}

// JSONL events in the shape of codex-rs/exec/src/exec_events.rs at 0.167.0:
// the session is thread.started's thread_id, the report the last
// agent_message, the usage turn.completed's, the tool calls the completed
// tool-call items. Input tokens exclude the cached
// ones, as Codex's own non_cached_input does; Codex prints no cost.
func TestParseCodex(t *testing.T) {
	out := strings.Join([]string{
		"2026-11-08T16:00:00Z WARN some log line on stderr",
		`{"type":"thread.started","thread_id":"019a5c3e-0000-7000-8000-000000000001"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"reasoning","text":"Thinking."}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"Looking."}}`,
		`{"type":"item.started","item":{"id":"item_2","type":"command_execution","command":"go test ./...","aggregated_output":"","exit_code":null,"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"command_execution","command":"go test ./...","aggregated_output":"ok","exit_code":0,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_4","type":"file_change","changes":[{"path":"a.go","kind":"update"}],"status":"completed"}}`,
		`{"type":"item.started","item":{"id":"item_5","type":"mcp_tool_call","server":"cinderline","tool":"get_issue","arguments":{},"result":null,"error":null,"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_5","type":"mcp_tool_call","server":"cinderline","tool":"get_issue","arguments":{},"result":null,"error":null,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_6","type":"web_search","query":"go vet"}}`,
		`{"type":"item.completed","item":{"id":"item_7","type":"command_execution","command":"go vet ./...","aggregated_output":"","exit_code":0,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_8","type":"todo_list","items":[]}}`,
		`{"type":"item.completed","item":{"id":"item_9","type":"mcp_tool_call","status":"failed"}}`,
		`{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"Done: added X.\n"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1200,"cached_input_tokens":1000,"cache_write_input_tokens":0,"output_tokens":300,"reasoning_output_tokens":120}}`,
	}, "\n")
	want := Result{
		SessionID: "019a5c3e-0000-7000-8000-000000000001",
		Report:    "Done: added X.",
		Usage:     &Usage{InputTokens: 200, OutputTokens: 300, CacheReadTokens: 1000},
		// turn.completed's usage is the thread's total so far, restored on
		// a resume (checked with 0.167.0, CLR-232).
		UsageTotal: true,
		// Codex's events name a tool call by its item type, and an MCP
		// call by server and tool, as Codex names MCP tools for the model.
		// One without either is counted by its type.
		Tools: map[string]int{"command_execution": 2, "file_change": 1, "mcp__cinderline__get_issue": 1, "mcp_tool_call": 1, "web_search": 1},
	}
	if got := parseCodex(out); !reflect.DeepEqual(got, want) {
		t.Errorf("parseCodex = %+v (usage %+v), want %+v", got, got.Usage, want)
	}
	if got := parseCodex("no json\n{broken"); !reflect.DeepEqual(got, Result{}) {
		t.Errorf("parseCodex(no json) = %+v, want zero", got)
	}
}

// A resumed thread's turn.completed includes the earlier turns' usage, as
// measured with 0.167.0 (CLR-232): Since takes the total before it off,
// each count at least 0, as a lower total must not report a negative turn.
func TestCodexUsageSince(t *testing.T) {
	first := parseCodex(`{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":100,"output_tokens":10}}`).Usage
	second := parseCodex(`{"type":"turn.completed","usage":{"input_tokens":3000,"cached_input_tokens":300,"output_tokens":30}}`).Usage
	if got, want := second.Since(*first), (Usage{InputTokens: 1800, CacheReadTokens: 200, OutputTokens: 20}); got != want {
		t.Errorf("Since = %+v, want %+v", got, want)
	}
	if got, want := first.Since(*second), (Usage{}); got != want {
		t.Errorf("Since a larger total = %+v, want %+v", got, want)
	}
}

// A failed turn keeps the session and has neither report nor usage.
func TestParseCodexFailedTurn(t *testing.T) {
	out := `{"type":"thread.started","thread_id":"019a5c3e-0000-7000-8000-000000000002"}` + "\n" +
		`{"type":"turn.started"}` + "\n" +
		`{"type":"error","message":"unexpected status 401 Unauthorized: Incorrect API key provided"}` + "\n" +
		`{"type":"turn.failed","error":{"message":"unexpected status 401 Unauthorized: Incorrect API key provided"}}`
	if got, want := parseCodex(out), (Result{SessionID: "019a5c3e-0000-7000-8000-000000000002"}); !reflect.DeepEqual(got, want) {
		t.Errorf("parseCodex = %+v, want %+v", got, want)
	}
}

// Run: a fresh turn runs the tools volume's codex through sh, which makes
// CODEX_HOME first (Codex refuses a CODEX_HOME that does not exist), with
// the prompt on stdin, only the OpenAI key and the token, the state volume
// prepared and the team's time limit. Codex has no turn limit to hit.
func TestCodexRunFresh(t *testing.T) {
	out := "noise\n" + `{"type":"thread.started","thread_id":"th_1"}` + "\n" +
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"Done."}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":5,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}`
	env := &environment.Fake{Runs: []environment.RunReply{{Output: out}}}
	var docker [][]string
	c := &CLI{
		Name: "codex", Project: "CSR", Env: env, Credential: "sk-oa", MCPURL: "https://cs.example/mcp",
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
	turn := Turn{Dir: "/runs/CSR-7", Prompt: "do it", MaxTurns: 5, MaxMinutes: 30, Idle: 10 * time.Minute, Model: "gpt-5.5", Author: "Dev / Coder", Token: "tok"}
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
		Mounts:  []string{"-v", "cinderline-CSR-repo:/repo:ro", "--mount", "type=volume,src=cinderline-CSR-runs,dst=/runs/CSR-7,volume-subpath=CSR-7", "-v", "cinderline-CSR-state-codex:/state", "-v", "tools:/opt/cinderline:ro"},
		Env:     slices.Concat([]string{"DISABLE_AUTOUPDATER=1"}, codexEnv("sk-oa", turn), wantIdentity),
		Cmd:     append([]string{"sh", "-c", `mkdir -p "$CODEX_HOME" && export PATH="/opt/cinderline/rg:$PATH" && exec "$0" "$@"`, "/opt/cinderline/bin/codex"}, codexArgs(turn, "https://cs.example/mcp", "")...),
		Stdin:   "do it",
		Timeout: 30 * time.Minute,
		Idle:    10 * time.Minute,
		Allow:   []string{"api.openai.com:443"},
	}
	if s := calls[0].Args[0].(environment.Spec); !reflect.DeepEqual(s, want) {
		t.Errorf("Spec =\n%+v\nwant\n%+v", s, want)
	}
	if res.SessionID != "th_1" || res.Report != "Done." || res.Tail != environment.Tail(out) || res.Usage == nil || res.HitTurnLimit {
		t.Errorf("Result = %+v", res)
	}
	if len(docker) != 2 || !slices.Contains(docker[0], "volume") || !slices.Contains(docker[1], "chown 1000:1000 /s") {
		t.Errorf("state volume preparation = %q", docker)
	}
}

// Run: a resumed turn passes its session and keeps it when the container
// fails before Codex printed one; the failure is returned with the result.
func TestCodexRunResumeFails(t *testing.T) {
	env := &environment.Fake{Runs: []environment.RunReply{{Output: "crash", Err: environment.ErrTimeout}}}
	c := &CLI{Name: "codex", Project: "CSR", Env: env, Exec: func(context.Context, []string, ...string) (string, error) { return "", nil }}
	res, err := c.Run(context.Background(), Turn{Dir: "/runs/CSR-7", SessionID: "th_1"})
	if !errors.Is(err, environment.ErrTimeout) || res.SessionID != "th_1" || res.Tail != "crash" {
		t.Errorf("Run = %+v, %v", res, err)
	}
	if cmd := env.Calls()[0].Args[0].(environment.Spec).Cmd; !reflect.DeepEqual(cmd[len(cmd)-3:], []string{"resume", "th_1", "-"}) {
		t.Errorf("Cmd = %q, want a resume of th_1", cmd)
	}
}

func TestCodexRunStateVolumeFails(t *testing.T) {
	env := &environment.Fake{}
	boom := errors.New("docker broke")
	c := &CLI{Name: "codex", Project: "CSR", Env: env, Exec: func(context.Context, []string, ...string) (string, error) { return "", boom }}
	if _, err := c.Run(context.Background(), Turn{Dir: "/runs/CSR-7"}); !errors.Is(err, boom) {
		t.Errorf("Run err = %v, want %v", err, boom)
	}
	if len(env.Calls()) != 0 {
		t.Error("the turn ran without its state volume")
	}
}
