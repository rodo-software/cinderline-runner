package run

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// hang is an agent that runs until it is stopped, as one that waits inside
// its session.
type hang struct {
	mu   sync.Mutex
	runs int
}

func (h *hang) Run(ctx context.Context, t client.Turn) (client.Result, error) {
	h.mu.Lock()
	h.runs++
	h.mu.Unlock()
	<-ctx.Done()
	return client.Result{SessionID: "s1"}, ctx.Err()
}

func fastWatch(t *testing.T) {
	t.Helper()
	old := watchEvery
	watchEvery = 5 * time.Millisecond
	t.Cleanup(func() { watchEvery = old })
}

func stopWorkspace() *workspace.Fake {
	return &workspace.Fake{
		Commits: []workspace.CommitReply{{Committed: true}},
		Heads:   []workspace.HeadReply{{Commit: "0123456789abcdef0123456789abcdef01234567"}},
		Pushes:  []error{nil},
	}
}

func wsMethods(ws *workspace.Fake) []string {
	var out []string
	for _, c := range ws.Calls() {
		out = append(out, c.Method)
	}
	return out
}

// A client with no output for the idle limit is stopped by the Environment;
// its work is committed and pushed, the issue is Blocked with a note that
// names the last activity, and the paused note resumes the session once a
// person moves it to To do (CSR-159).
func TestAnIdleRunIsPushedAndBlocked(t *testing.T) {
	p := project(t)
	api := working()
	api.routes["POST /issues/CSR-7/comments"] = reply{201, `{"id":9}`}
	api.routes["PATCH /issues/CSR-7"] = reply{200, issueJSON("CSR-7", "blocked")}
	ws := stopWorkspace()
	j := job
	j.Workspace = ws
	idle := fmt.Errorf("%w: no output for 10m0s; the last line was: sleep 3600", environment.ErrIdle)
	cl := &client.Fake{Runs: []client.RunReply{{Result: client.Result{SessionID: "s1"}, Err: idle}}}

	out, err := p.Attempts(context.Background(), api.client(t), cl, j)
	if err != nil || !out.Paused {
		t.Fatalf("Attempts = %+v, %v; want paused", out, err)
	}
	if got := strings.Join(wsMethods(ws), ","); got != "Commit,Head,Push" {
		t.Errorf("workspace calls = %s, want the work committed and pushed", got)
	}
	writes := strings.Join(api.writes(), "\n")
	if !strings.Contains(writes, `PATCH /issues/CSR-7 {"status":"blocked","expected_status":"in_progress"}`) {
		t.Errorf("writes = %s, want a move to Blocked", writes)
	}
	for _, want := range []string{"Review this manually", "sleep 3600", "cinderline/CSR-7", "0123456789ab"} {
		if !strings.Contains(writes, want) {
			t.Errorf("the note lacks %q:\n%s", want, writes)
		}
	}
	if n, ok := p.readPaused("CSR-7"); !ok || n.Session != "s1" {
		t.Errorf("paused note = %+v, %v; want session s1", n, ok)
	}
}

// Moving the issue to Blocked, Cancelled or Backlog during a run stops the
// agent within a status check, with its work pushed and a note; the runner
// does not move the issue itself.
func TestAStatusChangeStopsTheRun(t *testing.T) {
	fastWatch(t)
	for _, status := range []string{"blocked", "cancelled", "backlog"} {
		t.Run(status, func(t *testing.T) {
			p := project(t)
			api := working()
			api.routes["GET /issues/CSR-7"] = reply{200, issueJSON("CSR-7", status)}
			api.routes["POST /issues/CSR-7/comments"] = reply{201, `{"id":9}`}
			ws := stopWorkspace()
			j := job
			j.Workspace = ws
			out, err := p.Attempts(context.Background(), api.client(t), &hang{}, j)
			if err != nil || !out.Paused {
				t.Fatalf("Attempts = %+v, %v; want paused", out, err)
			}
			if got := strings.Join(wsMethods(ws), ","); got != "Commit,Head,Push" {
				t.Errorf("workspace calls = %s", got)
			}
			writes := strings.Join(api.writes(), "\n")
			if strings.Contains(writes, "PATCH") || !strings.Contains(writes, "Review this manually") || !strings.Contains(writes, "moved to x") {
				t.Errorf("writes = %s, want only the note naming the status", writes)
			}
		})
	}
}

// Right after the agent's own question the issue is Blocked while the agent
// ends its turn: the watch gives it one more check before stopping it.
func TestTheAgentsOwnQuestionGetsAGrace(t *testing.T) {
	fastWatch(t)
	p := project(t)
	blocked := `{"ref":"CSR-7","title":"T","status":{"value":"blocked","label":"Blocked"},"comments":[{"id":3,"body":"Which one?","author":{"name":"Runner","agent":"Coder"}}]}`
	api := working()
	api.queues = map[string][]reply{"GET /issues/CSR-7": {{200, blocked}, {200, blocked}}}
	api.routes["POST /issues/CSR-7/comments"] = reply{201, `{"id":9}`}
	h := &hang{}
	if _, err := p.turn(context.Background(), api.client(t), h, "CSR-7", client.Turn{Dir: "/clone"}); err == nil {
		t.Fatal("turn = nil, want a stop")
	}
	gets := 0
	for _, c := range api.calls {
		if strings.HasPrefix(c, "GET /issues/CSR-7") && !strings.Contains(c, "comments") {
			gets++
		}
	}
	if gets != 2 {
		t.Errorf("status checks = %d, want 2: one grace", gets)
	}
}

// A run that ends by itself is not touched by the watch.
func TestAFinishedRunIsNotStopped(t *testing.T) {
	fastWatch(t)
	res, cl, err := attempt(t, client.RunReply{Result: client.Result{SessionID: "s1", Report: "done"}})
	if err != nil || res.Paused || len(cl.Calls()) != 1 {
		t.Fatalf("Attempts = %+v, %v", res, err)
	}
}

// When Cinderline stops answering as the agent finishes, the finished work
// still goes on to publish, and the agent's final text waits in the outbox,
// as its own comment may have failed too (CSR-159, the CSR-34 incident).
func TestAnOutageAtTheEndStillPublishes(t *testing.T) {
	down := reply{502, `{}`}
	api := &fakeAPI{queues: map[string][]reply{
		"GET /issues/CSR-7":          {{200, issueJSON("CSR-7", "in_progress")}, down}, // the budget check, then the status
		"GET /issues/CSR-7/comments": {{200, `{"comments":[]}`}, down},
	}, routes: map[string]reply{"POST /issues/CSR-7/comments": down}}
	c := api.client(t)
	cl := &client.Fake{Runs: []client.RunReply{{Result: client.Result{SessionID: "s1", Report: "Added the endpoint."}}}}
	out, err := project(t).Attempts(context.Background(), c, cl, job)
	if err != nil || out.Paused || out.Report != "Added the endpoint." {
		t.Fatalf("Attempts = %+v, %v; want the finished work for publish", out, err)
	}
	if !c.Waiting("CSR-7") {
		t.Error("the agent's report is not in the outbox")
	}
}

// The idle limit is the project's idle_minutes from Cinderline, set in its
// web panel (Willem on CSR-159); 20 minutes while the app sends none. A
// project with none seen yet has no runState in tests: no limit.
func TestTheIdleLimitIsTheProjects(t *testing.T) {
	ten := 10
	for name, tt := range map[string]struct {
		proj *cinderline.Project
		want time.Duration
	}{
		"set":     {&cinderline.Project{IdleMinutes: &ten}, 10 * time.Minute},
		"not set": {&cinderline.Project{}, defaultIdle},
		"no run":  {nil, 0},
	} {
		t.Run(name, func(t *testing.T) {
			p := project(t)
			if tt.proj != nil {
				p.run = &runState{}
				p.run.setIdle(*tt.proj)
			}
			cl := &client.Fake{Runs: []client.RunReply{{}}}
			if _, err := p.turn(context.Background(), working().client(t), cl, "CSR-7", client.Turn{Dir: "/clone"}); err != nil {
				t.Fatal(err)
			}
			if got := turns(cl)[0].Idle; got != tt.want {
				t.Errorf("Idle = %v, want %v", got, tt.want)
			}
		})
	}
}
