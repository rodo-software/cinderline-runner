package run

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// CSR-80: the runner runs the project's check command after each finished
// run on a sub-issue, and on a solo team's run before publish, and resumes
// the agent's own session with FixPrompt on a failure, at most maxFixes
// times. The verdict heads the sub-issue's comment.

const checkCmd = "make check"

// exitError is a check that ran and failed, as the Docker environment returns it.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitError) ExitCode() int { return int(e) }

func pass() environment.RunReply { return environment.RunReply{Output: "ok"} }
func fail(out string) environment.RunReply {
	return environment.RunReply{Output: out, Err: exitError(1)}
}

func envRuns(e *environment.Fake) []environment.Spec {
	var out []environment.Spec
	for _, c := range e.Calls() {
		if c.Method == "Run" {
			out = append(out, c.Args[0].(environment.Spec))
		}
	}
	return out
}

const unverified = "This project has no check command, so the runner did not verify these reports: check the work yourself."

const checkRule = "- After your run, the runner runs the project's check command `make check` in this worktree and sends a failure back to you. Do not delete, skip or weaken tests or checks to make it pass; if the check itself is wrong, say so in your report."

func TestFixPromptGolden(t *testing.T) {
	golden(t, "fix", FixPrompt("CSR-4", checkCmd, "FAIL: TestX"))
}

func TestRoundPromptNamesChecksFailedAndUnverified(t *testing.T) {
	ran := []Ran{{Ref: "CSR-4", Title: "A", Agent: "Qwen", ChecksFailed: true}, {Ref: "CSR-5", Title: "B", Agent: "Qwen", Failed: true}}
	got := RoundPrompt("CSR-3", ran, checkCmd)
	if !strings.Contains(got, "- CSR-4: A (Qwen, checks failed)\n- CSR-5: B (Qwen, failed)") {
		t.Errorf("RoundPrompt = %q, want the checks failed line", got)
	}
	if strings.Contains(got, unverified) {
		t.Errorf("RoundPrompt with a check command says the reports are unverified")
	}
	if got := RoundPrompt("CSR-3", ran, ""); strings.Count(got, unverified) != 1 {
		t.Errorf("RoundPrompt without a check command = %q, want %q once", got, unverified)
	}
	if got := RoundPrompt("CSR-3", nil, ""); strings.Contains(got, unverified) {
		t.Errorf("RoundPrompt without runs = %q, want no unverified note", got)
	}
}

func TestPromptsNameTheCheckCommandBeforeTheData(t *testing.T) {
	for name, got := range map[string]string{
		"worker": WorkerPrompt("Code.", "CSR-4", "CSR-3", "Part", "Body.", "b", checkCmd, false, nil),
		"coding": CodingPrompt(soloTeam, "CSR-3", "Add X", "Body.", "b", "CSR", checkCmd, nil, nil, nil),
	} {
		i := strings.Index(got, checkRule)
		if i < 0 || i > strings.LastIndex(got, "\n## ") {
			t.Errorf("%s prompt lacks the check rule before the issue text:\n%s", name, got)
		}
	}
	if got := WorkerPrompt("Code.", "CSR-4", "CSR-3", "Part", "Body.", "b", "", false, nil); strings.Contains(got, "check command") {
		t.Errorf("worker prompt without a check command names one:\n%s", got)
	}
}

// checkRun is a team run with CSR-2 for the worker Qwen in To do, resuming
// the orchestrator's paused session, which ends the round with nothing left.
func checkRun(t *testing.T, check string, env ...environment.RunReply) (*teamRun, Project, *environment.Fake, string) {
	t.Helper()
	p := project(t)
	p.Hooks()
	e := &environment.Fake{Runs: env}
	p.Env = e
	clone := t.TempDir()
	if err := p.SavePaused("CSR-1", Paused{Dir: clone, Session: "the-session", Client: "claude"}); err != nil {
		t.Fatal(err)
	}
	tr := newTree("in_progress")
	tr.add("CSR-2", "Part", "Qwen", "todo")
	r := newTeamRun(tr)
	r.job.Check = check
	r.claude.Runs = []client.RunReply{ok("the-session", "Reviewed.")}
	r.claude.during = func(t client.Turn) {
		if t.SessionID == "the-session" {
			tr.set("CSR-2", "done")
		}
	}
	return r, p, e, clone
}

func TestChecksPassOnTheFirstTry(t *testing.T) {
	r, p, e, clone := checkRun(t, checkCmd, pass())
	r.opencode.Runs = []client.RunReply{ok("ses_w", "Qwen report.")}

	if out, err := r.run(t, context.Background(), p); err != nil || out.Paused {
		t.Fatalf("Rounds = %+v, %v", out, err)
	}
	specs := envRuns(e)
	if len(specs) != 1 {
		t.Fatalf("check runs = %d, want 1", len(specs))
	}
	s := specs[0]
	if !slices.Equal(s.Cmd, []string{"sh", "-c", checkCmd}) || s.Dir != clone || s.Issue != path.Base(clone) || s.Timeout != 45*time.Minute {
		t.Errorf("check spec = %+v; want sh -c %q in %s with a 45 minute limit", s, checkCmd, clone)
	}
	if runs := workspace.Volume(cfg.ID(), "runs") + ":" + workspace.RunsPath; !slices.Contains(s.Mounts, runs) {
		t.Errorf("check mounts = %v, want the clone's volume %s", s.Mounts, runs)
	}
	if got := r.commentOn("CSR-2"); got != "Checks passed on the first try.\n\nQwen report." {
		t.Errorf("CSR-2 comment = %q", got)
	}
	if !slices.Contains(r.progress(), "Checking CSR-2 (try 1)") {
		t.Errorf("progress = %q, want Checking CSR-2 (try 1)", r.progress())
	}
	if n := len(r.opencode.Calls()); n != 1 {
		t.Errorf("worker runs = %d, want 1", n)
	}
}

func TestChecksFailThenPassAfterAFixInTheWorkersSession(t *testing.T) {
	r, p, e, _ := checkRun(t, checkCmd, fail("FAIL: TestX"), pass())
	r.opencode.Runs = []client.RunReply{ok("ses_w", "First."), ok("ses_w", "Fixed.")}
	r.opencode.Runs[0].Result.Usage = cost(0.5)
	r.opencode.Runs[1].Result.Usage = cost(0.25)

	if out, err := r.run(t, context.Background(), p); err != nil || out.Paused {
		t.Fatalf("Rounds = %+v, %v", out, err)
	}
	ts := turns(&r.opencode.Fake)
	if len(ts) != 2 {
		t.Fatalf("worker runs = %d, want 2", len(ts))
	}
	if ts[1].SessionID != "ses_w" || ts[1].Prompt != FixPrompt("CSR-2", checkCmd, "FAIL: TestX") || ts[1].Model != ts[0].Model || ts[1].Dir != ts[0].Dir {
		t.Errorf("fix turn = %+v; want the worker's session with FixPrompt", ts[1])
	}
	if n := len(envRuns(e)); n != 2 {
		t.Errorf("check runs = %d, want 2", n)
	}
	if got := r.commentOn("CSR-2"); got != "Checks passed after 1 fix.\n\nFixed." {
		t.Errorf("CSR-2 comment = %q", got)
	}
	if pr := r.progress(); !slices.Contains(pr, "Checking CSR-2 (try 1)") || !slices.Contains(pr, "Checking CSR-2 (try 2)") {
		t.Errorf("progress = %q, want tries 1 and 2", pr)
	}
	if got := p.run.pendingCost("CSR-1"); got != 0.75 {
		t.Errorf("usage of CSR-1's run = %v, want 0.75 with the fix run's", got)
	}
}

func TestChecksFailFourTimesEndAsChecksFailedInReview(t *testing.T) {
	r, p, e, _ := checkRun(t, checkCmd, fail("FAIL 1"), fail("FAIL 2"), fail("FAIL 3"), fail("FAIL 4"))
	r.opencode.Runs = []client.RunReply{ok("ses_w", "First."), ok("ses_w", "Fix 1."), ok("ses_w", "Fix 2."), ok("ses_w", "Fix 3.")}

	if out, err := r.run(t, context.Background(), p); err != nil || out.Paused {
		t.Fatalf("Rounds = %+v, %v", out, err)
	}
	if n := len(r.opencode.Calls()); n != 1+maxFixes || maxFixes != 3 {
		t.Errorf("worker runs = %d with maxFixes %d, want 4 and 3", n, maxFixes)
	}
	if n := len(envRuns(e)); n != 4 {
		t.Errorf("check runs = %d, want 4", n)
	}
	want := "**Checks failed** after 3 fixes. The last lines of `make check`:\n\n```\nFAIL 4\n```\n\nFix 3."
	if got := r.commentOn("CSR-2"); got != want {
		t.Errorf("CSR-2 comment = %q, want %q", got, want)
	}
	if got := r.moves("CSR-2"); !slices.Equal(got, []string{"in_progress", "review"}) {
		t.Errorf("CSR-2 moves = %v, want [in_progress review]", got)
	}
	ts := turns(&r.claude.Fake)
	if len(ts) != 1 || !strings.Contains(ts[0].Prompt, "- CSR-2: Part (Qwen, checks failed)") {
		t.Errorf("orchestrator turns = %+v, want the round prompt with checks failed", ts)
	}
}

func TestNoCheckCommandMeansNoStepAndUnverifiedReports(t *testing.T) {
	r, p, e, _ := checkRun(t, "")
	r.opencode.Runs = []client.RunReply{ok("ses_w", "Qwen report.")}

	if out, err := r.run(t, context.Background(), p); err != nil || out.Paused {
		t.Fatalf("Rounds = %+v, %v", out, err)
	}
	if n := len(envRuns(e)); n != 0 {
		t.Errorf("check runs = %d, want none", n)
	}
	if got := r.commentOn("CSR-2"); got != "Qwen report." {
		t.Errorf("CSR-2 comment = %q", got)
	}
	ts := turns(&r.claude.Fake)
	if len(ts) != 1 || strings.Count(ts[0].Prompt, unverified) != 1 {
		t.Errorf("orchestrator turns = %+v, want the unverified note once", ts)
	}
}

func TestABudgetStopInsideTheCheckLoopKeepsThePausedNote(t *testing.T) {
	r, p, e, _ := checkRun(t, checkCmd, fail("FAIL: TestX"))
	r.opencode.Runs = []client.RunReply{{Result: client.Result{SessionID: "ses_w", Report: "First.", Usage: cost(9)}}}
	r.opencode.during = func(client.Turn) { // the worker's $9 reaches the $5 budget before its fix run
		r.tree.mu.Lock()
		r.tree.budget = 5
		r.tree.mu.Unlock()
	}

	out, err := r.run(t, context.Background(), p)
	if err != nil || !out.Paused {
		t.Fatalf("Rounds = %+v, %v; want paused", out, err)
	}
	if n := len(r.opencode.Calls()); n != 1 {
		t.Errorf("worker runs = %d, want 1: no fix run over budget", n)
	}
	if n := len(envRuns(e)); n != 1 {
		t.Errorf("check runs = %d, want 1", n)
	}
	if got := r.moves("CSR-1"); !slices.Equal(got, []string{"blocked"}) {
		t.Errorf("CSR-1 moves = %v, want [blocked]", got)
	}
	if n, _ := p.readPaused("CSR-1"); n.Session != "the-session" || !n.Kept.IsZero() {
		t.Errorf("note = %+v, want the paused session", n)
	}
}

func TestTheOrchestratorsOwnSubIssueGetsTheCheck(t *testing.T) {
	r, p, e, _ := checkRun(t, checkCmd, fail("FAIL: TestX"), pass())
	r.tree.agent["CSR-2"] = "Builder"
	r.claude.Runs = []client.RunReply{ok("fresh", "Mine."), ok("fresh", "Mine fixed."), ok("the-session", "Reviewed.")}

	if out, err := r.run(t, context.Background(), p); err != nil || out.Paused {
		t.Fatalf("Rounds = %+v, %v", out, err)
	}
	ts := turns(&r.claude.Fake)
	if len(ts) != 3 || ts[1].SessionID != "fresh" || ts[1].Prompt != FixPrompt("CSR-2", checkCmd, "FAIL: TestX") || ts[1].Token != "reader-CSR-2" {
		t.Fatalf("orchestrator turns = %+v; want its sub-issue's session resumed with FixPrompt and its reader token", ts)
	}
	if n := len(envRuns(e)); n != 2 {
		t.Errorf("check runs = %d, want 2", n)
	}
	if got := r.commentOn("CSR-2"); got != "Checks passed after 1 fix.\n\nMine fixed." {
		t.Errorf("CSR-2 comment = %q", got)
	}
}

// A solo team's run is checked inside Attempts, before publish, resuming its session.
func soloCheck(t *testing.T, env []environment.RunReply, runs ...client.RunReply) (Outcome, *client.Fake, *environment.Fake, *workspace.Fake, error) {
	t.Helper()
	p := project(t)
	p.Hooks()
	e := &environment.Fake{Runs: env}
	p.Env = e
	ws := &workspace.Fake{Commits: make([]workspace.CommitReply, 10), Pushes: make([]error, 10)}
	cl := &client.Fake{Runs: runs}
	j := job
	j.Check, j.Workspace, j.Branch = checkCmd, ws, "cinderline/CSR-7"
	j.Turn.MaxMinutes = 45
	api := working()
	api.routes["POST /issues/CSR-7/progress"] = reply{204, ""}
	out, err := p.Attempts(context.Background(), api.client(t), cl, j)
	return out, cl, e, ws, err
}

func TestASoloRunGetsTheCheckBeforePublish(t *testing.T) {
	out, cl, e, ws, err := soloCheck(t, []environment.RunReply{fail("FAIL: TestX"), pass()},
		ok("s1", "Done."), ok("s1", "Fixed."))
	if err != nil || out.Paused || out.ChecksFailed {
		t.Fatalf("Attempts = %+v, %v", out, err)
	}
	if out.Verdict != "Checks passed after 1 fix." || out.Report != "Fixed." {
		t.Errorf("verdict %q, report %q; want Checks passed after 1 fix. and the fix run's report", out.Verdict, out.Report)
	}
	ts := turns(cl)
	if len(ts) != 2 || ts[1].SessionID != "s1" || ts[1].Prompt != FixPrompt("CSR-7", checkCmd, "FAIL: TestX") {
		t.Errorf("turns = %+v; want the session resumed with FixPrompt", ts)
	}
	if n := len(envRuns(e)); n != 2 {
		t.Errorf("check runs = %d, want 2", n)
	}
	var pushes int
	for _, c := range ws.Calls() {
		if c.Method == "Push" {
			pushes++
		}
	}
	if pushes != 2 {
		t.Errorf("pushes = %d, want one before each check", pushes)
	}
}

func TestASoloRunThatStaysRedEndsAsChecksFailed(t *testing.T) {
	out, cl, _, _, err := soloCheck(t, []environment.RunReply{fail("F1"), fail("F2"), fail("F3"), fail("F4")},
		ok("s1", "Done."), ok("s1", "1."), ok("s1", "2."), ok("s1", "3."))
	if err != nil || out.Paused || !out.ChecksFailed {
		t.Fatalf("Attempts = %+v, %v; want ChecksFailed", out, err)
	}
	if want := "**Checks failed** after 3 fixes. The last lines of `make check`:\n\n```\nF4\n```"; out.Verdict != want {
		t.Errorf("verdict = %q, want %q", out.Verdict, want)
	}
	if n := len(cl.Calls()); n != 4 {
		t.Errorf("runs = %d, want 4", n)
	}
}

func TestAPublishCommentStartsWithTheVerdict(t *testing.T) {
	if got := withVerdict("Checks passed on the first try.", "Opened x."); got != "Checks passed on the first try.\n\nOpened x." {
		t.Errorf("withVerdict = %q", got)
	}
	if got := withVerdict("", "Opened x."); got != "Opened x." {
		t.Errorf("withVerdict without a verdict = %q", got)
	}
}
