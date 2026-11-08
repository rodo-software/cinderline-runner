package run

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/client"
)

// The legacy suites have no test of their own for attempts: runner.test.mjs
// only reaches them through other cases, and lib.test.mjs's "tail keeps the
// last lines and caps the length" and "parseRun reads the turn limit from the
// last JSON line" are the real Environment and Client (M4). These cases port
// the branches of legacy runner.mjs attempts() and orchestratorRound().

var first = client.Turn{Dir: "/clone", Prompt: "do CSR-7", MaxTurns: 30}

func turns(f *client.Fake) []client.Turn {
	var out []client.Turn
	for _, c := range f.Calls() {
		out = append(out, c.Args[0].(client.Turn))
	}
	return out
}

var job = Job{Ref: "CSR-7", Client: "claude", Turn: first}

// working is Cinderline while the agent works: the issue stays In progress
// and nobody comments.
func working() *fakeAPI {
	return &fakeAPI{routes: map[string]reply{
		"GET /issues/CSR-7":          {200, issueJSON("CSR-7", "in_progress")},
		"GET /issues/CSR-7/comments": {200, `{"comments":[]}`},
	}}
}

func attempt(t *testing.T, runs ...client.RunReply) (Outcome, *client.Fake, error) {
	t.Helper()
	cl := &client.Fake{Runs: runs}
	res, err := project(t).Attempts(context.Background(), working().client(t), cl, job)
	return res, cl, err
}

var errFailed = errors.New("exit status 1")

// A run that succeeds is run once.
func TestAttemptsSucceedsFirstTime(t *testing.T) {
	res, cl, err := attempt(t, client.RunReply{Result: client.Result{SessionID: "s1", Report: "done"}})
	if err != nil || res.Report != "done" {
		t.Fatalf("Attempts = %+v, %v", res, err)
	}
	if got := turns(cl); len(got) != 1 || got[0] != first {
		t.Fatalf("turns = %+v", got)
	}
}

// A run that hit the turn limit continues its own session with a fresh budget.
func TestAttemptsContinuesAfterTheTurnLimit(t *testing.T) {
	res, cl, err := attempt(t,
		client.RunReply{Result: client.Result{SessionID: "s1", HitTurnLimit: true}, Err: errFailed},
		client.RunReply{Result: client.Result{SessionID: "s1", Report: "done"}},
	)
	if err != nil || res.Report != "done" {
		t.Fatalf("Attempts = %+v, %v", res, err)
	}
	got := turns(cl)
	want := client.Turn{Dir: first.Dir, Prompt: ContinuePrompt("CSR-7"), SessionID: "s1", MaxTurns: first.MaxTurns}
	if len(got) != 2 || got[1] != want {
		t.Fatalf("turns = %+v, want second %+v", got, want)
	}
}

// Any other failure starts over: a fresh session with the first prompt.
func TestAttemptsStartsOverAfterOtherFailures(t *testing.T) {
	_, cl, err := attempt(t,
		client.RunReply{Result: client.Result{SessionID: "s1"}, Err: errFailed},
		client.RunReply{Result: client.Result{SessionID: "s2"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := turns(cl); len(got) != 2 || got[1] != first {
		t.Fatalf("turns = %+v, want second %+v", got, first)
	}
}

// A turn limit without a session to continue starts over too.
func TestAttemptsStartsOverWithoutASession(t *testing.T) {
	_, cl, err := attempt(t,
		client.RunReply{Result: client.Result{HitTurnLimit: true}, Err: errFailed},
		client.RunReply{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := turns(cl); len(got) != 2 || got[1] != first {
		t.Fatalf("turns = %+v, want second %+v", got, first)
	}
}

// A second failure ends the run with the tail of its output, which
// the scheduler posts as a comment before Review (0095); nothing is retried
// a third time, a turn limit included.
func TestAttemptsSecondFailureCarriesTheTail(t *testing.T) {
	for name, limit := range map[string]bool{"other": false, "turn limit": true} {
		t.Run(name, func(t *testing.T) {
			_, cl, err := attempt(t,
				client.RunReply{Result: client.Result{SessionID: "s1", HitTurnLimit: limit, Tail: "early lines"}, Err: errFailed},
				client.RunReply{Result: client.Result{SessionID: "s1", HitTurnLimit: limit, Tail: "last lines"}, Err: errFailed},
			)
			if err == nil || !strings.Contains(err.Error(), "both runs failed") || !strings.Contains(err.Error(), "```\nlast lines\n```") {
				t.Fatalf("err = %v, want both runs failed with the tail", err)
			}
			if !errors.Is(err, errFailed) {
				t.Fatalf("err = %v, want it to wrap the last failure", err)
			}
			if n := len(cl.Calls()); n != 2 {
				t.Fatalf("runs = %d, want 2", n)
			}
		})
	}
}

// A stop is not a failure: no retry, so the next start's recovery takes it.
func TestAttemptsStopsWithoutRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cl := &client.Fake{Runs: []client.RunReply{{Err: context.Canceled}, {}}}
	_, err := project(t).Attempts(ctx, working().client(t), cl, job)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// The cursor read already sees the stop, so no run starts; none is retried.
	if len(cl.Calls()) > 1 {
		t.Fatalf("client calls = %v", cl.Calls())
	}
}

// Every step logs a line tagged with project and issue.
func TestAttemptsLogsEveryStep(t *testing.T) {
	var buf bytes.Buffer
	p := project(t)
	p.Log = slog.New(slog.NewTextHandler(&buf, nil))
	cl := &client.Fake{Runs: []client.RunReply{{Result: client.Result{SessionID: "s1", HitTurnLimit: true}, Err: errFailed}, {}}}
	if _, err := p.Attempts(context.Background(), working().client(t), cl, job); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 4 {
		t.Fatalf("log = %q, want a line per start, failure, continue and success", lines)
	}
	for _, l := range lines {
		if !strings.Contains(l, "project=CSR") || !strings.Contains(l, "issue=CSR-7") {
			t.Fatalf("line %q is not tagged with project and issue", l)
		}
	}
}

// Ported from runner.mjs retryProgress (decision
// 2026-11-04-run-progress-updates): the run after a failure reports what it
// does, "Continuing after the turn limit" or "Retrying (attempt 2)", and a
// failing progress line never stops the run (runner.test.mjs "a failing
// progress line never stops the run": the fake API answers 404).
func TestAttemptsReportsTheRetry(t *testing.T) {
	for want, limit := range map[string]bool{"Retrying (attempt 2)": false, "Continuing after the turn limit": true} {
		t.Run(want, func(t *testing.T) {
			api := working()
			cl := &client.Fake{Runs: []client.RunReply{{Result: client.Result{SessionID: "s1", HitTurnLimit: limit}, Err: errFailed}, {}}}
			if _, err := project(t).Attempts(context.Background(), api.client(t), cl, job); err != nil {
				t.Fatal(err)
			}
			if w := api.writes(); len(w) != 1 || w[0] != `POST /issues/CSR-7/progress {"body":"`+want+`"}` {
				t.Fatalf("writes = %q, want the one progress line %q", w, want)
			}
		})
	}
}

// A progress line is cut to Cinderline's 200 characters, as legacy
// progress() does: a sub-issue title alone may be that long.
func TestProgressIsCutTo200Characters(t *testing.T) {
	api := &fakeAPI{routes: map[string]reply{"POST /issues/CSR-7/progress": {204, ""}}}
	project(t).progress(context.Background(), api.client(t), "CSR-7", strings.Repeat("é", 250))
	w := api.writes()
	if len(w) != 1 || w[0] != `POST /issues/CSR-7/progress {"body":"`+strings.Repeat("é", 200)+`"}` {
		t.Fatalf("writes = %q", w)
	}
}

// The cursor reads only comments after it (decision 2026-11-08-comment-cursor):
// on an issue with 250 comments, a person's answer followed by 200 agent
// comments during the run is still seen, and the session resumes to read it.
func TestAttemptsSeesAPersonsCommentPastTheCap(t *testing.T) {
	all := commentRange(1, 250, true)
	api := working()
	api.handle = func(key, _ string) (reply, bool) {
		path, ok := strings.CutPrefix(key, "GET ")
		if !ok || !strings.HasPrefix(path, "/issues/CSR-7/comments") {
			return reply{}, false
		}
		return commentPage(path, all), true
	}
	cl := &client.Fake{Runs: []client.RunReply{
		{Result: client.Result{SessionID: "s1", Report: "done"}},
		{Result: client.Result{SessionID: "s1", Report: "read"}},
	}}
	c := api.client(t)
	// The fake client has no hook into a run, so the comments the run adds are
	// in place as soon as the cursor read has happened.
	first := api.handle
	api.handle = func(key, body string) (reply, bool) {
		r, ok := first(key, body)
		if ok && len(all) == 250 {
			all = append(append(all, comment(251, false)), commentRange(252, 451, true)...)
		}
		return r, ok
	}
	res, err := project(t).Attempts(context.Background(), c, cl, job)
	if err != nil || res.Report != "read" {
		t.Fatalf("Attempts = %+v, %v", res, err)
	}
	if ts := turns(cl); len(ts) != 2 || ts[1].SessionID != "s1" || ts[1].Prompt != CommentsPrompt("CSR-7") {
		t.Fatalf("turns = %+v", ts)
	}
}
