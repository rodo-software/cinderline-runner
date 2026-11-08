package run

// Acceptance tests for CSR-79: NewProject builds a configured project's
// dependencies, and the seven hooks run an issue end to end through the
// scheduler against the fakes.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/scheduler"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
	"github.com/zalando/go-keyring"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func hooksSet(h scheduler.Hooks) []string {
	var missing []string
	for name, set := range map[string]bool{
		"Recover": h.Recover != nil, "Pin": h.Pin != nil, "Check": h.Check != nil, "Run": h.Run != nil,
		"Publish": h.Publish != nil, "ReviewBranches": h.ReviewBranches != nil, "MergeCheck": h.MergeCheck != nil,
	} {
		if !set {
			missing = append(missing, name)
		}
	}
	return missing
}

// A client is built only when its model credential is in the keychain
// (decision 0122); all seven hooks are set either way.
func TestNewProjectBuildsClientsForKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []secrets.Kind
		want []string
	}{
		{"both keys", []secrets.Kind{secrets.AnthropicCredential, secrets.OpenRouterKey}, []string{"claude", "opencode"}},
		{"anthropic only", []secrets.Kind{secrets.AnthropicCredential}, []string{"claude"}},
		{"openrouter only", []secrets.Kind{secrets.OpenRouterKey}, []string{"opencode"}},
		{"openai only", []secrets.Kind{secrets.OpenAIKey}, []string{"codex"}},
		{"all three keys", []secrets.Kind{secrets.AnthropicCredential, secrets.OpenAIKey, secrets.OpenRouterKey}, []string{"claude", "codex", "opencode"}},
		{"gemini only", []secrets.Kind{secrets.GeminiKey}, []string{"gemini"}},
		{"all four keys", []secrets.Kind{secrets.AnthropicCredential, secrets.OpenAIKey, secrets.GeminiKey, secrets.OpenRouterKey}, []string{"claude", "codex", "gemini", "opencode"}},
		{"no model key", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			state := t.TempDir()
			sec := secrets.New(state)
			for _, k := range append([]secrets.Kind{secrets.GitToken}, tc.keys...) {
				if _, err := sec.Set(cfg, k, "secret-"+string(k)); err != nil {
					t.Fatal(err)
				}
			}
			p, err := NewProject(cfg, sec, state, &environment.Docker{Project: cfg.Key, Log: discard()}, discard())
			if err != nil {
				t.Fatalf("NewProject: %v", err)
			}
			var got []string
			for name, cl := range p.Clients {
				if cl == nil {
					t.Errorf("client %s is nil", name)
				}
				got = append(got, name)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("clients = %v, want %v", got, tc.want)
			}
			if p.Workspace == nil || p.Forge == nil {
				t.Errorf("Workspace %v, Forge set %v; want both", p.Workspace, p.Forge != nil)
			}
			if missing := hooksSet(p.Hooks()); len(missing) > 0 {
				t.Errorf("nil hooks: %v", missing)
			}
		})
	}
}

// The Codex client gets the project's OpenAI key, Cinderline's MCP endpoint
// and the project's optional OpenAI-compatible base URL from the local config.
func TestNewProjectCodexFromConfig(t *testing.T) {
	keyring.MockInit()
	state := t.TempDir()
	sec := secrets.New(state)
	pc := cfg
	pc.OpenAIBaseURL = "http://host.docker.internal:11434/v1"
	for _, k := range []secrets.Kind{secrets.GitToken, secrets.OpenAIKey} {
		if _, err := sec.Set(pc, k, "secret-"+string(k)); err != nil {
			t.Fatal(err)
		}
	}
	p, err := NewProject(pc, sec, state, &environment.Docker{Project: pc.Key, Log: discard()}, discard())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	c, ok := p.Clients["codex"].(*client.Codex)
	if !ok {
		t.Fatalf("codex client = %T", p.Clients["codex"])
	}
	if c.Key != "secret-openai" || c.BaseURL != pc.OpenAIBaseURL || c.Project != pc.Key || !strings.HasSuffix(c.MCPURL, "/mcp") {
		t.Errorf("codex client = %+v", c)
	}
}

// The Gemini client gets the project's Gemini key, Cinderline's MCP endpoint
// and, from the local config, whether that key is a Vertex AI express-mode key.
func TestNewProjectGeminiFromConfig(t *testing.T) {
	keyring.MockInit()
	state := t.TempDir()
	sec := secrets.New(state)
	pc := cfg
	pc.GeminiVertex = true
	for _, k := range []secrets.Kind{secrets.GitToken, secrets.GeminiKey} {
		if _, err := sec.Set(pc, k, "secret-"+string(k)); err != nil {
			t.Fatal(err)
		}
	}
	p, err := NewProject(pc, sec, state, &environment.Docker{Project: pc.Key, Log: discard()}, discard())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	c, ok := p.Clients["gemini"].(*client.Gemini)
	if !ok {
		t.Fatalf("gemini client = %T", p.Clients["gemini"])
	}
	if c.Key != "secret-gemini" || !c.Vertex || c.Project != pc.Key || !strings.HasSuffix(c.MCPURL, "/mcp") {
		t.Errorf("gemini client = %+v", c)
	}
}

// Without the git token the project cannot fetch or push: NewProject says so
// instead of starting a slot that fails at its first run.
func TestNewProjectNeedsGitToken(t *testing.T) {
	keyring.MockInit()
	state := t.TempDir()
	sec := secrets.New(state)
	if _, err := sec.Set(cfg, secrets.AnthropicCredential, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewProject(cfg, sec, state, &environment.Docker{Project: cfg.Key, Log: discard()}, discard()); err == nil {
		t.Fatal("NewProject without a git token: no error")
	}
}

// The forge is the one GET project names, on the configured repository.
func TestNewProjectForgeByKind(t *testing.T) {
	for _, tc := range []struct {
		kind, repository string
		want             any
	}{
		{"github", "https://github.com/o/r", &forge.GitHub{}},
		{"gitlab", "https://gitlab.com/o/r", &forge.GitLab{}},
		{"forgejo", "https://git.example/o/r", &forge.Forgejo{}},
		{"bitbucket", "https://bitbucket.org/o/r", &forge.Bitbucket{}}, // Bitbucket Cloud only (decision 2026-11-04-bitbucket-cloud)
	} {
		keyring.MockInit()
		state := t.TempDir()
		sec := secrets.New(state)
		pc := cfg
		pc.Repository = tc.repository
		if _, err := sec.Set(pc, secrets.GitToken, "git"); err != nil {
			t.Fatal(err)
		}
		p, err := NewProject(pc, sec, state, &environment.Docker{Project: pc.Key, Log: discard()}, discard())
		if err != nil {
			t.Fatal(err)
		}
		fg, err := p.Forge(tc.kind)
		if err != nil {
			t.Errorf("Forge(%q): %v", tc.kind, err)
			continue
		}
		if got, w := typeName(fg), typeName(tc.want); got != w {
			t.Errorf("Forge(%q) = %s, want %s", tc.kind, got, w)
		}
		if tc.kind == "forgejo" {
			if _, err := p.Forge("svn"); err == nil {
				t.Error("Forge(\"svn\"): no error")
			}
		}
	}
}

func typeName(v any) string { return fmt.Sprintf("%T", v) }

// End to end against the fakes, driven only through the scheduler's hooks: a
// To do issue of a solo team is claimed, run, published as a pull request
// and reaches Review. The team's max_turns reaches the turn; without it the
// turn gets 50 (decision 2026-11-07-team-max-turns).
func TestHooksRunIssueToReview(t *testing.T) {
	for _, tc := range []struct {
		name     string
		maxTurns string
		want     int
	}{
		{"team max_turns", `,"max_turns":12`, 12},
		{"no max_turns falls back to 50", ``, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ref := "CSR-7"
			team := `{"id":1,"name":"Dev","max_minutes":30,"trusted":false,"max_sub_issues":5,"ponytail":false` + tc.maxTurns +
				`,"agents":[{"name":"Coder","role":"coder","client":"claude","model":null,"instructions":null,"description":null,"price":null}]}`
			issue := func(status string) string {
				return `{"ref":"` + ref + `","title":"Add hello","body":"Add a file.","status":{"value":"` + status + `","label":"x"},"team":{"id":1,"name":"Dev"},"sub_issues":[]}`
			}

			var mu sync.Mutex
			status := "todo"
			cur := func() string { mu.Lock(); defer mu.Unlock(); return status }
			api := &fakeAPI{handle: func(key, body string) (reply, bool) {
				switch {
				case strings.HasPrefix(key, "GET /work"):
					if cur() != "todo" {
						return reply{200, `{"issue":null}`}, true
					}
					return reply{200, `{"issue":` + issue("todo") + `}`}, true
				case strings.HasPrefix(key, "GET /issues?"):
					return reply{200, `{"issues":[]}`}, true
				case key == "GET /project":
					return reply{200, `{"key":"CSR","name":"CSR","repository_url":"` + cfg.Repository + `","forge":"forgejo","base_branch":"main","environment":"default"}`}, true
				case key == "GET /teams/1":
					return reply{200, team}, true
				case key == "POST /issues/"+ref+"/claim":
					mu.Lock()
					status = "in_progress"
					mu.Unlock()
					return reply{200, issue("in_progress")}, true
				case key == "GET /issues/"+ref:
					return reply{200, issue(cur())}, true
				case key == "PATCH /issues/"+ref:
					if strings.Contains(body, `"status":"review"`) {
						mu.Lock()
						status = "review"
						mu.Unlock()
					}
					return reply{200, issue(cur())}, true
				case key == "GET /issues/"+ref+"/comments":
					return reply{200, `{"comments":[]}`}, true
				case key == "POST /issues/"+ref+"/comments":
					return reply{201, `{"id":1,"body":"x","author":{"name":"runner"}}`}, true
				case key == "POST /issues/"+ref+"/progress":
					return reply{204, ""}, true
				case key == "POST /runs/"+ref+"/agent-token":
					return reply{201, `{"token":"scoped","expires_at":"2030-01-01T00:00:00Z"}`}, true
				case key == "DELETE /runs/"+ref+"/agent-token":
					return reply{204, ""}, true
				}
				return reply{}, false
			}}
			c := api.client(t)

			p := project(t)
			if _, err := p.Secrets.Set(cfg, secrets.AnthropicCredential, "sk-ant"); err != nil {
				t.Fatal(err)
			}
			cl := &client.Fake{Runs: []client.RunReply{{Result: client.Result{SessionID: "s1", Report: "Added hello."}}}}
			ws := &workspace.Fake{
				Prepares:   []workspace.PrepareReply{{Dir: "/clone"}},
				Commits:    []workspace.CommitReply{{Committed: true}, {Committed: true}, {Committed: true}},
				MergeBases: []workspace.MergeBaseReply{{}, {}, {}},
				Ancestors:  []workspace.IsAncestorReply{{Yes: false}, {Yes: false}, {Yes: false}},
				Pushes:     []error{nil, nil},
			}
			fg := &forge.Fake{PullRequests: []forge.PullRequestReply{{PullRequest: forge.PullRequest{Number: 3, URL: "https://git.example/o/r/pulls/3"}}}}
			p.Clients = map[string]client.Client{"claude": cl}
			p.Workspace = ws
			p.Forge = func(kind string) (forge.Forge, error) {
				if kind != "forgejo" {
					t.Errorf("Forge(%q), want the project's forgejo", kind)
				}
				return fg, nil
			}

			done := make(chan struct{})
			go func() {
				(&scheduler.Scheduler{Log: discard(), ReviewInterval: time.Hour, MergeInterval: time.Hour}).Run(ctx, []scheduler.Slot{{Project: cfg.Key, Client: c, Hooks: p.Hooks()}})
				close(done)
			}()
			for cur() != "review" && ctx.Err() == nil {
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
			<-done
			if s := cur(); s != "review" {
				t.Fatalf("status = %q, want review; requests:\n%s", s, strings.Join(api.writes(), "\n"))
			}

			runs := cl.Calls()
			if len(runs) != 1 {
				t.Fatalf("client runs = %d, want 1", len(runs))
			}
			turn := runs[0].Args[0].(client.Turn)
			if turn.Dir != "/clone" || turn.MaxTurns != tc.want || turn.MaxMinutes != 30 || turn.Token != "scoped" {
				t.Errorf("turn = %+v; want Dir /clone, MaxTurns %d, MaxMinutes 30, Token scoped", turn, tc.want)
			}
			if !strings.Contains(turn.Prompt, ref) {
				t.Errorf("prompt lacks %s: %q", ref, turn.Prompt)
			}
			var opened bool
			for _, call := range fg.Calls() {
				opened = opened || call.Method == "OpenPullRequest"
			}
			if !opened {
				t.Errorf("no pull request opened; forge calls %+v", fg.Calls())
			}
			var reported bool
			for _, w := range api.writes() {
				reported = reported || (strings.HasPrefix(w, "POST /issues/"+ref+"/comments") && strings.Contains(w, "pulls/3"))
			}
			if !reported {
				t.Errorf("no finish report naming the pull request; writes:\n%s", strings.Join(api.writes(), "\n"))
			}
		})
	}
}
