//go:build smoke

package client_test

// The smoke check for a bump of the agent CLI pins (CLR-261, decision
// 2026-11-26-client-smoke-check). Per client it runs short real turns
// against the real provider through the runner's own CLI Client and Docker
// Environment, with the pinned tools volume: so the argv, environment,
// mounts, proxy and parsing are the runner's, not a copy. It needs Docker
// and model keys, so it runs on a person's machine, never in CI or in an
// agent's container:
//
//	scripts/smoke-clients.sh [client ...]
//
// A client runs when its key is in the environment and is skipped otherwise:
// claude ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN, codex CODEX_API_KEY or
// OPENAI_API_KEY, gemini GEMINI_API_KEY, opencode OPENROUTER_API_KEY with
// OPENCODE_MODEL (openrouter/<model id>). <CLIENT>_MODEL picks a client's
// model; empty: the client's default, as an agent without a model gets.
// Each check prints one line, "PASS <client> <check>" or "FAIL <client>
// <check>: <why>", and a fail fails the test.

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/ca"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// smokeClients are the clients with the environment variables that hold
// their key, the first one set wins, and whether they have a turn limit to
// hit: Codex has none and OpenCode's steps end in a report (codex.go,
// opencode.go).
var smokeClients = []struct {
	name      string
	keys      []string
	turnLimit bool
}{
	{"claude", []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}, true},
	{"codex", []string{"CODEX_API_KEY", "OPENAI_API_KEY"}, false},
	{"gemini", []string{"GEMINI_API_KEY"}, true},
	{"opencode", []string{"OPENROUTER_API_KEY"}, false},
}

// probeSkill is the SKILL.md of the probe package; %s is the code word,
// which only the skill holds.
const probeSkill = `---
name: smoke-probe
description: Gives the code word of the runner's smoke check. Use it whenever you are asked for the code word.
---

The code word is %s. Reply with it exactly.
`

func TestSmokeClients(t *testing.T) {
	// A client runs with its key and model; none left: nothing to set up.
	creds, models := map[string]string{}, map[string]string{}
	for _, c := range smokeClients {
		var cred string
		for _, k := range c.keys {
			cred = cmp.Or(cred, os.Getenv(k))
		}
		models[c.name] = os.Getenv(strings.ToUpper(c.name) + "_MODEL")
		if cred == "" || c.name == "opencode" && models[c.name] == "" {
			need := strings.Join(c.keys, " or ")
			if c.name == "opencode" {
				need += " and OPENCODE_MODEL"
			}
			fmt.Printf("SKIP %s: set %s\n", c.name, need)
			continue
		}
		creds[c.name] = cred
	}
	if len(creds) == 0 {
		t.Skip("no client has its key in the environment")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	t.Cleanup(cancel)
	x := workspace.RunCommand
	project := "SMOKE" + strconv.Itoa(os.Getpid())
	state := t.TempDir()
	log := slog.New(slog.NewTextHandler(t.Output(), nil))

	// No project hosts at all: the proxy lets a turn reach its client's
	// hosts and the MCP server below, and refuses and reports the rest.
	var mu sync.Mutex
	refused := map[string][]environment.Refusal{} // by issue, one per client
	docker := &environment.Docker{
		Project: project,
		Exec:    x,
		Log:     log,
		Memory:  4 << 30,
		CA:      ca.Dir(state),
		OnRefused: func(issue string, r environment.Refusal) {
			mu.Lock()
			defer mu.Unlock()
			refused[issue] = append(refused[issue], r)
		},
	}
	label := "label=" + workspace.ProjectLabel(project)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(ctx)
		if err := docker.Cleanup(ctx); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		if out, err := x(ctx, nil, "docker", "volume", "ls", "--quiet", "--filter", label); err == nil && out != "" {
			_, _ = x(ctx, nil, append([]string{"docker", "volume", "rm", "--force"}, strings.Fields(out)...)...)
		}
	})

	mcp := &mcpServer{tokens: map[string]string{}, listed: map[string]bool{}}
	ln, err := net.Listen("tcp", "0.0.0.0:0") // the proxy reaches it through the host gateway
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(mcp)
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	gateway := environment.HostGateway + ":" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	env := withMCP{docker, gateway}

	image, err := docker.Image(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mount, err := (&tools.Tools{State: state, Exec: x}).Mount(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	labels := workspace.Labels(project, "")
	for _, kind := range []string{"repo", "runs"} {
		if _, err := x(ctx, nil, slices.Concat([]string{"docker", "volume", "create"}, labels, []string{workspace.Volume(project, kind)})...); err != nil {
			t.Fatal(err)
		}
	}
	word := "smoke-" + randomHex()
	skill := probePackage(t, ctx, x, log, project, state, word)

	for _, c := range smokeClients {
		cred, model := creds[c.name], models[c.name]
		if cred == "" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ref := project + "-" + c.name
			dir := workspace.RunsPath + "/" + ref
			if _, err := x(ctx, nil, "docker", "run", "--rm", "--network", "none", "-v", workspace.Volume(project, "runs")+":"+workspace.RunsPath,
				"--entrypoint", "sh", workspace.GitImage, "-c", `git init --quiet "$1" && chown -R `+workspace.AgentUser+` "$1"`, "sh", dir); err != nil {
				fmt.Printf("FAIL %s setup: %v\n", c.name, err)
				t.Fatal(err)
			}
			token := randomHex()
			mcp.add(token, c.name)
			cl := &client.CLI{Name: c.name, Project: project, Env: env, Exec: x, Credential: cred, MCPURL: "http://" + gateway + "/mcp", Tools: mount}
			s := &smoke{t: t, client: c.name}

			// Turn 1: a fresh session with the MCP server and the probe package.
			remember := "word-" + randomHex()
			turn := client.Turn{Dir: dir, MaxTurns: 10, MaxMinutes: 10, Model: model, Token: token, Skills: []client.SkillMount{skill},
				Prompt: "Remember the word " + remember + ". Then use your smoke-probe skill to find the code word, and reply with only the code word."}
			r1, err := cl.Run(ctx, turn)
			s.check("turn", err == nil, "%v\n%s", err, r1.Tail)
			s.check("session", r1.SessionID != "", "no session ID parsed")
			s.check("usage", r1.Usage != nil && r1.Usage.InputTokens+r1.Usage.CacheReadTokens+r1.Usage.OutputTokens > 0, "usage %+v", r1.Usage)
			s.check("mcp", mcp.didList(c.name), "the CLI never listed the MCP server's tools with the turn's token")
			s.check("skill", strings.Contains(r1.Report, word), "report %q lacks the skill's code word", r1.Report)

			// Turn 2 resumes it: only the session knows the word.
			if r1.SessionID != "" {
				turn.SessionID = r1.SessionID
				turn.Prompt = "Which word did I ask you to remember? Reply with only that word."
				r2, err := cl.Run(ctx, turn)
				s.check("resume", err == nil && r2.SessionID == r1.SessionID && strings.Contains(r2.Report, remember),
					"%v, session %q, report %q\n%s", err, r2.SessionID, r2.Report, r2.Tail)
			} else {
				s.check("resume", false, "no session to resume")
			}

			// Turn 3: one turn allowed, two tool calls asked.
			if c.turnLimit {
				r3, err := cl.Run(ctx, client.Turn{Dir: dir, MaxTurns: 1, MaxMinutes: 10, Model: model,
					Prompt: "Run the shell command `date`, then the shell command `pwd`, each as its own tool call, then reply with both outputs."})
				s.check("turn-limit", r3.HitTurnLimit, "not recognised: %v\n%s", err, r3.Tail)
			} else {
				fmt.Printf("SKIP %s turn-limit: the client has no turn limit\n", c.name)
			}

			mu.Lock()
			hosts := refused[ref]
			mu.Unlock()
			var why []string
			for _, r := range hosts {
				why = append(why, strings.TrimSpace(r.Host+" ("+r.Reason+") "+r.Method+" "+r.URL))
			}
			s.check("hosts", len(hosts) == 0, "refused outside the client's hosts: %s", strings.Join(why, ", "))
			s.print()
		})
	}
}

// smoke collects one client's check lines, printed together so parallel
// clients do not interleave.
type smoke struct {
	t      *testing.T
	client string
	lines  []string
}

func (s *smoke) check(name string, ok bool, format string, args ...any) {
	if ok {
		s.lines = append(s.lines, "PASS "+s.client+" "+name)
		return
	}
	why := fmt.Sprintf(format, args...)
	first, rest, _ := strings.Cut(why, "\n")
	s.lines = append(s.lines, "FAIL "+s.client+" "+name+": "+first)
	s.t.Errorf("%s %s: %s\n%s", s.client, name, first, rest) // the output tail goes to the log only
}

func (s *smoke) print() { fmt.Print(strings.Join(s.lines, "\n") + "\n") }

// probePackage fills a skill package volume with one skill that holds word,
// as a fetched package's volume looks, and reads it the runner's way.
func probePackage(t *testing.T, ctx context.Context, x workspace.Exec, log *slog.Logger, project, state, word string) client.SkillMount {
	t.Helper()
	const url, commit = "https://smoke.invalid/probe", "smoke"
	vol := tools.SkillVolume(url, commit)
	if _, err := x(ctx, nil, slices.Concat([]string{"docker", "volume", "create"}, workspace.Labels(project, ""), []string{vol})...); err != nil {
		t.Fatal(err)
	}
	dir := "/t/" + workspace.SkillRepo + "/skills/smoke-probe"
	if _, err := x(ctx, nil, "docker", "run", "--rm", "--network", "none", "-v", vol+":/t", "--entrypoint", "sh", workspace.GitImage,
		"-c", `mkdir -p "$1" && printf '%s' "$2" > "$1/SKILL.md" && touch /t/complete`, "sh", dir, fmt.Sprintf(probeSkill, word)); err != nil {
		t.Fatal(err)
	}
	vol, contents, err := (&tools.Skills{State: state, Exec: x, Log: log, Project: project}).Fill(ctx, url, commit, "")
	if err != nil || !slices.Equal(contents.Skills, []string{"smoke-probe"}) {
		t.Fatalf("probe package = %+v, %v", contents, err)
	}
	return client.SkillMount{Name: "smoke", Volume: vol, SkillContents: contents}
}

// withMCP lets every turn reach the MCP server on this machine, which the
// runner's Cinderline is not.
type withMCP struct {
	*environment.Docker
	gateway string
}

func (e withMCP) Run(ctx context.Context, s environment.Spec) (string, error) {
	s.Allow = append(slices.Clone(s.Allow), e.gateway)
	return e.Docker.Run(ctx, s)
}

// mcpServer is a minimal MCP server over streamable HTTP with one tool. It
// answers only a known bearer token, and records which client listed its
// tools: that proves the CLI took the runner's MCP config, token included.
type mcpServer struct {
	mu     sync.Mutex
	tokens map[string]string // client by token
	listed map[string]bool   // by client
}

func (m *mcpServer) add(token, client string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[token] = client
}

func (m *mcpServer) didList(client string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listed[client]
}

func (m *mcpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	m.mu.Lock()
	client, ok := m.tokens[token]
	m.mu.Unlock()
	if !ok {
		http.Error(w, "unknown token", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost { // no server-sent event stream
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == nil { // a notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	switch req.Method {
	case "initialize":
		resp["result"] = map[string]any{
			"protocolVersion": cmp.Or(req.Params.ProtocolVersion, "2025-06-18"),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "smoke", "version": "1"},
		}
	case "tools/list":
		m.mu.Lock()
		m.listed[client] = true
		m.mu.Unlock()
		resp["result"] = map[string]any{"tools": []any{map[string]any{
			"name": "smoke_ping", "description": "Answers pong.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		}}}
	case "tools/call":
		resp["result"] = map[string]any{"content": []any{map[string]string{"type": "text", "text": "pong"}}}
	case "ping":
		resp["result"] = map[string]any{}
	default:
		resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func randomHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
