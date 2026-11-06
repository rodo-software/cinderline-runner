package run

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/rodo-software/cinderstack-runner/internal/client"
	"github.com/rodo-software/cinderstack-runner/internal/environment"
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

var job = Job{Ref: "CSR-7", Client: "claude", Kind: KindCode, Container: "box", Turn: first}

// working is CinderStack while the agent works: the issue stays In progress
// and nobody comments.
func working() *fakeAPI {
	return &fakeAPI{routes: map[string]reply{
		"GET /issues/CSR-7":          {200, issueJSON("CSR-7", "in_progress")},
		"GET /issues/CSR-7/comments": {200, `{"comments":[]}`},
	}}
}

func attempt(t *testing.T, runs ...client.RunReply) (Outcome, *client.Fake, *environment.Fake, error) {
	t.Helper()
	cl := &client.Fake{Runs: runs}
	env := &environment.Fake{Tails: []environment.TailReply{{Tail: "last lines"}}}
	res, err := project(t).Attempts(context.Background(), working().client(t), cl, env, job)
	return res, cl, env, err
}

var errFailed = errors.New("exit status 1")

// A run that succeeds is run once.
func TestAttemptsSucceedsFirstTime(t *testing.T) {
	res, cl, env, err := attempt(t, client.RunReply{Result: client.Result{SessionID: "s1", Report: "done"}})
	if err != nil || res.Report != "done" {
		t.Fatalf("Attempts = %+v, %v", res, err)
	}
	if got := turns(cl); len(got) != 1 || got[0] != first {
		t.Fatalf("turns = %+v", got)
	}
	if len(env.Calls()) != 0 {
		t.Fatalf("environment calls = %v, want none", env.Calls())
	}
}

// A run that hit the turn limit continues its own session with a fresh budget.
func TestAttemptsContinuesAfterTheTurnLimit(t *testing.T) {
	res, cl, _, err := attempt(t,
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
	_, cl, _, err := attempt(t,
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
	_, cl, _, err := attempt(t,
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

// A second failure ends the run with the Environment's output tail, which
// the scheduler posts as a comment before Review (0095); nothing is retried
// a third time, a turn limit included.
func TestAttemptsSecondFailureCarriesTheTail(t *testing.T) {
	for name, limit := range map[string]bool{"other": false, "turn limit": true} {
		t.Run(name, func(t *testing.T) {
			_, cl, env, err := attempt(t,
				client.RunReply{Result: client.Result{SessionID: "s1", HitTurnLimit: limit}, Err: errFailed},
				client.RunReply{Result: client.Result{SessionID: "s1", HitTurnLimit: limit}, Err: errFailed},
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
			if c := env.Calls(); len(c) != 1 || c[0].Method != "Tail" || c[0].Args[0] != "box" {
				t.Fatalf("environment calls = %v, want Tail(box)", c)
			}
		})
	}
}

// A tail the Environment cannot give does not hide the failure.
func TestAttemptsSecondFailureWithoutTail(t *testing.T) {
	cl := &client.Fake{Runs: []client.RunReply{{Err: errFailed}, {Err: errFailed}}}
	env := &environment.Fake{Tails: []environment.TailReply{{Err: errors.New("gone")}}}
	_, err := project(t).Attempts(context.Background(), working().client(t), cl, env, job)
	if err == nil || !strings.Contains(err.Error(), "both runs failed") || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("err = %v", err)
	}
}

// A stop is not a failure: no retry, so the next start's recovery takes it.
func TestAttemptsStopsWithoutRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cl := &client.Fake{Runs: []client.RunReply{{Err: context.Canceled}, {}}}
	env := &environment.Fake{}
	_, err := project(t).Attempts(ctx, working().client(t), cl, env, job)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// The cursor read already sees the stop, so no run starts; none is retried.
	if len(cl.Calls()) > 1 || len(env.Calls()) != 0 {
		t.Fatalf("client calls = %v, environment calls = %v", cl.Calls(), env.Calls())
	}
}

// Every step logs a line tagged with project and issue.
func TestAttemptsLogsEveryStep(t *testing.T) {
	var buf bytes.Buffer
	p := project(t)
	p.Log = slog.New(slog.NewTextHandler(&buf, nil))
	cl := &client.Fake{Runs: []client.RunReply{{Result: client.Result{SessionID: "s1", HitTurnLimit: true}, Err: errFailed}, {}}}
	if _, err := p.Attempts(context.Background(), working().client(t), cl, &environment.Fake{}, job); err != nil {
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
