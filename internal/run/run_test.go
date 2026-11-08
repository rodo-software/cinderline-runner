package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// fakeAPI answers "METHOD path" with handle, when it is set and answers, or
// with queues[...] in order, the last reply repeating, or else with
// routes[...]; a missing route is a 404. It records every request with its
// JSON body.
type fakeAPI struct {
	handle func(key, body string) (reply, bool)
	routes map[string]reply
	queues map[string][]reply
	mu     sync.Mutex
	calls  []string
}

type reply struct {
	status int
	body   string
}

func (f *fakeAPI) client(t *testing.T) *cinderline.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/api/runner/v1")
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		in, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, strings.TrimSpace(key+" "+string(in)))
		rep, ok := f.routes[key]
		if q := f.queues[key]; len(q) > 0 {
			rep, ok = q[0], true
			if len(q) > 1 {
				f.queues[key] = q[1:]
			}
		}
		if f.handle != nil {
			if r, handled := f.handle(key, string(in)); handled {
				rep, ok = r, true
			}
		}
		f.mu.Unlock()
		if !ok {
			rep = reply{404, `{"message":"Not found."}`}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(srv.Close)
	c := cinderline.New(srv.URL, "tok", "0.0.0")
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
	return Project{Config: cfg, Secrets: secrets.New(state), State: state, Env: &environment.Fake{Cleanups: []error{nil}}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
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

// Review item 9: what a previous process left in Docker is removed first,
// so its networks do not use up Docker's address pools. A failure there
// only logs: it must not keep the project from working.
func TestRecoverCleansUpStaleResources(t *testing.T) {
	for _, cleanupErr := range []error{nil, errors.New("docker is not running")} {
		p := project(t)
		var logs bytes.Buffer
		p.Log = slog.New(slog.NewTextHandler(&logs, nil))
		env := &environment.Fake{Cleanups: []error{cleanupErr}}
		p.Env = env
		api := &fakeAPI{routes: map[string]reply{"GET /issues?status=in_progress": {200, `{"issues":[]}`}}}
		if err := p.Recover(context.Background(), api.client(t)); err != nil {
			t.Fatal(err)
		}
		if c := env.Calls(); len(c) != 1 || c[0].Method != "Cleanup" {
			t.Errorf("environment calls = %v, want Cleanup", c)
		}
		if cleanupErr != nil && !strings.Contains(logs.String(), "docker is not running") {
			t.Errorf("log lacks the cleanup failure:\n%s", logs.String())
		}
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
		{"a Codex agent with an OpenAI key", team(agent("C", "codex")), []secrets.Kind{secrets.OpenAIKey}, ""},
		{"a Codex agent without an OpenAI key", team(agent("C", "codex")), []secrets.Kind{secrets.AnthropicCredential, secrets.OpenRouterKey}, "C runs on codex, which needs this project's openai"},
		{"a Gemini agent with a Gemini key", team(agent("G", "gemini")), []secrets.Kind{secrets.GeminiKey}, ""},
		{"a Gemini agent without a Gemini key", team(agent("G", "gemini")), []secrets.Kind{secrets.AnthropicCredential, secrets.OpenAIKey}, "G runs on gemini, which needs this project's gemini"},
		{"an orchestrator on another client", team(agent("A", "aider")), nil, "A runs on aider; this runner supports only claude, codex, gemini and opencode"},
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
			issue := cinderline.Issue{Ref: "CSR-1", Team: &cinderline.IssueTeam{ID: 1, Name: "Dev"}}
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

// CSR-81 point 3: at start, a clone whose branch has commits the remote
// lacks is pushed, and if its issue is in Review its pull request is found
// or opened; one log line per branch. A clone of a closed issue, or one
// whose pull request is already merged or closed, is left alone; a refused
// push is logged and skipped, never forced. None of it writes to the issue.
func TestRecoverPushesStrandedBranches(t *testing.T) {
	p := project(t)
	var log bytes.Buffer
	p.Log = slog.New(slog.NewTextHandler(&log, nil))
	ws := &workspace.Fake{
		Strandeds: []workspace.StrandedReply{{Clones: []workspace.Clone{
			{Dir: "/runs/CSR-5", Branch: "cinderline/CSR-5"}, // Review, no pull request: push, open
			{Dir: "/runs/CSR-6", Branch: "cinderline/CSR-6"}, // Done: left alone
			{Dir: "/runs/CSR-7", Branch: "cinderline/CSR-7"}, // To do, merged: left alone
			{Dir: "/runs/CSR-8", Branch: "cinderline/CSR-8"}, // Review, push refused: no pull request
		}}},
		Pushes: []error{nil, errors.New("rejected: non-fast-forward")},
	}
	fg := &forge.Fake{
		MergeStates:  []forge.MergeStateReply{{State: ""}, {State: forge.MergeMerged}, {State: ""}},
		PullRequests: []forge.PullRequestReply{{PullRequest: forge.PullRequest{Number: 53, URL: "https://git.example/o/r/pulls/53"}}},
	}
	p.Workspace = ws
	p.Forge = func(string) (forge.Forge, error) { return fg, nil }
	api := &fakeAPI{routes: map[string]reply{
		"GET /project":                   {200, `{"key":"CSR","forge":"forgejo","base_branch":"main","repository_url":"https://git.example/o/r"}`},
		"GET /issues?status=in_progress": {200, `{"issues":[]}`},
		"GET /issues/CSR-5":              {200, issueJSON("CSR-5", "review")},
		"GET /issues/CSR-6":              {200, issueJSON("CSR-6", "done")},
		"GET /issues/CSR-7":              {200, issueJSON("CSR-7", "todo")},
		"GET /issues/CSR-8":              {200, issueJSON("CSR-8", "review")},
	}}
	if err := p.Recover(context.Background(), api.client(t)); err != nil {
		t.Fatal(err)
	}
	wantWS := []workspace.Call{
		{Method: "Stranded"},
		{Method: "Push", Args: []any{"/runs/CSR-5", "cinderline/CSR-5"}},
		{Method: "Push", Args: []any{"/runs/CSR-8", "cinderline/CSR-8"}},
	}
	if got := ws.Calls(); !reflect.DeepEqual(got, wantWS) {
		t.Errorf("workspace calls =\n%+v, want\n%+v", got, wantWS)
	}
	wantForge := []forge.Call{
		{Method: "MergeState", Args: []any{"cinderline/CSR-5"}},
		{Method: "OpenPullRequest", Args: []any{forge.PullRequestInput{Branch: "cinderline/CSR-5", Base: "main", Title: "CSR-5: T", Body: "Resolves CSR-5 in Cinderline."}}},
		{Method: "MergeState", Args: []any{"cinderline/CSR-7"}},
		{Method: "MergeState", Args: []any{"cinderline/CSR-8"}},
	}
	if got := fg.Calls(); !reflect.DeepEqual(got, wantForge) {
		t.Errorf("forge calls =\n%+v, want\n%+v", got, wantForge)
	}
	if w := api.writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
	for _, want := range []string{"issue=CSR-5", "pulls/53", "issue=CSR-7", "issue=CSR-8", "non-fast-forward"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, log.String())
		}
	}
}

// CSR-81 point 3: a stranded branch the runner cannot push, because the
// clones cannot be listed, only logs; recovery goes on.
func TestRecoverStrandedListingFailureOnlyLogs(t *testing.T) {
	p := project(t)
	var log bytes.Buffer
	p.Log = slog.New(slog.NewTextHandler(&log, nil))
	p.Workspace = &workspace.Fake{Strandeds: []workspace.StrandedReply{{Err: errors.New("docker down")}}}
	p.Forge = func(string) (forge.Forge, error) { return &forge.Fake{}, nil }
	api := &fakeAPI{routes: map[string]reply{
		"GET /project":                   {200, `{"key":"CSR","forge":"forgejo","base_branch":"main"}`},
		"GET /issues?status=in_progress": {200, `{"issues":[]}`},
	}}
	if err := p.Recover(context.Background(), api.client(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "docker down") {
		t.Errorf("log = %s", log.String())
	}
}
