package run

import (
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// The generic-run cases of legacy runner.test.mjs (CSR-75), driven through
// the Run and Publish hooks against the fakes.

// genericTeam is a generic team whose old data still lists a worker: it
// runs its orchestrator alone (legacy work()).
const genericTeam = `{"id":1,"name":"Ops","max_minutes":30,"trusted":false,"max_sub_issues":5,"ponytail":false,"generic_runs":true,"agents":[` +
	`{"name":"Clerk","role":"orchestrator","client":"claude","model":null,"instructions":"Tidy.","description":null,"price":null},` +
	`{"name":"Qwen","role":"worker","client":"opencode","model":null,"instructions":null,"description":null,"price":null}]}`

// genericRun runs CSR-7 of team (JSON) through the Run and Publish hooks, as
// the scheduler does after the claim, and returns the API's writes.
func genericRun(t *testing.T, p Project, team string, cl *client.Fake, ws *workspace.Fake, fg *forge.Fake) *fakeAPI {
	t.Helper()
	ref := "CSR-7"
	var mu sync.Mutex
	status := "in_progress"
	issue := func() string {
		mu.Lock()
		defer mu.Unlock()
		return `{"ref":"` + ref + `","title":"T","body":"B","status":{"value":"` + status + `","label":"x"},"team":{"id":1,"name":"Ops"},"sub_issues":[]}`
	}
	api := &fakeAPI{handle: func(key, body string) (reply, bool) {
		switch key {
		case "GET /project":
			return reply{200, `{"key":"CSR","name":"CSR","repository_url":"` + cfg.Repository + `","forge":"forgejo","base_branch":"main","environment":"default","check_command":"make check"}`}, true
		case "GET /teams/1":
			return reply{200, team}, true
		case "GET /issues/" + ref:
			return reply{200, issue()}, true
		case "PATCH /issues/" + ref:
			if strings.Contains(body, `"status":"review"`) {
				mu.Lock()
				status = "review"
				mu.Unlock()
			}
			return reply{200, issue()}, true
		case "GET /issues/" + ref + "/comments":
			return reply{200, `{"comments":[]}`}, true
		case "POST /issues/" + ref + "/comments":
			return reply{201, `{"id":1,"body":"x","author":{"name":"runner"}}`}, true
		case "POST /issues/" + ref + "/progress", "DELETE /runs/" + ref + "/agent-token", "POST /runs/" + ref + "/usage":
			return reply{204, ""}, true
		case "POST /runs/" + ref + "/agent-token":
			return reply{201, `{"token":"scoped","expires_at":"2030-01-01T00:00:00Z"}`}, true
		}
		if strings.HasPrefix(key, "GET /issues?") {
			return reply{200, `{"issues":[]}`}, true
		}
		return reply{}, false
	}}
	c := api.client(t)
	if _, err := p.Secrets.Set(cfg, secrets.AnthropicCredential, "sk-ant"); err != nil {
		t.Fatal(err)
	}
	p.Clients = map[string]client.Client{"claude": cl}
	p.Workspace = ws
	p.Forge = func(string) (forge.Forge, error) { return fg, nil }
	h := p.Hooks()
	in := cinderline.Issue{Ref: ref, Team: &cinderline.IssueTeam{ID: 1}}
	if err := h.Run(context.Background(), c, in); err != nil {
		t.Fatalf("Run: %v; writes:\n%s", err, strings.Join(api.writes(), "\n"))
	}
	if err := h.Publish(context.Background(), c, in); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return api
}

// commentBodies are the bodies of the comments the runner added to CSR-7.
func commentBodies(api *fakeAPI) []string {
	var out []string
	for _, w := range api.writes() {
		if b, ok := strings.CutPrefix(w, "POST /issues/CSR-7/comments "); ok {
			out = append(out, b)
		}
	}
	return out
}

// Ported from runner.test.mjs "a generic run uses genericPrompt, ends with
// GENERIC_REPORT and moves to review", with the generic half of
// lib.prompt.test.mjs "runKind returns the kind from the team flags": the
// team's generic_runs picks the generic prompt. No commit, push, base merge
// or forge call; the orchestrator runs alone although the team lists a worker.
func TestGenericRunEndsWithReportInReview(t *testing.T) {
	cl := &client.Fake{Runs: []client.RunReply{{Result: client.Result{SessionID: "s1"}}}}
	ws := &workspace.Fake{Prepares: []workspace.PrepareReply{{Dir: "/clone"}}}
	fg := &forge.Fake{}
	p := project(t)
	api := genericRun(t, p, genericTeam, cl, ws, fg)

	runs := cl.Calls()
	if len(runs) != 1 {
		t.Fatalf("client runs = %d, want 1", len(runs))
	}
	if got, want := runs[0].Args[0].(client.Turn).Prompt, GenericPrompt("Tidy.", "CSR-7", "T", "B", "CSR"); got != want {
		t.Errorf("prompt =\n%s\nwant GenericPrompt:\n%s", got, want)
	}
	if got := commentBodies(api); len(got) == 0 || got[len(got)-1] != `{"body":"`+GenericReport+`"}` {
		t.Errorf("comments = %q, want the last to be %q", got, GenericReport)
	}
	var moves []string
	for _, w := range api.writes() {
		if strings.HasPrefix(w, "PATCH /issues/CSR-7 ") {
			moves = append(moves, w)
		}
	}
	if want := []string{`PATCH /issues/CSR-7 {"status":"review","expected_status":"in_progress"}`}; !slices.Equal(moves, want) {
		t.Errorf("moves = %q, want %q", moves, want)
	}
	for _, call := range ws.Calls() {
		if call.Method != "Prepare" {
			t.Errorf("workspace call %s, want only Prepare", call.Method)
		}
	}
	if calls := fg.Calls(); len(calls) != 0 {
		t.Errorf("forge calls = %+v, want none", calls)
	}
	// The project has a check command; a generic run changes no files, so it gets no check (CSR-80).
	if n := len(envRuns(p.Env.(*environment.Fake))); n != 0 {
		t.Errorf("check runs = %d, want none", n)
	}
	// The session is kept for a follow-up as a generic run's.
	if n, ok := p.readPaused("CSR-7"); !ok || !n.Generic || n.Session != "s1" {
		t.Errorf("kept note = %+v, want Generic", n)
	}
}

// Ported from runner.test.mjs "a paused generic run resumes as generic, no
// team-changed comment": the generic note's session resumes in its clone.
func TestPausedGenericRunResumesAsGeneric(t *testing.T) {
	p := project(t)
	note := pausedNote(t, p, Paused{Session: "the-session", Client: "claude", Generic: true})
	cl := &client.Fake{Runs: []client.RunReply{{Result: client.Result{SessionID: "the-session"}}}}
	ws := &workspace.Fake{}
	api := genericRun(t, p, genericTeam, cl, ws, &forge.Fake{})

	runs := cl.Calls()
	if len(runs) != 1 {
		t.Fatalf("client runs = %d, want 1", len(runs))
	}
	turn := runs[0].Args[0].(client.Turn)
	if turn.SessionID != "the-session" || turn.Dir != note.Dir || turn.Prompt != ResumePrompt("CSR-7") {
		t.Errorf("turn = %+v; want the session the-session in %s with ResumePrompt", turn, note.Dir)
	}
	got := commentBodies(api)
	for _, b := range got {
		if strings.Contains(b, "The team changed while paused") {
			t.Errorf("team-changed comment %s", b)
		}
	}
	if len(got) == 0 || got[len(got)-1] != `{"body":"`+GenericReport+`"}` {
		t.Errorf("comments = %q, want the last to be %q", got, GenericReport)
	}
}

// Ported from the generic half of runner.test.mjs "a paused note with
// writer:true on a generic team is dropped" (its writer half is dropped by
// decision 2026-11-06-no-writer-teams): a note of the other run kind is
// dropped with a comment, and the run starts over. A coding note names its
// last commit, as legacy dropChangedKind.
func TestPausedNoteOfTheOtherKindIsDropped(t *testing.T) {
	for _, tc := range []struct {
		name        string
		noteGeneric bool
		teamGeneric bool
		want        string
	}{
		{"a coding note on a generic team", false, true, "The team changed while paused, so this run starts over. The paused coding run's last commit, abc123, stays in the runner's clone until git prunes it."},
		{"a generic note on a coding team", true, false, "The team changed while paused, so this run starts over."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := project(t)
			pausedNote(t, p, Paused{Session: "the-session", Client: "claude", Generic: tc.noteGeneric})
			api := &fakeAPI{routes: map[string]reply{"POST /issues/CSR-7/comments": {201, `{"id":1,"body":"x","author":{"name":"runner"}}`}}}
			ws := &workspace.Fake{Heads: []workspace.HeadReply{{Commit: "abc123"}}}
			if _, ok := p.TakePaused(context.Background(), api.client(t), ws, "CSR-7", "cinderline/CSR-7", "claude", tc.teamGeneric); ok {
				t.Fatal("TakePaused resumed a note of the other kind")
			}
			if want := []string{`POST /issues/CSR-7/comments {"body":"` + tc.want + `"}`}; !slices.Equal(api.writes(), want) {
				t.Errorf("writes = %q, want %q", api.writes(), want)
			}
			if _, err := os.Stat(noteFile(p)); !os.IsNotExist(err) {
				t.Errorf("note after the drop: %v, want removed", err)
			}
		})
	}
}

// A generic run over its budget is blocked and keeps its note, as a coding
// run, but commits and pushes nothing: it has nothing to publish.
func TestGenericBudgetStopPushesNothing(t *testing.T) {
	clone := t.TempDir()
	p := project(t)
	p.Hooks()
	if err := p.SavePaused("CSR-7", Paused{Dir: clone, Session: "the-session", Client: "claude", Generic: true}); err != nil {
		t.Fatal(err)
	}
	cl := &client.Fake{}
	ws := &workspace.Fake{}
	p.Clients, p.Workspace = map[string]client.Client{"claude": cl}, ws
	p.Forge = func(string) (forge.Forge, error) { return &forge.Fake{}, nil }
	over := strings.TrimSuffix(issueJSON("CSR-7", "in_progress"), "}") + `,"budget_usd":5,"over_budget":true,"usage":{"cost_usd":5.126}}`
	api := &fakeAPI{routes: map[string]reply{
		"GET /teams/1":                   {200, genericTeam},
		"GET /issues/CSR-7":              {200, over},
		"GET /project":                   {200, `{"key":"CSR","forge":"forgejo","base_branch":"main"}`},
		"GET /issues/CSR-7/comments":     {200, `{"comments":[]}`},
		"POST /issues/CSR-7/progress":    {204, ""},
		"POST /issues/CSR-7/comments":    {201, `{"id":9,"body":"x"}`},
		"POST /runs/CSR-7/agent-token":   {201, `{"token":"tok","expires_at":"2030-01-01T00:00:00Z"}`},
		"DELETE /runs/CSR-7/agent-token": {204, ""},
		"PATCH /issues/CSR-7":            {200, issueJSON("CSR-7", "blocked")},
	}}
	if err := p.hookRun(context.Background(), api.client(t), cinderline.Issue{Ref: "CSR-7", Team: &cinderline.IssueTeam{ID: 1}}); err != nil {
		t.Fatal(err)
	}
	if n := len(cl.Calls()); n != 0 {
		t.Errorf("agent runs = %d, want none", n)
	}
	for _, call := range ws.Calls() {
		if call.Method != "Exists" {
			t.Errorf("workspace call %s, want only Exists", call.Method)
		}
	}
	if !slices.Contains(api.writes(), `PATCH /issues/CSR-7 {"status":"blocked","expected_status":"in_progress"}`) {
		t.Errorf("not blocked; writes:\n%s", strings.Join(api.writes(), "\n"))
	}
	if n, ok := p.readPaused("CSR-7"); !ok || n.Session != "the-session" || !n.Generic {
		t.Errorf("note = %+v, %v; want the generic session kept", n, ok)
	}
}

// A finished generic run's kept session resumes for a follow-up without the
// fast-forward to the remote branch: a generic run never pushes one.
func TestKeptGenericSessionResumesWithoutFastForward(t *testing.T) {
	p := project(t)
	note := pausedNote(t, p, Paused{Session: "s1", Client: "claude", Generic: true, Kept: time.Now()})
	ws := &workspace.Fake{}
	got, ok := p.TakePaused(context.Background(), nil, ws, "CSR-7", "cinderline/CSR-7", "claude", true)
	if !ok || got.Session != "s1" || got.Dir != note.Dir {
		t.Fatalf("TakePaused = %+v, %v; want the kept generic session", got, ok)
	}
	for _, call := range ws.Calls() {
		if call.Method == "FastForward" {
			t.Errorf("FastForward called for a generic note")
		}
	}
}
