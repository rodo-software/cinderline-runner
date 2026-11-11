package run

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

func cost(usd float64) *client.Usage {
	return &client.Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 40, CostUSD: &usd}
}

// usageReports returns the bodies of the usage calls on ref.
func usageReports(api *fakeAPI, ref string) []string {
	var out []string
	for _, w := range api.writes() {
		if body, ok := strings.CutPrefix(w, "POST /runs/"+ref+"/usage "); ok {
			out = append(out, body)
		}
	}
	return out
}

// Ported from legacy runner.test.mjs "an issue over its budget runs no
// agent, is Blocked and keeps its note", through the Run hook. Before the
// move to Blocked the clone is committed and pushed, so no work is lost
// (Willem on CSR-76, EMBER-143).
func TestAnIssueOverItsBudgetRunsNoAgentIsBlockedAndKeepsItsNote(t *testing.T) {
	clone := t.TempDir()
	p := project(t)
	p.Hooks()
	if err := p.SavePaused("CSR-7", Paused{Dir: clone, Session: "the-session", Client: "claude"}); err != nil {
		t.Fatal(err)
	}
	cl := &client.Fake{}
	ws := &workspace.Fake{Commits: []workspace.CommitReply{{}, {Committed: true}}, MergeBases: []workspace.MergeBaseReply{{}}, Pushes: []error{nil}}
	p.Clients, p.Workspace = map[string]client.Client{"claude": cl}, ws
	p.Forge = func(string) (forge.Forge, error) { return &forge.Fake{}, nil }
	over := strings.TrimSuffix(issueJSON("CSR-7", "in_progress"), "}") + `,"budget_usd":5,"over_budget":true,"usage":{"cost_usd":5.126}}`
	api := &fakeAPI{routes: map[string]reply{
		"GET /teams/1":                   {200, `{"id":1,"name":"Dev","agents":[{"name":"Builder","role":"orchestrator","client":"claude","model":"m"}]}`},
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
	var steps []string
	for _, c := range ws.Calls() {
		steps = append(steps, c.Method+" "+fmt.Sprint(c.Args[1:]...))
	}
	want := []string{"Exists ", "Commit Dev / BuilderCSR-7: T", "MergeBase mainDev / Builder", "Commit Dev / BuilderCSR-7: T", "Push cinderline/CSR-7"}
	if !slices.Equal(steps, want) {
		t.Errorf("workspace = %q, want %q", steps, want)
	}
	var moves, comments []string
	for _, w := range api.writes() {
		if b, ok := strings.CutPrefix(w, "PATCH /issues/CSR-7 "); ok {
			moves = append(moves, b)
		}
		if b, ok := strings.CutPrefix(w, "POST /issues/CSR-7/comments "); ok {
			var in struct{ Body string }
			_ = json.Unmarshal([]byte(b), &in)
			comments = append(comments, in.Body)
		}
	}
	if !slices.Equal(moves, []string{`{"status":"blocked","expected_status":"in_progress"}`}) {
		t.Errorf("moves = %q", moves)
	}
	if !slices.Equal(comments, []string{"**Budget reached**: this issue has cost $5.13 of its $5.00 budget. Raise the budget on the issue and move it to To do to continue."}) {
		t.Errorf("comments = %q", comments)
	}
	if n, ok := p.readPaused("CSR-7"); !ok || n.Session != "the-session" || n.Dir != clone || !n.Kept.IsZero() {
		t.Errorf("note = %+v, %v; want the paused session kept", n, ok)
	}
	if got := usageReports(api, "CSR-7"); len(got) != 0 {
		t.Errorf("usage reports = %q, want none: nothing ran", got)
	}
}

// The usage of a run is reported only at its end, so the budget check adds
// the known cost of its earlier CLI runs: here the first run's $3 on top of
// the app's $2.50 reaches the $5 budget before the retry.
func TestTheBudgetCountsTheRunsUnreportedCost(t *testing.T) {
	p := project(t)
	p.Hooks()
	api := &fakeAPI{routes: map[string]reply{
		"GET /issues/CSR-7":           {200, strings.TrimSuffix(issueJSON("CSR-7", "in_progress"), "}") + `,"budget_usd":5,"over_budget":false,"usage":{"cost_usd":2.5}}`},
		"GET /issues/CSR-7/comments":  {200, `{"comments":[]}`},
		"POST /issues/CSR-7/progress": {204, ""},
		"POST /issues/CSR-7/comments": {201, `{"id":9,"body":"x"}`},
		"PATCH /issues/CSR-7":         {200, issueJSON("CSR-7", "blocked")},
	}}
	cl := &client.Fake{Runs: []client.RunReply{{Result: client.Result{SessionID: "s1", Usage: cost(3)}, Err: errFailed}, {}}}
	ws := &workspace.Fake{Commits: []workspace.CommitReply{{}}, Pushes: []error{nil}}
	j := job
	j.Agent, j.Workspace = "Builder", ws

	out, err := p.Attempts(context.Background(), api.client(t), cl, j)
	if err != nil || !out.Paused {
		t.Fatalf("Attempts = %+v, %v; want paused", out, err)
	}
	if n := len(cl.Calls()); n != 1 {
		t.Errorf("agent runs = %d, want 1", n)
	}
	if !strings.Contains(strings.Join(api.writes(), "\n"), "has cost $5.50 of its $5.00 budget") {
		t.Errorf("writes = %q", api.writes())
	}
	if n, _ := p.readPaused("CSR-7"); n.Dir != "/clone" {
		t.Errorf("note = %+v", n)
	}
}

// A budget stop between sub-issues: the next worker does not run and its
// sub-issue stays In progress for the resumed parent, the clone is pushed,
// the parent is Blocked and the orchestrator's session is kept (EMBER-112).
func TestABudgetStopBetweenSubIssues(t *testing.T) {
	p := project(t)
	p.Hooks()
	if err := p.SavePaused("CSR-1", Paused{Dir: t.TempDir(), Session: "the-session", Client: "claude"}); err != nil {
		t.Fatal(err)
	}
	tr := newTree("in_progress")
	tr.add("CSR-2", "First", "Qwen", "todo")
	tr.add("CSR-3", "Second", "Qwen", "todo")
	r := newTeamRun(tr)
	r.opencode.Runs = []client.RunReply{{Result: client.Result{Report: "Done.", Usage: cost(9)}}}
	r.opencode.during = func(client.Turn) { // the worker's $9, not reported yet, reaches the $5 budget
		tr.mu.Lock()
		tr.budget = 5
		tr.mu.Unlock()
	}

	out, err := r.run(t, context.Background(), p)
	if err != nil || !out.Paused {
		t.Fatalf("Rounds = %+v, %v; want paused", out, err)
	}
	if n := len(r.opencode.Calls()); n != 1 {
		t.Errorf("worker runs = %d, want 1", n)
	}
	if got := r.moves("CSR-3"); !slices.Equal(got, []string{"in_progress"}) {
		t.Errorf("CSR-3 moves = %v, want it left In progress", got)
	}
	if got := r.moves("CSR-1"); !slices.Equal(got, []string{"blocked"}) {
		t.Errorf("CSR-1 moves = %v, want [blocked]", got)
	}
	if pushes := r.wsCalls("Push"); len(pushes) != 2 {
		t.Errorf("pushes = %v, want after CSR-2 and at the budget stop", pushes)
	}
	if n, _ := p.readPaused("CSR-1"); n.Session != "the-session" || !n.Kept.IsZero() {
		t.Errorf("note = %+v, want the paused session", n)
	}
}

// The end of every run reports its usage in one call; a stopped run too.
func TestReportUsageSendsOneCallAndClears(t *testing.T) {
	p := project(t)
	p.Hooks()
	p.run.addUsage("CSR-1", "CSR-2", "Qwen", "", cost(0.25))
	p.run.addUsage("CSR-1", "CSR-1", "Builder", "m", &client.Usage{InputTokens: 1})
	p.run.addUsage("CSR-1", "CSR-1", "Builder", "m", nil) // a CLI that printed none
	p.run.addUsage("CSR-9", "CSR-9", "Other", "m", cost(1))
	api := &fakeAPI{routes: map[string]reply{"POST /runs/CSR-1/usage": {204, ""}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p.reportUsage(ctx, api.client(t), "CSR-1")
	p.reportUsage(ctx, api.client(t), "CSR-1")

	want := `{"entries":[` +
		`{"issue":"CSR-2","agent":"Qwen","model":null,"input_tokens":10,"output_tokens":20,"cache_read_tokens":30,"cache_write_tokens":40,"cost_usd":0.25},` +
		`{"issue":"CSR-1","agent":"Builder","model":"m","input_tokens":1,"output_tokens":0,"cache_read_tokens":0,"cache_write_tokens":0,"cost_usd":null}]}`
	if got := usageReports(api, "CSR-1"); !slices.Equal(got, []string{want}) {
		t.Errorf("usage reports = %q, want %q", got, want)
	}
	if got := p.run.pendingCost("CSR-9"); got != 1 {
		t.Errorf("another run's entries = %v, want kept", got)
	}
}

// A budget stop before the merge attempt at publish: no agent runs, the
// clone is pushed as it is, the issue is Blocked and the publish ends
// without a pull request or an error (EMBER-112).
func TestABudgetStopAtPublish(t *testing.T) {
	r := newPubRun(false)
	r.api.routes["GET /issues/CSR-1"] = reply{200, strings.TrimSuffix(issueJSON("CSR-1", "in_progress"), "}") + `,"budget_usd":1,"over_budget":true,"usage":{"cost_usd":1}}`}
	r.pub.Team = cinderline.Team{Name: "Solo", Agents: devTeam.Agents[:1]}
	r.pub.Session, r.pub.Token = "s1", "tok"
	r.ws.Commits = []workspace.CommitReply{{Committed: true}, {}}
	r.ws.MergeBases = []workspace.MergeBaseReply{{Conflicts: []string{"work.txt"}}}
	r.ws.Heads = []workspace.HeadReply{{Commit: "old"}}
	r.ws.Logs = []workspace.LogReply{{}}
	claude := &client.Fake{}
	p := project(t)

	if err := p.Publish(context.Background(), r.api.client(t), map[string]client.Client{"claude": claude}, r.ws, r.forge, r.pub); err != nil {
		t.Fatal(err)
	}
	if n := len(claude.Calls()); n != 0 {
		t.Errorf("agent runs = %d, want none", n)
	}
	if got := r.wsMethods(); !slices.Equal(got, []string{"Commit", "MergeBase", "Head", "Log", "Commit", "Push"}) {
		t.Errorf("workspace = %v", got)
	}
	if len(r.forge.Calls()) != 0 {
		t.Errorf("forge calls = %v, want none", r.forge.Calls())
	}
	if w := strings.Join(r.api.writes(), "\n"); !strings.Contains(w, `PATCH /issues/CSR-1 {"status":"blocked","expected_status":"in_progress"}`) || strings.Contains(w, `"status":"review"`) {
		t.Errorf("writes = %s", w)
	}
}
