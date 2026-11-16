package run

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// A team with workers follows the app's steps as a solo run does (CSR-179):
// the app's answers to nextStep are scripted, and the tests check what the
// runner does for each step and the facts it sends. Which sub-issue runs,
// the rounds, attempts, comments and budget, and every prompt text, are
// tested in the app (EMBER-214, EMBER-216). CSR-1 is the parent.

var devTeam = cinderline.Team{ID: 1, Name: "Dev", MaxMinutes: 45, Agents: []cinderline.TeamAgent{
	{Name: "Builder", Role: "orchestrator", Client: "claude", Model: ptr("m"), Effort: ptr("high"), Instructions: ptr("Lead.")},
	{Name: "Qwen", Role: "worker", Client: "opencode", Model: ptr("openrouter/qwen"), Instructions: ptr("Code.")},
}}

var resolverAgent = cinderline.TeamAgent{Name: "Merge Resolver", Role: "merge resolver", Client: "opencode", Model: ptr("openrouter/qwen"), Instructions: ptr("Resolve merges.")}

var errFailed = errors.New("exit status 1")

func turns(f *client.Fake) []client.Turn {
	var out []client.Turn
	for _, c := range f.Calls() {
		out = append(out, c.Args[0].(client.Turn))
	}
	return out
}

// agentCLI is a scripted client that can look around during its run.
type agentCLI struct {
	client.Fake
	during func(client.Turn)
}

func (a *agentCLI) Run(ctx context.Context, t client.Turn) (client.Result, error) {
	if a.during != nil {
		a.during(t)
	}
	return a.Fake.Run(ctx, t)
}

func ok(session, report string) client.RunReply {
	return client.RunReply{Result: client.Result{SessionID: session, Report: report}}
}

// newTeamStepRun is devTeam with its Merge Resolver on CSR-1, In progress,
// through the Run and Publish hooks; an agent token is "<kind>-<issue>".
func newTeamStepRun(t *testing.T, steps ...string) *stepRun {
	t.Helper()
	team, _ := json.Marshal(cinderline.Team{ID: 1, Name: "Dev", MaxMinutes: 45, Agents: append(slices.Clone(devTeam.Agents), resolverAgent)})
	api := &fakeAPI{
		handle: func(key, body string) (reply, bool) {
			method, path, _ := strings.Cut(key, " ")
			issue, ok := strings.CutSuffix(strings.TrimPrefix(path, "/runs/"), "/agent-token")
			switch {
			case !ok:
				return reply{}, false
			case method == "DELETE":
				return reply{204, ""}, true
			}
			var in struct{ Kind string }
			_ = json.Unmarshal([]byte(body), &in)
			return reply{201, `{"token":"` + in.Kind + `-` + issue + `","expires_at":"2030-01-01T00:00:00Z"}`}, true
		},
		routes: map[string]reply{
			"GET /project":                {200, `{"key":"CSR","name":"CSR","repository_url":"` + cfg.Repository + `","forge":"forgejo","base_branch":"main","environment":"default"}`},
			"GET /teams/1":                {200, string(team)},
			"GET /issues/CSR-1":           {200, `{"ref":"CSR-1","title":"Parent","body":"Do it.","status":{"value":"in_progress","label":"x"},"team":{"id":1,"name":"Dev"},"sub_issues":[]}`},
			"POST /issues/CSR-1/progress": {204, ""},
			"POST /issues/CSR-1/comments": {201, `{"id":1,"body":"x","author":{"name":"runner"}}`},
			"PATCH /issues/CSR-1":         {200, issueJSON("CSR-1", "in_progress")},
			"POST /runs/CSR-1/usage":      {204, ""},
		},
	}
	return newStepRunOf(t, "CSR-1", api, steps)
}

// teamStep is a run step of agent on issue, with a prompt that names both.
func teamStep(issue, title, agent, session, token string) string {
	return `{"kind":"run","agent":"` + agent + `","session":"` + session + `","token":"` + token + `","prompt":"Prompt for ` + agent + ` on ` + issue + `.","progress":"` + agent + ` works","issue":{"ref":"` + issue + `","title":"` + title + `"},"attachments":null}`
}

// resolveStep is the Merge Resolver's step on a conflict, in a new session
// without a token.
const resolveStep = `{"kind":"resolve","agent":"Merge Resolver","session":"new","token":"none","prompt":"Merge main.","progress":"Resolving the merge with main"}`

// fullTeamRun is a fresh team run: Builder plans, Qwen does CSR-1.1,
// Builder its own CSR-1.2 with a reader token, Builder reviews in its first
// session, and the run publishes.
func fullTeamRun(t *testing.T) *stepRun {
	t.Helper()
	r := newTeamStepRun(t,
		teamStep("CSR-1", "Parent", "Builder", "new", "orchestrator"),
		teamStep("CSR-1.1", "One", "Qwen", "new", "none"),
		teamStep("CSR-1.2", "Two", "Builder", "new", "reader"),
		teamStep("CSR-1", "Parent", "Builder", "resume", "orchestrator"),
		publishStep,
	)
	r.cl.Runs = []client.RunReply{
		{Result: client.Result{SessionID: "s1", Report: "Planned.", Usage: cost(1)}},
		ok("s2", "Two done."),
		ok("s1", "Reviewed."),
	}
	r.worker.Runs = []client.RunReply{{Result: client.Result{SessionID: "w1", Report: "One done.", Usage: cost(2)}}}
	return r
}

func offered(issue, agent, origin string) cinderline.ResumableSession {
	return cinderline.ResumableSession{Issue: issue, Agent: agent, Origin: origin}
}

// Each run step runs the named agent on the named issue, in a new session
// or the one the run holds for that agent and issue, with a token of the
// step's kind for that issue. A sub-issue's work is committed and pushed
// before the next request. Every request offers the lead's session and the
// last turn's when that is another's. The runner writes no sub-issue: the
// app claims, reports and moves them.
func TestATeamRunFollowsTheSteps(t *testing.T) {
	r := fullTeamRun(t)
	var notes []Paused
	r.worker.during = func(client.Turn) { n, _ := r.p.readPaused("CSR-1"); notes = append(notes, n) }
	var pushesAt []int
	answer := r.api.handle
	r.api.handle = func(key, body string) (reply, bool) {
		if key == "POST /runs/CSR-1/next" {
			pushesAt = append(pushesAt, strings.Count(strings.Join(r.wsMethods(), ","), "Push"))
		}
		return answer(key, body)
	}
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}

	builder := client.Turn{Dir: r.dir, MaxTurns: 50, MaxMinutes: 45, Idle: defaultIdle, Model: "m", Effort: "high", Author: "Dev / Builder"}
	plan, own, review := builder, builder, builder
	plan.Prompt, plan.Token = "Prompt for Builder on CSR-1.", "orchestrator-CSR-1"
	own.Prompt, own.Token = "Prompt for Builder on CSR-1.2.", "reader-CSR-1.2"
	review.Prompt, review.Token, review.SessionID = "Prompt for Builder on CSR-1.", "orchestrator-CSR-1", "s1"
	if got := turns(r.cl); !slices.Equal(got, []client.Turn{plan, own, review}) {
		t.Errorf("Builder's turns = %+v\nwant %+v", got, []client.Turn{plan, own, review})
	}
	worker := client.Turn{Dir: r.dir, Prompt: "Prompt for Qwen on CSR-1.1.", MaxTurns: 50, MaxMinutes: 45, Idle: defaultIdle, Model: "openrouter/qwen", Author: "Dev / Qwen"}
	if got := turns(&r.worker.Fake); !slices.Equal(got, []client.Turn{worker}) {
		t.Errorf("Qwen's turns = %+v\nwant %+v", got, []client.Turn{worker})
	}

	reqs := r.reqs(t)
	if len(reqs) != 5 {
		t.Fatalf("requests = %d, want 5", len(reqs))
	}
	lead := offered("CSR-1", "Builder", "turn")
	for i, want := range []struct {
		issue, agent, report string
		sessions             []cinderline.ResumableSession
	}{
		{"CSR-1", "Builder", "Planned.", []cinderline.ResumableSession{lead}},
		{"CSR-1.1", "Qwen", "One done.", []cinderline.ResumableSession{lead, offered("CSR-1.1", "Qwen", "turn")}},
		{"CSR-1.2", "Builder", "Two done.", []cinderline.ResumableSession{lead, offered("CSR-1.2", "Builder", "turn")}},
		{"CSR-1", "Builder", "Reviewed.", []cinderline.ResumableSession{lead}},
	} {
		req := reqs[i+1]
		if l := req.Last; l == nil || l.Step != "run" || l.Issue != want.issue || deref(l.Agent) != want.agent || l.Outcome != "finished" || deref(l.Report) != want.report {
			t.Errorf("request %d last = %+v, want %s's turn on %s", i+2, l, want.agent, want.issue)
		}
		if !slices.Equal(req.Sessions, want.sessions) {
			t.Errorf("request %d sessions = %+v, want %+v", i+2, req.Sessions, want.sessions)
		}
	}
	if u := reqs[2].Last.Usage; u == nil || u.Issue != "CSR-1.1" || u.Agent != "Qwen" || *u.CostUSD != 2 || deref(u.Model) != "openrouter/qwen" {
		t.Errorf("Qwen's usage = %+v, want it on CSR-1.1", u)
	}

	var commits []string
	for _, c := range r.ws.Calls() {
		if c.Method == "Commit" {
			commits = append(commits, c.Args[1].(string)+": "+c.Args[2].(string))
		}
	}
	if want := []string{"Dev / Qwen: CSR-1.1: One", "Dev / Builder: CSR-1.2: Two"}; len(commits) < 3 || !slices.Equal(commits[1:3], want) {
		t.Errorf("commits = %q, want the catch-up's, then %q", commits, want)
	}
	if want := []int{0, 0, 1, 2, 2}; !slices.Equal(pushesAt, want) {
		t.Errorf("pushes before each request = %v, want %v: each sub-issue pushed before the next request", pushesAt, want)
	}
	if want := (Paused{Dir: r.dir, Session: "s1", Client: "claude"}); len(notes) != 1 || notes[0] != want {
		t.Errorf("note during the worker = %+v, want %+v", notes, want)
	}
	if got := r.writesTo("/runs/CSR-1.2/agent-token"); !slices.Equal(got, []string{`POST {"agent":"Builder","kind":"reader"}`, "DELETE"}) {
		t.Errorf("CSR-1.2 token calls = %q", got)
	}
	if got := r.writesTo("/runs/CSR-1/agent-token"); !slices.Equal(got, []string{`POST {"agent":"Builder","kind":"orchestrator"}`, "DELETE"}) {
		t.Errorf("CSR-1 token calls = %q, want one orchestrator token for the run", got)
	}
	for _, c := range r.api.calls {
		if strings.Contains(c, "/issues/CSR-1.") {
			t.Errorf("the runner wrote or read a sub-issue: %s", c)
		}
	}
	if got := r.writesTo("/issues/CSR-1/progress"); !slices.Equal(got, []string{`POST {"body":"Started Dev / Builder"}`, `POST {"body":"Opening the pull request"}`}) {
		t.Errorf("progress = %q, want only the runner's own lines", got)
	}
	if got := r.writesTo("/issues/CSR-1"); !slices.Equal(got, []string{`PATCH {"status":"review","expected_status":"in_progress"}`}) {
		t.Errorf("moves = %q", got)
	}
	if n, _ := r.p.readPaused("CSR-1"); n.Session != "s1" || n.Kept.IsZero() {
		t.Errorf("note = %+v, want Builder's session kept", n)
	}
}

// The team run makes fewer requests than before the step loop: next replaces
// the issue, comment and budget reads around every turn, the progress lines
// and each sub-issue's claim, reads, comment and move.
func TestATeamRunMakesNoMoreRequestsThanBefore(t *testing.T) {
	r := fullTeamRun(t)
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	// Before CSR-179 the same run (Rounds) made 41 requests, 43 with a check
	// command; a check now adds one next request per check.
	if n := len(r.api.calls); n > 41 || n != 17 {
		t.Errorf("requests = %d, want 17 (41 before):\n%s", n, strings.Join(r.api.calls, "\n"))
	}
}

// A worker the app gives a reader token gets one made for its sub-issue,
// deleted when a step on another issue starts.
func TestAReaderWorkerGetsAReaderToken(t *testing.T) {
	r := newTeamStepRun(t,
		teamStep("CSR-1.1", "One", "Qwen", "new", "reader"),
		teamStep("CSR-1", "Parent", "Builder", "new", "orchestrator"),
		failStep,
	)
	r.worker.Runs = []client.RunReply{ok("w1", "One done.")}
	r.cl.Runs = []client.RunReply{ok("s1", "Reviewed.")}
	_ = r.run(t)
	if ts := turns(&r.worker.Fake); len(ts) != 1 || ts[0].Token != "reader-CSR-1.1" {
		t.Errorf("worker turns = %+v, want the reader token", ts)
	}
	var tokens []string
	for _, c := range r.api.calls {
		if strings.Contains(c, "/agent-token") {
			tokens = append(tokens, c)
		}
	}
	want := []string{
		`POST /runs/CSR-1.1/agent-token {"agent":"Qwen","kind":"reader"}`,
		"DELETE /runs/CSR-1.1/agent-token",
		`POST /runs/CSR-1/agent-token {"agent":"Builder","kind":"orchestrator"}`,
		"DELETE /runs/CSR-1/agent-token",
	}
	if !slices.Equal(tokens, want) {
		t.Errorf("token calls = %q\nwant %q", tokens, want)
	}
}

// A sub-issue's turn that hit the turn limit is sent as such, with the
// worker's session offered; the app's resume continues that session. Each
// turn's work is committed and pushed.
func TestASubIssuesTurnLimit(t *testing.T) {
	r := newTeamStepRun(t,
		teamStep("CSR-1.1", "One", "Qwen", "new", "none"),
		teamStep("CSR-1.1", "One", "Qwen", "resume", "none"),
		failStep,
	)
	r.worker.Runs = []client.RunReply{
		{Result: client.Result{SessionID: "w1", HitTurnLimit: true, Tail: "tail"}, Err: errFailed},
		ok("w1", "Done."),
	}
	_ = r.run(t)
	reqs := r.reqs(t)
	if l := reqs[1].Last; l.Issue != "CSR-1.1" || l.Outcome != "turn_limit" || deref(l.Tail) != "tail" {
		t.Errorf("last = %+v, want the turn limit on CSR-1.1", l)
	}
	if s := reqs[1].Sessions; !slices.Equal(s, []cinderline.ResumableSession{offered("CSR-1.1", "Qwen", "turn")}) {
		t.Errorf("sessions = %+v, want the worker's", s)
	}
	if ts := turns(&r.worker.Fake); len(ts) != 2 || ts[1].SessionID != "w1" || ts[1].Prompt != "Prompt for Qwen on CSR-1.1." {
		t.Errorf("worker turns = %+v, want the second in w1", ts)
	}
	if got := strings.Join(r.wsMethods(), ","); strings.Count(got, "Commit,Push") != 2 {
		t.Errorf("workspace = %s, want each turn committed and pushed", got)
	}
}

// A pause keeps the lead's session in the paused note and ends the run
// without a push, a move or a pull request.
func TestATeamPauseKeepsTheLeadsSession(t *testing.T) {
	r := newTeamStepRun(t,
		teamStep("CSR-1", "Parent", "Builder", "new", "orchestrator"),
		teamStep("CSR-1.1", "One", "Qwen", "new", "none"),
		`{"kind":"pause","reason":"waiting","block":false,"comment":null}`,
	)
	r.cl.Runs = []client.RunReply{ok("s1", "Planned.")}
	r.worker.Runs = []client.RunReply{ok("w1", "Asked.")}
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	if n, ok := r.p.readPaused("CSR-1"); !ok || n != (Paused{Dir: r.dir, Session: "s1", Client: "claude"}) {
		t.Errorf("note = %+v, %v; want Builder's s1 paused", n, ok)
	}
	if got := r.writesTo("/issues/CSR-1"); len(got) != 0 {
		t.Errorf("moves = %q, want none", got)
	}
	if n := len(r.fg.Calls()); n != 0 {
		t.Errorf("forge calls = %d, want none", n)
	}
}

// After a restart the run starts from the paused note, which stays during
// the run: the first request sends no state and offers the lead's session
// as paused, and the app's resume continues it.
func TestARestartSendsNoState(t *testing.T) {
	r := newTeamStepRun(t,
		teamStep("CSR-1.1", "One", "Qwen", "new", "none"),
		teamStep("CSR-1", "Parent", "Builder", "resume", "orchestrator"),
		failStep,
	)
	clone := t.TempDir()
	if err := r.p.SavePaused("CSR-1", Paused{Dir: clone, Session: "s0", Client: "claude"}); err != nil {
		t.Fatal(err)
	}
	var during Paused
	r.worker.during = func(client.Turn) { during, _ = r.p.readPaused("CSR-1") }
	r.worker.Runs = []client.RunReply{ok("w1", "One done.")}
	r.cl.Runs = []client.RunReply{ok("s0", "Reviewed.")}
	_ = r.run(t)

	first := r.reqs(t)[0]
	if first.State != nil || first.Last != nil || !slices.Equal(first.Sessions, []cinderline.ResumableSession{offered("CSR-1", "Builder", "paused")}) {
		t.Errorf("first request = %+v, want no state and the paused session", first)
	}
	if during.Session != "s0" {
		t.Errorf("note during the run = %+v, want it kept for another restart", during)
	}
	if ts := turns(r.cl); len(ts) != 1 || ts[0].SessionID != "s0" || ts[0].Dir != clone {
		t.Errorf("Builder's turns = %+v, want s0 resumed in the paused clone", ts)
	}
	if slices.Contains(r.wsMethods(), "Prepare") {
		t.Errorf("workspace = %v, want the paused clone reused", r.wsMethods())
	}
}

// conflictAt makes the n-th base merge conflict in work.txt, with the
// branch's head "old" and its commits.
func (r *stepRun) conflictAt(n int) {
	r.ws.MergeBases = make([]workspace.MergeBaseReply, 10)
	r.ws.MergeBases[n] = workspace.MergeBaseReply{Conflicts: []string{"work.txt"}}
	r.ws.Heads = []workspace.HeadReply{{Commit: "old"}, {Commit: "old"}}
	r.ws.Logs = []workspace.LogReply{{Log: "abc CSR-1: Part"}}
}

// A conflict in the merge that catches continued work up with the base is
// the first request's last step, with its files and commits. The resolve
// step runs the app's agent, and a verified merge is sent as finished; the
// app posts its report.
func TestAMergeConflictInARun(t *testing.T) {
	r := newTeamStepRun(t, resolveStep, teamStep("CSR-1", "Parent", "Builder", "resume", "orchestrator"), failStep)
	if err := r.p.SavePaused("CSR-1", Paused{Dir: r.dir, Session: "s0", Client: "claude"}); err != nil {
		t.Fatal(err)
	}
	r.conflictAt(0)
	r.ws.Cleans = []workspace.BoolReply{{Yes: true}}
	r.ws.Ancestors = []workspace.IsAncestorReply{{Yes: true}, {Yes: true}}
	r.ws.Markers = []workspace.BoolReply{{}}
	r.worker.Runs = []client.RunReply{ok("r1", "Resolved.")}
	r.cl.Runs = []client.RunReply{ok("s0", "Done.")}
	_ = r.run(t)

	reqs := r.reqs(t)
	if l := reqs[0].Last; l == nil || l.Step != "merge" || l.Issue != "CSR-1" || l.Agent != nil || l.Outcome != "conflict" ||
		!slices.Equal(l.Conflicts, []string{"work.txt"}) || deref(l.Commits) != "abc CSR-1: Part" || l.ID == "" {
		t.Fatalf("first last = %+v, want the conflict", l)
	}
	if l := reqs[1].Last; l.Step != "resolve" || deref(l.Agent) != "Merge Resolver" || l.Outcome != "finished" || deref(l.Report) != "Resolved." {
		t.Errorf("second last = %+v, want the verified resolve", l)
	}
	if s := reqs[1].Sessions; !slices.Equal(s, []cinderline.ResumableSession{offered("CSR-1", "Builder", "paused"), offered("CSR-1", "Merge Resolver", "turn")}) {
		t.Errorf("sessions = %+v", s)
	}
	want := client.Turn{Dir: r.dir, Prompt: "Merge main.", MaxTurns: 50, MaxMinutes: 45, Idle: defaultIdle, Model: "openrouter/qwen", Author: "Dev / Merge Resolver"}
	if ts := turns(&r.worker.Fake); !slices.Equal(ts, []client.Turn{want}) {
		t.Errorf("resolver turns = %+v, want %+v", ts, want)
	}
	if slices.Contains(r.wsMethods(), "Reset") {
		t.Errorf("workspace = %v, want a verified merge kept", r.wsMethods())
	}
	if got := r.writesTo("/issues/CSR-1/comments"); len(got) != 0 {
		t.Errorf("comments = %q, want none: the app posts the merge report", got)
	}
	if ts := turns(r.cl); len(ts) != 1 || ts[0].SessionID != "s0" {
		t.Errorf("Builder's turns = %+v", ts)
	}
}

// A solo agent resolves in its own session with its token when the app
// says so (decision 2026-11-07-solo-team-resolves-merge-conflicts).
func TestASoloAgentResolvesInItsSession(t *testing.T) {
	r := newStepRun(t, `{"kind":"resolve","agent":"Coder","session":"resume","token":"orchestrator","prompt":"Merge main.","progress":null}`, failStep)
	if err := r.p.SavePaused("CSR-7", Paused{Dir: r.dir, Session: "s0", Client: "claude"}); err != nil {
		t.Fatal(err)
	}
	r.conflictAt(0)
	r.ws.Cleans = []workspace.BoolReply{{Yes: true}}
	r.ws.Ancestors = []workspace.IsAncestorReply{{Yes: true}, {Yes: true}}
	r.ws.Markers = []workspace.BoolReply{{}}
	r.cl.Runs = []client.RunReply{ok("s0", "Resolved.")}
	_ = r.run(t)
	if ts := turns(r.cl); len(ts) != 1 || ts[0].SessionID != "s0" || ts[0].Token != "scoped" {
		t.Errorf("turns = %+v, want s0 with the token", ts)
	}
	if l := r.reqs(t)[1].Last; l.Outcome != "finished" || l.Issue != "CSR-7" {
		t.Errorf("last = %+v", l)
	}
}

// A conflict at publish ends the publish step and goes to next; the app's
// publish without resolve_conflicts pushes it as it is, for a person.
func TestAMergeConflictAtPublish(t *testing.T) {
	r := newTeamStepRun(t,
		teamStep("CSR-1", "Parent", "Builder", "new", "orchestrator"),
		publishStep,
		`{"kind":"publish","resolve_conflicts":false,"report_head":null,"report_foot":null}`,
	)
	r.conflictAt(1) // the catch-up's merge is clean, publish's two conflict
	r.ws.MergeBases[2] = r.ws.MergeBases[1]
	r.cl.Runs = []client.RunReply{ok("s1", "Done.")}
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	reqs := r.reqs(t)
	if len(reqs) != 3 || reqs[2].Last.Step != "merge" || !slices.Equal(reqs[2].Last.Conflicts, []string{"work.txt"}) {
		t.Fatalf("requests = %+v, want the publish's conflict last", reqs)
	}
	if got := r.writesTo("/issues/CSR-1/comments"); len(got) != 1 || !strings.Contains(got[0], "It conflicts with main in work.txt") {
		t.Errorf("comments = %q, want the conflict in the finish report", got)
	}
	if n := len(turns(&r.worker.Fake)); n != 0 {
		t.Errorf("resolver turns = %d, want none", n)
	}
}

// The runner verifies a resolution itself and undoes one it cannot verify:
// a dirty worktree, a missing base or branch head, conflict markers, a
// report that says Blocked, a failed turn, or a check that errs. Ported from
// the rows of legacy runner.test.mjs "a branch that conflicts with main at
// the push, and a Merge Resolver that <way>".
func TestAMergeTheRunnerCannotVerifyIsUndone(t *testing.T) {
	resolved := ok("r1", "Resolved.")
	yes, no := workspace.BoolReply{Yes: true}, workspace.BoolReply{}
	anc := func(base, head bool) []workspace.IsAncestorReply {
		return []workspace.IsAncestorReply{{Yes: base}, {Yes: head}}
	}
	for _, tt := range []struct {
		way     string
		run     client.RunReply
		cleans  []workspace.BoolReply
		ancs    []workspace.IsAncestorReply
		markers []workspace.BoolReply
		outcome string
		checks  []string // the workspace calls after the turn, before a reset
	}{
		{"resolves", resolved, []workspace.BoolReply{yes}, anc(true, true), []workspace.BoolReply{no}, "finished",
			[]string{"Clean", "IsAncestor", "IsAncestor", "ConflictMarkers"}},
		{"commits the markers", resolved, []workspace.BoolReply{yes}, anc(true, true), []workspace.BoolReply{yes}, "not_verified",
			[]string{"Clean", "IsAncestor", "IsAncestor", "ConflictMarkers"}},
		{"leaves the worktree unclean", resolved, []workspace.BoolReply{no}, nil, nil, "not_verified", []string{"Clean"}},
		{"does not merge the base", resolved, []workspace.BoolReply{yes}, anc(false, true), nil, "not_verified", []string{"Clean", "IsAncestor"}},
		{"drops the branch's work", resolved, []workspace.BoolReply{yes}, anc(true, false), nil, "not_verified", []string{"Clean", "IsAncestor", "IsAncestor"}},
		{"reports Blocked", ok("r1", "**Blocked**: unsure."), nil, nil, nil, "not_verified", nil},
		{"fails", client.RunReply{Err: errFailed}, nil, nil, nil, "failed", nil},
		{"leaves a check failing", resolved, []workspace.BoolReply{{Err: errors.New("git broke")}}, nil, nil, "not_verified", []string{"Clean"}},
	} {
		t.Run(tt.way, func(t *testing.T) {
			r := newTeamStepRun(t, resolveStep, failStep)
			r.conflictAt(0)
			r.ws.Cleans, r.ws.Ancestors, r.ws.Markers = tt.cleans, tt.ancs, tt.markers
			r.ws.Resets = []error{nil}
			r.worker.Runs = []client.RunReply{tt.run}
			_ = r.run(t)

			if l := r.reqs(t)[1].Last; l.Outcome != tt.outcome || (tt.outcome != "finished") != (l.Error != nil) {
				t.Errorf("last = %+v, want %s", l, tt.outcome)
			}
			want := append([]string{"Stranded", "Prepare", "Commit", "MergeBase", "Head", "Log"}, tt.checks...)
			if tt.outcome != "finished" {
				want = append(want, "Reset")
			}
			if got := r.wsMethods(); !slices.Equal(got, want) {
				t.Errorf("workspace = %v, want %v", got, want)
			}
			for _, c := range r.ws.Calls() {
				switch c.Method {
				case "IsAncestor":
					if a := c.Args[1]; a != "main" && a != "old" || c.Args[2] != "HEAD" {
						t.Errorf("IsAncestor %v: want main, then the old head, in HEAD", c.Args)
					}
				case "ConflictMarkers":
					if !slices.Equal(c.Args[1].([]string), []string{"work.txt"}) {
						t.Errorf("ConflictMarkers looked at %v", c.Args[1])
					}
				case "Reset":
					if c.Args[1] != "old" {
						t.Errorf("Reset to %v, want the head before the merge", c.Args[1])
					}
				}
			}
		})
	}
}

// A reset that fails stops the run: the clone may hold a half merge, which
// must never be pushed.
func TestAMergeResetThatFailsStopsTheRun(t *testing.T) {
	r := newTeamStepRun(t, resolveStep, failStep)
	r.conflictAt(0)
	r.ws.Resets = []error{errors.New("disk full")}
	r.worker.Runs = []client.RunReply{{Err: errFailed}}
	if err := r.run(t); err == nil || err.Error() != "disk full" {
		t.Fatalf("run = %v, want the reset's error", err)
	}
	if slices.Contains(r.wsMethods(), "Push") {
		t.Errorf("workspace = %v, want nothing pushed", r.wsMethods())
	}
}

// A resolve turn a person stops is undone before halt pushes the work.
func TestAStoppedResolveIsUndoneBeforeThePush(t *testing.T) {
	fastWatch(t)
	r := newTeamStepRun(t, resolveStep, `{"kind":"pause","reason":"stopped","block":false,"comment":null}`)
	r.api.routes["GET /issues/CSR-1"] = reply{200, issueJSON("CSR-1", "cancelled")}
	r.conflictAt(0)
	r.ws.Resets = []error{nil}
	r.p.Clients["opencode"] = &hang{}
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.wsMethods(), ",")
	if !strings.HasSuffix(got, "Reset,Commit,Head,Push") {
		t.Errorf("workspace = %s, want the reset before halt's push", got)
	}
	if l := r.reqs(t)[1].Last; l.Outcome != "stopped" {
		t.Errorf("last = %+v", l)
	}
}

// CSR-169: an agent's git name is its token's name, cut as the app cuts it.
func TestGitAuthor(t *testing.T) {
	if got := gitAuthor(cinderline.Team{Name: "Opus 5.5 Solo"}, "Feature Coder"); got != "Opus 5.5 Solo / Feature Coder" {
		t.Errorf("gitAuthor = %q", got)
	}
	long := gitAuthor(cinderline.Team{Name: strings.Repeat("é", 99)}, "Coder")
	if want := strings.Repeat("é", 99) + " "; long != want {
		t.Errorf("gitAuthor of a long name = %q, want 100 characters", long)
	}
	if got := leadAuthor(cinderline.Team{Name: "Empty"}); got != "" {
		t.Errorf("leadAuthor without agents = %q, want none", got)
	}
}
