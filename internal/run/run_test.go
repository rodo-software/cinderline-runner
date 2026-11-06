package run

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderstack-runner/internal/cinderstack"
	"github.com/rodo-software/cinderstack-runner/internal/config"
	"github.com/rodo-software/cinderstack-runner/internal/secrets"
)

// fakeAPI answers "METHOD path" with routes[...] as {status, body}; a missing
// route is a 404. It records every request with its JSON body.
type fakeAPI struct {
	routes map[string]reply
	mu     sync.Mutex
	calls  []string
}

type reply struct {
	status int
	body   string
}

func (f *fakeAPI) client(t *testing.T) *cinderstack.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/api/runner/v1")
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		in, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, strings.TrimSpace(key+" "+string(in)))
		f.mu.Unlock()
		rep, ok := f.routes[key]
		if !ok {
			rep = reply{404, `{"message":"Not found."}`}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(srv.Close)
	c := cinderstack.New(srv.URL, "tok", "0.0.0")
	c.HTTP = srv.Client()
	c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return c
}

func (f *fakeAPI) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

var cfg = config.Project{URL: "https://cs.example.com", Key: "CSR", Repository: "https://git.example/o/r"}

func project(t *testing.T) Project {
	t.Helper()
	keyring.MockInit()
	state := t.TempDir()
	return Project{Config: cfg, Secrets: secrets.New(state), State: state, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func issueJSON(ref, status string) string {
	return `{"ref":"` + ref + `","title":"T","status":{"value":"` + status + `","label":"x"},"team":{"id":1,"name":"Dev"}}`
}

// Ported from legacy runner.test.mjs "a restart puts team issues left In
// progress back in To do". Its team-less issue is the API's job here: the
// runner API lists only issues with a team. An issue someone moved meanwhile
// (409) is left alone.
func TestRecoverPutsInProgressBackInTodo(t *testing.T) {
	p := project(t)
	api := &fakeAPI{routes: map[string]reply{
		"GET /issues?status=in_progress": {200, `{"issues":[` + issueJSON("CSR-1", "in_progress") + `,` + issueJSON("CSR-2", "in_progress") + `]}`},
		"PATCH /issues/CSR-1":            {200, issueJSON("CSR-1", "todo")},
		"PATCH /issues/CSR-2":            {409, `{"message":"Status changed.","status":"review"}`},
	}}
	if err := p.Recover(context.Background(), api.client(t)); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`PATCH /issues/CSR-1 {"status":"todo","expected_status":"in_progress"}`,
		`PATCH /issues/CSR-2 {"status":"todo","expected_status":"in_progress"}`,
	}
	if got := api.writes(); !slices.Equal(got, want) {
		t.Fatalf("writes = %q, want %q", got, want)
	}
}

func TestRecoverFailsWhenTheListFails(t *testing.T) {
	p := project(t)
	api := &fakeAPI{routes: map[string]reply{"GET /issues?status=in_progress": {500, `{"message":"Server error."}`}}}
	if err := p.Recover(context.Background(), api.client(t)); err == nil {
		t.Fatal("Recover succeeded without the list of In progress issues")
	}
}

// Ported from legacy runner.test.mjs "at startup a paused issue a person has
// closed loses its note", with the statuses legacy keeps a note for (blocked,
// todo, in_progress) and a note it cannot check, which stays.
func TestRecoverDropsNotesOfClosedIssues(t *testing.T) {
	p := project(t)
	dir := PausedDir(p.State, p.Config)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	routes := map[string]reply{"GET /issues?status=in_progress": {200, `{"issues":[]}`}}
	for ref, status := range map[string]string{
		"CSR-1": "done", "CSR-2": "blocked", "CSR-3": "todo", "CSR-4": "in_progress", "CSR-5": "review", "CSR-6": "",
	} {
		if err := os.WriteFile(filepath.Join(dir, ref+".json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if status != "" {
			routes["GET /issues/"+ref] = reply{200, issueJSON(ref, status)}
		}
	}
	api := &fakeAPI{routes: routes}
	if err := p.Recover(context.Background(), api.client(t)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, e := range entries {
		kept = append(kept, e.Name())
	}
	if want := []string{"CSR-2.json", "CSR-3.json", "CSR-4.json", "CSR-6.json"}; !slices.Equal(kept, want) {
		t.Fatalf("kept %q, want %q", kept, want)
	}
}

func TestRecoverWithoutNotes(t *testing.T) {
	p := project(t)
	api := &fakeAPI{routes: map[string]reply{"GET /issues?status=in_progress": {200, `{"issues":[]}`}}}
	if err := p.Recover(context.Background(), api.client(t)); err != nil {
		t.Fatal(err)
	}
}

func TestPausedDirIsPerProject(t *testing.T) {
	a := PausedDir("/s", config.Project{URL: "https://a.example.com", Key: "WEB"})
	b := PausedDir("/s", config.Project{URL: "https://b.example.com", Key: "WEB"})
	if a == b || filepath.Dir(filepath.Dir(a)) != filepath.Clean("/s") {
		t.Fatalf("PausedDir = %q and %q, want two folders under /s/paused", a, b)
	}
}

// Ported from legacy runner.test.mjs "<x> is skipped before the claim", the
// repository cases, and lib.test.mjs "assertRepository accepts only the
// configured repository".
func TestPin(t *testing.T) {
	for _, tt := range []struct {
		name string
		rep  reply
		err  string
	}{
		{"the configured repository", reply{200, `{"key":"CSR","repository_url":"git@git.example:o/r.git"}`}, ""},
		{"a repository other than the configured one", reply{200, `{"key":"CSR","repository_url":"https://evil.example/o/r.git"}`}, "evil.example"},
		{"a project without a repository", reply{200, `{"key":"CSR","repository_url":""}`}, "no repository"},
		{"no project", reply{500, `{"message":"Server error."}`}, "500"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := project(t)
			api := &fakeAPI{routes: map[string]reply{"GET /project": tt.rep}}
			err := p.Pin(context.Background(), api.client(t))
			if tt.err == "" && err != nil || tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)) {
				t.Fatalf("Pin = %v, want an error with %q", err, tt.err)
			}
		})
	}
}

// Ported from legacy lib.test.mjs "assertRunnable refuses Codex, and OpenCode
// without a key" and runner.test.mjs "<x> is skipped before the claim", the
// team cases, plus "an issue that fails before its claim does not block the
// next" (a team that cannot be read). A Claude agent needs this project's
// Anthropic credential, as an OpenCode agent needs its OpenRouter key.
func TestCheck(t *testing.T) {
	agent := func(name, client string) map[string]any {
		return map[string]any{"name": name, "role": "orchestrator", "client": client, "model": "m", "instructions": nil}
	}
	team := func(agents ...map[string]any) reply {
		b, _ := json.Marshal(map[string]any{"id": 1, "name": "Dev", "max_minutes": 1, "trusted": false, "agents": agents})
		return reply{200, string(b)}
	}
	for _, tt := range []struct {
		name string
		team reply
		keys []secrets.Kind
		err  string
	}{
		{"claude and opencode with both keys", team(agent("Boss", "claude"), agent("Qwen", "opencode")), []secrets.Kind{secrets.AnthropicCredential, secrets.OpenRouterKey}, ""},
		{"an OpenCode worker without an OpenRouter key", team(agent("Boss", "claude"), agent("Qwen", "opencode")), []secrets.Kind{secrets.AnthropicCredential}, "Qwen runs on opencode, which needs this project's openrouter"},
		{"an orchestrator on another client", team(agent("C", "codex")), nil, "C runs on codex; this runner supports only claude and opencode"},
		{"a Claude agent without an Anthropic credential", team(agent("Boss", "claude")), nil, "Boss runs on claude, which needs this project's anthropic"},
		{"a team that cannot be read", reply{404, `{"message":"Unknown team."}`}, nil, "Unknown team."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := project(t)
			for _, k := range tt.keys {
				if _, err := p.Secrets.Set(cfg, k, "s3cr3t-value"); err != nil {
					t.Fatal(err)
				}
			}
			api := &fakeAPI{routes: map[string]reply{"GET /teams/1": tt.team}}
			issue := cinderstack.Issue{Ref: "CSR-1", Team: &cinderstack.IssueTeam{ID: 1, Name: "Dev"}}
			err := p.Check(context.Background(), api.client(t), issue)
			if tt.err == "" && err != nil || tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)) {
				t.Fatalf("Check = %v, want an error with %q", err, tt.err)
			}
			if err != nil && strings.Contains(err.Error(), "s3cr3t-value") {
				t.Errorf("error %q carries a secret", err)
			}
		})
	}
}
