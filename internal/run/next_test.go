package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/scheduler"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// The step loop of a solo or generic run (CSR-178): the app's answers to
// nextStep are scripted, and the tests check the facts each request sends
// and how the runner does each step. What the app decides, and the prompt
// texts, are tested in the app (EMBER-215).

// stepRun is a solo team's CSR-7 through the Run and Publish hooks, with
// next answering steps in order, the last one repeating.
type stepRun struct {
	ref    string
	p      Project
	api    *fakeAPI
	cl     *client.Fake // claude: the solo agent, or a team's Builder
	worker *agentCLI    // opencode: a team's Qwen and Merge Resolver
	ws     *workspace.Fake
	fg     *forge.Fake
	env    *environment.Fake
	dir    string
}

func newStepRun(t *testing.T, steps ...string) *stepRun {
	t.Helper()
	api := soloRunAPI()
	api.handle = nil // the steps are scripted
	api.routes["PATCH /issues/CSR-7"] = reply{200, issueJSON("CSR-7", "in_progress")}
	api.routes["POST /runs/CSR-7/usage"] = reply{204, ""}
	api.routes["GET /issues/CSR-7/attachments/5"] = reply{200, "PDF"}
	return newStepRunOf(t, "CSR-7", api, steps)
}

// newStepRunOf is a run of ref with api's answers and next answering steps.
func newStepRunOf(t *testing.T, ref string, api *fakeAPI, steps []string) *stepRun {
	t.Helper()
	var q []reply
	for i, s := range steps {
		q = append(q, reply{200, `{"state":"state-` + string(rune('1'+i)) + `","step":` + s + `}`})
	}
	api.queues = map[string][]reply{"POST /runs/" + ref + "/next": q}
	r := &stepRun{
		ref:    ref,
		p:      project(t),
		api:    api,
		cl:     &client.Fake{},
		worker: &agentCLI{},
		ws: &workspace.Fake{
			Commits:    make([]workspace.CommitReply, 10),
			MergeBases: make([]workspace.MergeBaseReply, 10),
			Ancestors:  make([]workspace.IsAncestorReply, 10),
			Pushes:     make([]error, 10),
			Heads:      []workspace.HeadReply{{Commit: "abc123"}, {Commit: "abc123"}},
		},
		fg:  &forge.Fake{PullRequests: []forge.PullRequestReply{{PullRequest: testPR}}},
		env: &environment.Fake{Cleanups: []error{nil}},
		dir: t.TempDir(),
	}
	r.ws.Prepares = []workspace.PrepareReply{{Dir: r.dir}}
	if _, err := r.p.Secrets.Set(cfg, secrets.AnthropicCredential, "sk-ant"); err != nil {
		t.Fatal(err)
	}
	r.p.Env = r.env
	r.p.Clients = map[string]client.Client{"claude": r.cl, "opencode": r.worker}
	r.p.Workspace = r.ws
	r.p.Forge = func(string) (forge.Forge, error) { return r.fg, nil }
	r.p.Hooks()
	return r
}

// run calls the Run hook and, when it hands off, the Publish hook, as the
// scheduler does.
func (r *stepRun) run(t *testing.T) error {
	t.Helper()
	c := r.api.client(t)
	issue := cinderline.Issue{Ref: r.ref, Team: &cinderline.IssueTeam{ID: 1}}
	h := r.p.Hooks()
	if err := h.Run(context.Background(), c, issue); err != nil {
		return err
	}
	return h.Publish(context.Background(), c, issue)
}

// A plain solo run (one turn, then publish) makes no more requests than it
// did before the step loop: next replaces the budget read, the status read
// after the turn, both comment reads and the usage call.
func TestAPlainSoloRunMakesNoMoreRequestsThanBefore(t *testing.T) {
	r := newStepRun(t,
		`{"kind":"run","agent":"Coder","session":"new","token":"orchestrator","prompt":"Do CSR-7.","progress":null,"issue":{"ref":"CSR-7","title":"Add hello"},"attachments":[]}`,
		`{"kind":"publish","resolve_conflicts":true,"report_head":null,"report_foot":null}`,
	)
	r.cl.Runs = []client.RunReply{{Result: client.Result{SessionID: "s1", Report: "Added hello.", Usage: cost(1)}}}
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	// Before CSR-178 the same run made 15 requests: two status reads, two
	// comment reads and a usage call around its turn, which two next calls
	// replace.
	calls := strings.Join(r.api.calls, "\n")
	if n := len(r.api.calls); n != 12 {
		t.Errorf("requests = %d, want 12 (15 before):\n%s", n, calls)
	}
	if strings.Count(calls, "GET /issues/CSR-7\n") != 1 || strings.Contains(calls, "/comments\n") || strings.Contains(calls, "GET /issues/CSR-7/comments") || strings.Contains(calls, "/usage") {
		t.Errorf("requests around the turn:\n%s", calls)
	}
}

// simpleNext is an app for agent's solo team on CSR-7: the first call gets a
// run, in the session the request offers if any, and a later call publish.
func simpleNext(agent string) func(key, body string) (reply, bool) {
	return func(key, body string) (reply, bool) {
		if key != "POST /runs/CSR-7/next" {
			return reply{}, false
		}
		var req cinderline.NextRequest
		_ = json.Unmarshal([]byte(body), &req)
		if req.Last != nil {
			return reply{200, `{"state":"s2","step":{"kind":"publish","resolve_conflicts":true,"report_head":null,"report_foot":null}}`}, true
		}
		session := "new"
		if len(req.Sessions) > 0 {
			session = "resume"
		}
		return reply{200, `{"state":"s1","step":{"kind":"run","agent":"` + agent + `","session":"` + session + `","token":"orchestrator","prompt":"Do CSR-7.","progress":null,"issue":{"ref":"CSR-7","title":"Add hello"},"attachments":[]}}`}, true
	}
}

// nextRequests are the next requests api received, in order, of any issue.
func nextRequests(t *testing.T, api *fakeAPI) []cinderline.NextRequest {
	t.Helper()
	api.mu.Lock()
	defer api.mu.Unlock()
	var out []cinderline.NextRequest
	for _, c := range api.calls {
		if _, body, ok := strings.Cut(c, "/next "); ok && strings.HasPrefix(c, "POST /runs/") {
			var r cinderline.NextRequest
			if err := json.Unmarshal([]byte(body), &r); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
	}
	return out
}

// runStep is a run step on CSR-7 for Coder.
func runStep(session, token string) string {
	return `{"kind":"run","agent":"Coder","session":"` + session + `","token":"` + token + `","prompt":"Do CSR-7.","progress":"Coder is working","issue":{"ref":"CSR-7","title":"Add hello"},"attachments":[]}`
}

const (
	publishStep = `{"kind":"publish","resolve_conflicts":true,"report_head":null,"report_foot":null}`
	failStep    = `{"kind":"fail","reason":"attempts_failed","comment":"both runs failed"}`
)

func (r *stepRun) reqs(t *testing.T) []cinderline.NextRequest {
	t.Helper()
	return nextRequests(t, r.api)
}

// writesTo are the writes on path, as "METHOD body".
func (r *stepRun) writesTo(path string) []string {
	var out []string
	for _, w := range r.api.writes() {
		method, rest, _ := strings.Cut(w, " ")
		if p, body, _ := strings.Cut(rest, " "); p == path {
			out = append(out, strings.TrimSpace(method+" "+body))
		}
	}
	return out
}

func (r *stepRun) wsMethods() []string { return wsMethods(r.ws) }

// The first request carries no state, no last step and the run's facts: the
// branch, the base, the extra repositories it fetched (their URL without
// user info) and the linked projects. Every later request echoes the state
// of the answer before it.
func TestTheFirstRequestSendsTheRunsFacts(t *testing.T) {
	r := newStepRun(t, runStep("new", "orchestrator"), failStep)
	r.p.Config.ExtraRepositories = []config.ExtraRepository{{URL: "https://bob:secret@git.example/o/lib"}}
	if _, err := r.p.Secrets.Set(r.p.Config, secrets.ExtraGitToken("https://bob:secret@git.example/o/lib"), "tok"); err != nil {
		t.Fatal(err)
	}
	r.p.fetchExtras = func(context.Context, string, []workspace.Extra) map[string]error { return nil }
	r.cl.Runs = []client.RunReply{{Result: client.Result{SessionID: "s1"}}}
	_ = r.run(t)

	reqs := r.reqs(t)
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	first := reqs[0]
	if first.Context != "run" || first.State != nil || first.Last != nil || first.Sessions == nil || len(first.Sessions) != 0 {
		t.Errorf("first request = %+v", first)
	}
	w := first.Workspace
	if w.Branch != "cinderline/CSR-7" || w.Base != "main" || len(w.LinkedProjects) != 0 ||
		len(w.ExtraRepositories) != 1 || w.ExtraRepositories[0] != (cinderline.WorkspaceRepository{Path: "/extra/lib", URL: "https://git.example/o/lib"}) {
		t.Errorf("workspace = %+v", w)
	}
	if s := reqs[1].State; s == nil || *s != "state-1" {
		t.Errorf("second state = %v, want state-1 echoed", s)
	}
	// Empty lists are sent as lists, not null, as the contract has them.
	api := r.api
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, c := range api.calls {
		if strings.HasPrefix(c, "POST /runs/CSR-7/next") && strings.Contains(c, "null,") && strings.Contains(c, `"sessions":null`) {
			t.Errorf("request with a null list: %s", c)
		}
	}
}

// A run step starts a new session with the step's prompt and a token of the
// step's kind; the next request says how it ended, with its report and
// usage, and offers its session. A resume continues that session, and the
// token is made once per kind.
func TestARunStepRunsTheAgentAndReportsTheTurn(t *testing.T) {
	r := newStepRun(t, runStep("new", "reader"), runStep("resume", "reader"), failStep)
	r.cl.Runs = []client.RunReply{
		{Result: client.Result{SessionID: "s1", Report: "Built it.", Usage: cost(2)}},
		{Result: client.Result{SessionID: "s1", Report: "Fixed it."}},
	}
	_ = r.run(t)

	ts := turns(r.cl)
	if len(ts) != 2 {
		t.Fatalf("turns = %d, want 2", len(ts))
	}
	want := client.Turn{Dir: r.dir, Prompt: "Do CSR-7.", MaxTurns: 50, MaxMinutes: 30, Idle: defaultIdle, Author: "Dev / Coder", Token: "scoped"}
	if ts[0] != want {
		t.Errorf("first turn = %+v, want %+v", ts[0], want)
	}
	want.SessionID = "s1"
	if ts[1] != want {
		t.Errorf("resumed turn = %+v, want %+v", ts[1], want)
	}
	if got := r.writesTo("/runs/CSR-7/agent-token"); len(got) != 2 || got[0] != `POST {"agent":"Coder","kind":"reader"}` || got[1] != "DELETE" {
		t.Errorf("token calls = %q, want one reader token, deleted at the end", got)
	}
	reqs := r.reqs(t)
	last := reqs[1].Last
	if last == nil || last.Step != "run" || last.Issue != "CSR-7" || deref(last.Agent) != "Coder" || last.Outcome != "finished" ||
		deref(last.Report) != "Built it." || last.Error != nil || last.Tail != nil || last.ID == "" || last.Conflicts == nil {
		t.Fatalf("last = %+v", last)
	}
	if u := last.Usage; u == nil || u.Issue != "CSR-7" || u.Agent != "Coder" || *u.CostUSD != 2 || u.InputTokens != 10 {
		t.Errorf("usage = %+v", u)
	}
	if s := reqs[1].Sessions; len(s) != 1 || s[0] != (cinderline.ResumableSession{Issue: "CSR-7", Agent: "Coder", Origin: "turn"}) {
		t.Errorf("sessions = %+v, want s1 from this turn", s)
	}
	if reqs[2].Last.ID == last.ID || reqs[2].Last.Usage != nil {
		t.Errorf("second turn's last = %+v; want a new id and no usage", reqs[2].Last)
	}
}

// A turn that hit the turn limit, failed, or ran out of time is sent with
// that outcome, the error and the output's tail.
func TestAFailedTurnIsSentWithItsOutcome(t *testing.T) {
	for outcome, rep := range map[string]client.RunReply{
		"turn_limit": {Result: client.Result{SessionID: "s1", HitTurnLimit: true, Tail: "tail"}, Err: errFailed},
		"failed":     {Result: client.Result{SessionID: "s1", Tail: "tail"}, Err: errFailed},
		"time_limit": {Result: client.Result{SessionID: "s1", Tail: "tail"}, Err: fmt.Errorf("%w (30m0s)", environment.ErrTimeout)},
	} {
		t.Run(outcome, func(t *testing.T) {
			r := newStepRun(t, runStep("new", "none"), failStep)
			r.cl.Runs = []client.RunReply{rep}
			_ = r.run(t)
			last := r.reqs(t)[1].Last
			if last.Outcome != outcome || deref(last.Error) != rep.Err.Error() || deref(last.Tail) != "tail" || last.Report != nil {
				t.Errorf("last = %+v, want %s with the error and tail", last, outcome)
			}
			if got := r.writesTo("/runs/CSR-7/agent-token"); len(got) != 0 {
				t.Errorf("token calls = %q, want none for token none", got)
			}
			if ts := turns(r.cl); len(ts) != 1 || ts[0].Token != "" {
				t.Errorf("turns = %+v, want one without a token", ts)
			}
		})
	}
}

// A step the runner cannot start is the not_started outcome: an agent that
// is not the team's, or an issue other than the run's.
func TestARunStepItCannotStartIsNotStarted(t *testing.T) {
	for name, step := range map[string]string{
		"another agent": strings.Replace(runStep("new", "none"), `"agent":"Coder"`, `"agent":"Ghost"`, 1),
		"another issue": strings.Replace(runStep("new", "none"), `"ref":"CSR-7"`, `"ref":"CSR-8"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			r := newStepRun(t, step, failStep)
			_ = r.run(t)
			if n := len(r.cl.Calls()); n != 0 {
				t.Errorf("turns = %d, want none", n)
			}
			if last := r.reqs(t)[1].Last; last.Outcome != "not_started" || last.Error == nil {
				t.Errorf("last = %+v, want not_started", last)
			}
		})
	}
}

// A resume without a session the runner holds stops the run with an error.
func TestAResumeWithoutASessionStopsTheRun(t *testing.T) {
	r := newStepRun(t, runStep("resume", "none"))
	if err := r.run(t); err == nil || !strings.Contains(err.Error(), "does not hold") {
		t.Fatalf("run = %v, want an error", err)
	}
	if n := len(r.cl.Calls()); n != 0 {
		t.Errorf("turns = %d, want none", n)
	}
}

// A pause for a person keeps the paused note with the session, pushes and
// moves nothing, posts its comment when it has one, and publishes nothing.
func TestAPauseKeepsThePausedNote(t *testing.T) {
	r := newStepRun(t, runStep("new", "orchestrator"), `{"kind":"pause","reason":"waiting","block":false,"comment":null}`)
	r.cl.Runs = []client.RunReply{{Result: client.Result{SessionID: "s1"}}}
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	if n, ok := r.p.readPaused("CSR-7"); !ok || n.Session != "s1" || n.Dir != r.dir || !n.Kept.IsZero() {
		t.Errorf("note = %+v, %v; want s1 paused", n, ok)
	}
	if got := r.wsMethods(); slices.Contains(got, "Push") {
		t.Errorf("workspace = %v, want no push", got)
	}
	if got := r.writesTo("/issues/CSR-7"); len(got) != 0 {
		t.Errorf("moves = %q, want none", got)
	}
	if got := r.writesTo("/issues/CSR-7/comments"); len(got) != 0 {
		t.Errorf("comments = %q, want none", got)
	}
	if n := len(r.fg.Calls()); n != 0 {
		t.Errorf("forge calls = %d, want none", n)
	}
}

// A budget stop (pause with block) commits and pushes the work before it
// moves the issue to Blocked and posts the app's comment; the note keeps the
// session. A push that fails stops the run with that error before the move.
func TestABudgetPausePushesFirst(t *testing.T) {
	budget := `{"kind":"pause","reason":"budget","block":true,"comment":"**Budget reached**: this issue has cost $5.13 of its $5.00 budget."}`
	r := newStepRun(t, runStep("new", "orchestrator"), budget)
	r.cl.Runs = []client.RunReply{{Result: client.Result{SessionID: "s1"}}}
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	if got := r.wsMethods(); !slices.Equal(got, []string{"Prepare", "Commit", "MergeBase", "Commit", "Push"}) {
		t.Errorf("workspace = %v, want the work committed and pushed", got)
	}
	if got := r.writesTo("/issues/CSR-7"); !slices.Equal(got, []string{`PATCH {"status":"blocked","expected_status":"in_progress"}`}) {
		t.Errorf("moves = %q", got)
	}
	if got := r.writesTo("/issues/CSR-7/comments"); len(got) != 1 || !strings.Contains(got[0], "Budget reached") {
		t.Errorf("comments = %q", got)
	}
	if n, ok := r.p.readPaused("CSR-7"); !ok || n.Session != "s1" || !n.Kept.IsZero() {
		t.Errorf("note = %+v, %v; want s1 paused", n, ok)
	}

	r = newStepRun(t, runStep("new", "orchestrator"), budget)
	r.cl.Runs = []client.RunReply{{Result: client.Result{SessionID: "s1"}}}
	r.ws.Pushes = []error{errors.New("rejected")}
	if err := r.run(t); err == nil || err.Error() != "rejected" {
		t.Fatalf("run = %v, want the push's error", err)
	}
	if got := r.writesTo("/issues/CSR-7"); len(got) != 0 {
		t.Errorf("moves = %q, want none before the work is pushed", got)
	}
}

// fail ends the run with the app's comment as its error, which
// scheduler.finish posts before Blocked; the session is kept for a follow-up.
func TestAFailStepIsTheRunsError(t *testing.T) {
	r := newStepRun(t, runStep("new", "orchestrator"), failStep)
	r.cl.Runs = []client.RunReply{{Result: client.Result{SessionID: "s1"}}}
	if err := r.run(t); err == nil || err.Error() != "both runs failed" {
		t.Fatalf("run = %v, want the fail step's comment", err)
	}
	if n, ok := r.p.readPaused("CSR-7"); !ok || n.Session != "s1" || n.Kept.IsZero() {
		t.Errorf("note = %+v, %v; want s1 kept", n, ok)
	}
	if got := r.writesTo("/runs/CSR-7/agent-token"); len(got) != 2 || got[1] != "DELETE" {
		t.Errorf("token calls = %q, want the token deleted", got)
	}
}

// A kind the runner does not know, or one a run of its kind never gets,
// stops the run with an error that names the runner's version; a coding
// run's work is committed and pushed first, a generic run's never.
func TestAnUnknownStepStopsTheRun(t *testing.T) {
	for name, tt := range map[string]struct {
		step    string
		generic bool
		push    bool
	}{
		"a new kind":           {`{"kind":"gate","until":"approved"}`, false, true},
		"none in a run":        {`{"kind":"none"}`, false, true},
		"resolve before merge": {`{"kind":"resolve","agent":"Coder","session":"resume","token":"none","prompt":"Merge.","progress":null}`, false, true},
		"check, generic":       {`{"kind":"check","issue":{"ref":"CSR-7","title":"T"},"progress":null}`, true, false},
		"publish, generic":     {publishStep, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := newStepRun(t, tt.step)
			if tt.generic {
				r.api.routes["GET /teams/1"] = reply{200, `{"id":1,"name":"Dev","max_minutes":30,"generic_runs":true,"agents":[{"name":"Coder","role":"coder","client":"claude"}]}`}
			}
			err := r.run(t)
			if err == nil || !strings.Contains(err.Error(), "runner 0.0.0") {
				t.Fatalf("run = %v, want an error naming the version", err)
			}
			if pushed := slices.Contains(r.wsMethods(), "Push"); pushed != tt.push {
				t.Errorf("workspace = %v, want pushed %v", r.wsMethods(), tt.push)
			}
			if n := len(r.cl.Calls()) + len(r.fg.Calls()); n != 0 {
				t.Errorf("%d agent or forge calls, want none", n)
			}
		})
	}
}

// A check step commits and pushes the work, runs the project's check
// command, and sends whether it passed, with the tail of a failure and the
// last turn's report.
func TestACheckStepRunsTheCheck(t *testing.T) {
	check := `{"kind":"check","issue":{"ref":"CSR-7","title":"Add hello"},"progress":"Checking CSR-7 (try 1)"}`
	r := newStepRun(t, runStep("new", "orchestrator"), check, check, failStep)
	r.api.routes["GET /project"] = reply{200, `{"key":"CSR","name":"CSR","repository_url":"` + cfg.Repository + `","forge":"forgejo","base_branch":"main","check_command":"make check"}`}
	r.env.Runs = []environment.RunReply{fail("FAIL: TestX"), pass()}
	r.cl.Runs = []client.RunReply{{Result: client.Result{SessionID: "s1", Report: "Done."}}}
	_ = r.run(t)

	specs := envRuns(r.env)
	if len(specs) != 2 || !slices.Equal(specs[0].Cmd, []string{"sh", "-c", "make check"}) || specs[0].Dir != r.dir || specs[0].Timeout != 30*time.Minute {
		t.Errorf("check runs = %+v", specs)
	}
	if got := strings.Join(r.wsMethods(), ","); !strings.HasSuffix(got, "Commit,Push,Commit,Push") {
		t.Errorf("workspace = %s, want a commit and push before each check", got)
	}
	reqs := r.reqs(t)
	failed, passed := reqs[2].Last, reqs[3].Last
	if failed.Step != "check" || failed.Outcome != "failed" || deref(failed.Tail) != "FAIL: TestX" || deref(failed.Report) != "Done." || failed.Usage != nil {
		t.Errorf("failed check = %+v", failed)
	}
	if passed.Outcome != "passed" || passed.Tail != nil {
		t.Errorf("passed check = %+v", passed)
	}
	if s := reqs[3].Sessions; len(s) != 1 || s[0].Origin != "turn" {
		t.Errorf("sessions = %+v, want the turn's still offered", s)
	}
}

// A check without a check command does not start.
func TestACheckWithoutACommandIsNotStarted(t *testing.T) {
	r := newStepRun(t, `{"kind":"check","issue":{"ref":"CSR-7","title":"T"},"progress":null}`, failStep)
	_ = r.run(t)
	if last := r.reqs(t)[1].Last; last.Outcome != "not_started" || !strings.Contains(deref(last.Error), "no check command") {
		t.Errorf("last = %+v", last)
	}
	if n := len(envRuns(r.env)); n != 0 {
		t.Errorf("check runs = %d, want none", n)
	}
}

// publish hands the run to the Publish hook with the session and token for
// its merge attempt; the finish report sits between the app's head and foot.
func TestAPublishStepPublishesBetweenHeadAndFoot(t *testing.T) {
	r := newStepRun(t, runStep("new", "orchestrator"), `{"kind":"publish","resolve_conflicts":true,"report_head":"Checks passed on the first try.","report_foot":"A person commented."}`)
	r.cl.Runs = []client.RunReply{{Result: client.Result{SessionID: "s1"}}}
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	want := `POST {"body":"Checks passed on the first try.\n\nOpened ` + testPR.URL + `. Review it and merge it; then move this issue to done.\n\nA person commented."}`
	if got := r.writesTo("/issues/CSR-7/comments"); !slices.Equal(got, []string{want}) {
		t.Errorf("comments = %q, want %q", got, want)
	}
	if got := r.writesTo("/issues/CSR-7"); !slices.Equal(got, []string{`PATCH {"status":"review","expected_status":"in_progress"}`}) {
		t.Errorf("moves = %q", got)
	}
	if n, _ := r.p.readPaused("CSR-7"); n.Session != "s1" || n.Kept.IsZero() {
		t.Errorf("note = %+v, want s1 kept", n)
	}
}

// finish pushes a coding run's branch, posts the app's comment and moves the
// issue to Review, with no pull request.
func TestAFinishStepEndsWithoutAPullRequest(t *testing.T) {
	r := newStepRun(t, runStep("new", "orchestrator"), `{"kind":"finish","comment":"**Checks failed** after 3 fixes."}`)
	r.cl.Runs = []client.RunReply{{Result: client.Result{SessionID: "s1"}}}
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	if got := r.wsMethods(); got[len(got)-1] != "Push" {
		t.Errorf("workspace = %v, want the branch pushed", got)
	}
	if got := r.writesTo("/issues/CSR-7/comments"); !slices.Equal(got, []string{`POST {"body":"**Checks failed** after 3 fixes."}`}) {
		t.Errorf("comments = %q", got)
	}
	if got := r.writesTo("/issues/CSR-7"); !slices.Equal(got, []string{`PATCH {"status":"review","expected_status":"in_progress"}`}) {
		t.Errorf("moves = %q", got)
	}
	if n := len(r.fg.Calls()); n != 0 {
		t.Errorf("forge calls = %d, want none", n)
	}
}

// A person moving the issue during the turn stops it (CSR-159): halt pushes
// the work and posts its note, and the next request says stopped.
func TestAStoppedTurnIsSentAsStopped(t *testing.T) {
	fastWatch(t)
	r := newStepRun(t, runStep("new", "orchestrator"), `{"kind":"pause","reason":"stopped","block":false,"comment":null}`)
	r.api.routes["GET /issues/CSR-7"] = reply{200, issueJSON("CSR-7", "cancelled")}
	r.p.Clients["claude"] = &hang{}
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	if last := r.reqs(t)[1].Last; last.Outcome != "stopped" || !strings.Contains(deref(last.Error), "moved to x") {
		t.Errorf("last = %+v, want stopped", last)
	}
	if got := r.writesTo("/issues/CSR-7/comments"); len(got) != 1 || !strings.Contains(got[0], "Review this manually") {
		t.Errorf("comments = %q, want halt's note", got)
	}
	if n, ok := r.p.readPaused("CSR-7"); !ok || n.Session != "s1" {
		t.Errorf("note = %+v, %v", n, ok)
	}
}

// When the first next call fails, the run did not start: the issue goes back
// to To do (ErrNotStarted) and the note TakePaused took is put back as it was.
func TestAFirstNextThatFailsDoesNotStartTheRun(t *testing.T) {
	r := newStepRun(t)
	r.api.routes["POST /runs/CSR-7/next"] = reply{502, `{}`}
	if err := r.p.SavePaused("CSR-7", Paused{Dir: r.dir, Session: "old", Client: "claude"}); err != nil {
		t.Fatal(err)
	}
	r.ws.FastForwards = []error{nil}
	err := r.run(t)
	if !errors.Is(err, scheduler.ErrNotStarted) {
		t.Fatalf("run = %v, want ErrNotStarted", err)
	}
	if n := len(r.reqs(t)); n != 1 {
		t.Errorf("next calls = %d, want 1: the first is not retried", n)
	}
	if n, ok := r.p.readPaused("CSR-7"); !ok || n.Session != "old" || !n.Kept.IsZero() {
		t.Errorf("note = %+v, %v; want the paused note back", n, ok)
	}
}

// When a later next call fails after its retries, the run ends as a pause
// with block: the work is pushed, the session kept in the paused note, the
// issue Blocked with the runner's own comment, and the turn's usage that
// next did not deliver goes with reportUsage (decision
// 2026-11-15-server-decides-steps point 8).
func TestALaterNextThatFailsPausesTheRun(t *testing.T) {
	r := newStepRun(t, runStep("new", "orchestrator"))
	r.api.queues["POST /runs/CSR-7/next"] = append(r.api.queues["POST /runs/CSR-7/next"], reply{502, `{}`})
	r.cl.Runs = []client.RunReply{{Result: client.Result{SessionID: "s1", Usage: cost(1)}}}
	c := r.api.client(t)
	c.SendWaits = []time.Duration{time.Millisecond, time.Millisecond}
	if err := r.p.Hooks().Run(context.Background(), c, cinderline.Issue{Ref: "CSR-7", Team: &cinderline.IssueTeam{ID: 1}}); err != nil {
		t.Fatal(err)
	}
	if n := len(r.reqs(t)); n != 4 {
		t.Errorf("next calls = %d, want 4: the first and three tries of the second", n)
	}
	if got := r.wsMethods(); got[len(got)-1] != "Push" {
		t.Errorf("workspace = %v, want the work pushed", got)
	}
	if got := r.writesTo("/issues/CSR-7"); !slices.Equal(got, []string{`PATCH {"status":"blocked","expected_status":"in_progress"}`}) {
		t.Errorf("moves = %q", got)
	}
	if got := r.writesTo("/issues/CSR-7/comments"); len(got) != 1 || !strings.Contains(got[0], "Cinderline did not answer") || !strings.Contains(got[0], "cinderline/CSR-7") {
		t.Errorf("comments = %q", got)
	}
	if got := usageReports(r.api, "CSR-7"); len(got) != 1 || !strings.Contains(got[0], `"cost_usd":1`) {
		t.Errorf("usage reports = %q, want the turn's", got)
	}
	if n, ok := r.p.readPaused("CSR-7"); !ok || n.Session != "s1" || !n.Kept.IsZero() {
		t.Errorf("note = %+v, %v; want s1 paused", n, ok)
	}
	if h := r.p.takeHandoff(); h != nil {
		t.Errorf("handoff = %+v, want no publish", h)
	}
}

// The workspace facts name each extra repository by its folder and each
// linked project with the folders it fetched; empty lists stay lists.
func TestRunWorkspace(t *testing.T) {
	w := runWorkspace("cinderline/CSR-7", "main",
		[]config.ExtraRepository{{URL: "https://git.example/o/design-system.git"}},
		[]Linked{{Key: "EMBER", Name: "App", Paths: []string{"EMBER/app"}}, {Key: "DOCS"}})
	b, _ := json.Marshal(w)
	want := `{"branch":"cinderline/CSR-7","base":"main","extra_repositories":[{"path":"/extra/design-system","url":"https://git.example/o/design-system.git"}],"linked_projects":[{"key":"EMBER","paths":["EMBER/app"]},{"key":"DOCS","paths":[]}]}`
	if string(b) != want {
		t.Errorf("workspace = %s\nwant %s", b, want)
	}
	if b, _ := json.Marshal(runWorkspace("b", "main", nil, nil)); !strings.Contains(string(b), `"extra_repositories":[],"linked_projects":[]`) {
		t.Errorf("empty workspace = %s", b)
	}
}

// A step's issue is the run's or a sub-issue's ref, nothing more.
func TestInTree(t *testing.T) {
	for issue, want := range map[string]bool{
		"CSR-1": true, "CSR-1.2": true, "CSR-1.2.3": true,
		"CSR-12": false, "CSR-1.": false, "CSR-1.x": false, "CSR-1.2 ; rm": false, "CSR-1.2/../3": false, "": false,
	} {
		if got := inTree("CSR-1", issue); got != want {
			t.Errorf("inTree(CSR-1, %q) = %v, want %v", issue, got, want)
		}
	}
}
