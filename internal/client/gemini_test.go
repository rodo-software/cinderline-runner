package client

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/tools"
)

// Gemini CLI's headless argv (checked by running 0.67.0): the prompt on
// stdin with an empty -p, JSONL events, every tool approved (the container
// is the sandbox), the workspace trusted (headless refuses an untrusted one,
// exit 55), a session resumed by its UUID.
func TestGeminiArgs(t *testing.T) {
	want := []string{"-p", "", "--output-format", "stream-json", "--yolo", "--skip-trust", "--model", "gemini-2.5-pro"}
	if got := geminiArgs(Turn{Model: "gemini-2.5-pro", Token: "tok"}); !reflect.DeepEqual(got, want) {
		t.Errorf("args =\n%q\nwant\n%q", got, want)
	}
	want = []string{"-p", "", "--output-format", "stream-json", "--yolo", "--skip-trust", "--resume", "739e1123-0386-4683-911a-96ff5f72dcf2"}
	if got := geminiArgs(Turn{SessionID: "739e1123-0386-4683-911a-96ff5f72dcf2"}); !reflect.DeepEqual(got, want) {
		t.Errorf("resumed args =\n%q\nwant\n%q", got, want)
	}
}

func decodeSettings(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("settings %q: %v", s, err)
	}
	return m
}

// The settings file: the auth type, the team's max_turns as
// model.maxSessionTurns, no updates or usage statistics, and for agents with
// Cinderline access the cinderline MCP server with the token read from the
// environment, so it is never written to disk, and the project's AGENTS.md as
// context next to GEMINI.md.
func TestGeminiSettings(t *testing.T) {
	orch := decodeSettings(t, geminiSettingsWithAuth(Turn{MaxTurns: 40, Token: "tok"}, "https://cs.example/mcp", geminiAuthType(false)))
	want := decodeSettings(t, `{
		"security": {"auth": {"selectedType": "gemini-api-key"}},
		"model": {"maxSessionTurns": 40},
		"general": {"enableAutoUpdate": false, "enableAutoUpdateNotification": false},
		"privacy": {"usageStatisticsEnabled": false},
		"context": {"fileName": ["AGENTS.md", "GEMINI.md"]},
		"mcpServers": {"cinderline": {"httpUrl": "https://cs.example/mcp", "headers": {"Authorization": "Bearer $CINDERLINE_TOKEN"}}}
	}`)
	if !reflect.DeepEqual(orch, want) {
		t.Errorf("orchestrator settings =\n%v\nwant\n%v", orch, want)
	}

	// A worker gets no MCP server and not the project's AGENTS.md (app
	// repository decision 2026-11-05-workers-skip-project-files); without a
	// turn limit the CLI's own default (none) applies; a Vertex key is a
	// vertex-ai auth.
	worker := decodeSettings(t, geminiSettingsWithAuth(Turn{}, "https://cs.example/mcp", geminiAuthType(true)))
	want = decodeSettings(t, `{
		"security": {"auth": {"selectedType": "vertex-ai"}},
		"general": {"enableAutoUpdate": false, "enableAutoUpdateNotification": false},
		"privacy": {"usageStatisticsEnabled": false}
	}`)
	if !reflect.DeepEqual(worker, want) {
		t.Errorf("worker settings =\n%v\nwant\n%v", worker, want)
	}
	if s := geminiSettingsWithAuth(Turn{MaxTurns: 3, Token: "tok"}, "u", geminiAuthType(false)); strings.Contains(s, "tok\"") {
		t.Errorf("settings %s hold the token", s)
	}
}

// Only the Gemini key (as GEMINI_API_KEY, or for Vertex express mode as
// GOOGLE_API_KEY with Vertex switched on), the settings and, for agents with
// Cinderline access, the scoped token enter the container; the sessions live
// under GEMINI_CLI_HOME on the state volume.
func TestGeminiEnv(t *testing.T) {
	orch := Turn{Token: "tok", MaxTurns: 9}
	want := []string{
		"GEMINI_CLI_HOME=/state/gemini",
		"GEMINI_API_KEY=AIza-key",
		"CINDERLINE_GEMINI_SETTINGS=" + geminiSettingsWithAuth(orch, "https://cs.example/mcp", geminiAuthType(false)),
		"CINDERLINE_TOKEN=tok",
	}
	if got := geminiEnv("AIza-key", false, orch, "https://cs.example/mcp"); !reflect.DeepEqual(got, want) {
		t.Errorf("orchestrator env =\n%q\nwant\n%q", got, want)
	}
	want = []string{
		"GEMINI_CLI_HOME=/state/gemini",
		"GOOGLE_GENAI_USE_VERTEXAI=true",
		"GOOGLE_API_KEY=AQ-key",
		"CINDERLINE_GEMINI_SETTINGS=" + geminiSettingsWithAuth(Turn{}, "https://cs.example/mcp", geminiAuthType(true)),
	}
	if got := geminiEnv("AQ-key", true, Turn{}, "https://cs.example/mcp"); !reflect.DeepEqual(got, want) {
		t.Errorf("vertex worker env =\n%q\nwant\n%q", got, want)
	}
}

// Recorded from 0.67.0 (-o stream-json) with a tool call and a report the
// API streamed in two chunks, with warnings on stderr in between. The
// session is init's session_id; the report the assistant text after the last
// tool call; the usage the result's stats: input without the cached tokens,
// output as total minus input, which counts the thinking tokens Gemini bills
// as output. Gemini prints no cost.
func TestParseGemini(t *testing.T) {
	out := strings.Join([]string{
		"YOLO mode is enabled. All tool calls will be automatically approved.",
		`{"type":"init","timestamp":"2026-11-08T18:08:40.563Z","session_id":"02fc2568-0a78-4896-974d-8ce6cf77f85d","model":"gemini-2.5-pro"}`,
		`{"type":"message","timestamp":"2026-11-08T18:08:40.564Z","role":"user","content":"hi\n"}`,
		`{"type":"message","timestamp":"2026-11-08T18:08:40.570Z","role":"assistant","content":"Looking first.","delta":true}`,
		`{"type":"tool_use","timestamp":"2026-11-08T18:08:40.601Z","tool_name":"list_directory","tool_id":"list_directory__list_directory_1794161320583_0","parameters":{"dir_path":"."}}`,
		`{"type":"tool_result","timestamp":"2026-11-08T18:08:40.632Z","tool_id":"list_directory__list_directory_1794161320583_0","status":"success"}`,
		"Ripgrep is not available. Falling back to GrepTool.",
		`{"type":"message","timestamp":"2026-11-08T18:08:40.639Z","role":"assistant","content":"Done: ","delta":true}`,
		`{"type":"message","timestamp":"2026-11-08T18:08:40.639Z","role":"assistant","content":"added X.\n","delta":true}`,
		`{"type":"result","timestamp":"2026-11-08T18:08:40.640Z","status":"success","stats":{"total_tokens":2500,"input_tokens":2400,"output_tokens":60,"cached":1600,"input":800,"duration_ms":77,"tool_calls":1,"models":{"gemini-2.5-pro":{"total_tokens":2500,"input_tokens":2400,"output_tokens":60,"cached":1600,"input":800}}}}`,
	}, "\n")
	want := Result{
		SessionID: "02fc2568-0a78-4896-974d-8ce6cf77f85d",
		Report:    "Done: added X.",
		Usage:     &Usage{InputTokens: 800, OutputTokens: 100, CacheReadTokens: 1600},
	}
	if got := parseGemini(out); !reflect.DeepEqual(got, want) {
		t.Errorf("parseGemini = %+v (usage %+v), want %+v", got, got.Usage, want)
	}
	if got := parseGemini("no json\n{broken"); !reflect.DeepEqual(got, Result{}) {
		t.Errorf("parseGemini(no json) = %+v, want zero", got)
	}
}

// Recorded from 0.67.0 with model.maxSessionTurns 1: the CLI stops after the
// first tool call, exits 53 and ends with a result of status error and type
// FatalTurnLimitedError.
const geminiTurnLimitOutput = `{"type":"init","timestamp":"2026-11-08T18:08:39.431Z","session_id":"739e1123-0386-4683-911a-96ff5f72dcf2","model":"gemini-2.5-pro"}
{"type":"message","timestamp":"2026-11-08T18:08:39.432Z","role":"user","content":"hi\n"}
{"type":"tool_use","timestamp":"2026-11-08T18:08:39.471Z","tool_name":"list_directory","tool_id":"list_directory__list_directory_1794161319450_0","parameters":{"dir_path":"."}}
{"type":"tool_result","timestamp":"2026-11-08T18:08:39.502Z","tool_id":"list_directory__list_directory_1794161319450_0","status":"success"}
{"type":"result","timestamp":"2026-11-08T18:08:39.503Z","status":"error","error":{"type":"FatalTurnLimitedError","message":"Reached max session turns for this session. Increase the number of turns by specifying maxSessionTurns in settings.json."},"stats":{"total_tokens":1250,"input_tokens":1200,"output_tokens":30,"cached":800,"input":400,"duration_ms":0,"tool_calls":1,"models":{"gemini-2.5-pro":{"total_tokens":1250,"input_tokens":1200,"output_tokens":30,"cached":800,"input":400}}}}`

func TestParseGeminiTurnLimit(t *testing.T) {
	want := Result{
		SessionID:    "739e1123-0386-4683-911a-96ff5f72dcf2",
		HitTurnLimit: true,
		Usage:        &Usage{InputTokens: 400, OutputTokens: 50, CacheReadTokens: 800},
	}
	if got := parseGemini(geminiTurnLimitOutput); !reflect.DeepEqual(got, want) {
		t.Errorf("parseGemini = %+v (usage %+v), want %+v", got, got.Usage, want)
	}
	// Another error is not the turn limit.
	other := `{"type":"init","session_id":"s1"}` + "\n" + `{"type":"result","status":"error","error":{"type":"FatalAuthenticationError","message":"API key not valid"}}`
	if got := parseGemini(other); got.HitTurnLimit || got.SessionID != "s1" {
		t.Errorf("parseGemini(auth error) = %+v", got)
	}
}

func geminiTools() tools.Mount {
	return tools.Mount{
		Args:        []string{"-v", "tools:/opt/cinderline:ro"},
		Env:         []string{"DISABLE_AUTOUPDATER=1"},
		OpenCodeEnv: []string{"LD_LIBRARY_PATH=/opt/cinderline/lib"},
	}
}

// Run: a fresh turn writes the settings to GEMINI_CLI_HOME/.gemini through
// sh, then runs the tools volume's gemini bundle with its node, with the
// prompt on stdin, the musl libraries node needs, only the key and the token,
// the state volume prepared and the team's time limit.
func TestGeminiRunFresh(t *testing.T) {
	out := `{"type":"init","session_id":"s-1","model":"gemini-2.5-pro"}` + "\n" +
		`{"type":"message","role":"assistant","content":"Done.","delta":true}` + "\n" +
		`{"type":"result","status":"success","stats":{"total_tokens":6,"input_tokens":5,"output_tokens":1,"cached":0,"input":5}}`
	env := &environment.Fake{Runs: []environment.RunReply{{Output: out}}}
	var docker [][]string
	c := &Gemini{
		Project: "CSR", Env: env, Key: "AIza-key", MCPURL: "https://cs.example/mcp", Tools: geminiTools(),
		Exec: func(_ context.Context, _ []string, argv ...string) (string, error) {
			docker = append(docker, argv)
			return "", nil
		},
	}
	turn := Turn{Dir: "/runs/CSR-7", Prompt: "do it", MaxTurns: 5, MaxMinutes: 30, Model: "gemini-2.5-pro", Author: "Dev / Coder", Token: "tok"}
	res, err := c.Run(context.Background(), turn)
	if err != nil {
		t.Fatal(err)
	}
	calls := env.Calls()
	if len(calls) != 1 {
		t.Fatalf("Environment calls = %+v", calls)
	}
	want := environment.Spec{
		Issue:  "CSR-7",
		Dir:    "/runs/CSR-7",
		Mounts: []string{"-v", "cinderline-CSR-repo:/repo:ro", "-v", "cinderline-CSR-runs:/runs", "-v", "cinderline-CSR-state-gemini:/state", "-v", "tools:/opt/cinderline:ro"},
		Env:    slices.Concat([]string{"DISABLE_AUTOUPDATER=1", "LD_LIBRARY_PATH=/opt/cinderline/lib"}, geminiEnv("AIza-key", false, turn, "https://cs.example/mcp"), wantIdentity),
		Cmd: append([]string{
			"sh", "-c", `mkdir -p "$GEMINI_CLI_HOME/.gemini" && printf '%s' "$CINDERLINE_GEMINI_SETTINGS" > "$GEMINI_CLI_HOME/.gemini/settings.json" && exec "$0" "$@"`,
			"/opt/cinderline/bin/node", "/opt/cinderline/gemini/bundle/gemini.js",
		}, geminiArgs(turn)...),
		Stdin:   "do it",
		Timeout: 30 * time.Minute,
		Allow:   []string{"generativelanguage.googleapis.com:443"},
	}
	if s := calls[0].Args[0].(environment.Spec); !reflect.DeepEqual(s, want) {
		t.Errorf("Spec =\n%+v\nwant\n%+v", s, want)
	}
	if res.SessionID != "s-1" || res.Report != "Done." || res.Tail != environment.Tail(out) || res.Usage == nil || res.HitTurnLimit {
		t.Errorf("Result = %+v", res)
	}
	if len(docker) != 2 || !slices.Contains(docker[0], "volume") || !slices.Contains(docker[1], "chown 1000:1000 /s") {
		t.Errorf("state volume preparation = %q", docker)
	}
}

// Run: the turn limit is an error with HitTurnLimit and the session set, so
// Attempts continues the session.
func TestGeminiRunTurnLimit(t *testing.T) {
	env := &environment.Fake{Runs: []environment.RunReply{{Output: geminiTurnLimitOutput, Err: errors.New("exit status 53")}}}
	c := &Gemini{Project: "CSR", Env: env, Exec: func(context.Context, []string, ...string) (string, error) { return "", nil }}
	res, err := c.Run(context.Background(), Turn{Dir: "/runs/CSR-7", MaxTurns: 1})
	if err == nil || !res.HitTurnLimit || res.SessionID != "739e1123-0386-4683-911a-96ff5f72dcf2" {
		t.Errorf("Run = %+v, %v", res, err)
	}
}

// Run: a resumed turn passes its session and keeps it when the container
// fails before Gemini printed one; the failure is returned with the result.
func TestGeminiRunResumeFails(t *testing.T) {
	env := &environment.Fake{Runs: []environment.RunReply{{Output: "crash", Err: environment.ErrTimeout}}}
	c := &Gemini{Project: "CSR", Env: env, Exec: func(context.Context, []string, ...string) (string, error) { return "", nil }}
	res, err := c.Run(context.Background(), Turn{Dir: "/runs/CSR-7", SessionID: "s-1"})
	if !errors.Is(err, environment.ErrTimeout) || res.SessionID != "s-1" || res.Tail != "crash" {
		t.Errorf("Run = %+v, %v", res, err)
	}
	if cmd := env.Calls()[0].Args[0].(environment.Spec).Cmd; !reflect.DeepEqual(cmd[len(cmd)-2:], []string{"--resume", "s-1"}) {
		t.Errorf("Cmd = %q, want a resume of s-1", cmd)
	}
}

func TestGeminiRunStateVolumeFails(t *testing.T) {
	env := &environment.Fake{}
	boom := errors.New("docker broke")
	c := &Gemini{Project: "CSR", Env: env, Exec: func(context.Context, []string, ...string) (string, error) { return "", boom }}
	if _, err := c.Run(context.Background(), Turn{Dir: "/runs/CSR-7"}); !errors.Is(err, boom) {
		t.Errorf("Run err = %v, want %v", err, boom)
	}
	if len(env.Calls()) != 0 {
		t.Error("the turn ran without its state volume")
	}
}
