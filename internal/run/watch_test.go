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

// working is Cinderline while the agent works: the issue stays In progress.
func working() *fakeAPI {
	return &fakeAPI{routes: map[string]reply{"GET /issues/CSR-7": {200, issueJSON("CSR-7", "in_progress")}}}
}

// stopStep is the app's answer after a stopped turn.
const stopStep = `{"kind":"pause","reason":"stopped","block":false,"comment":null}`

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
	r := newStepRun(t, runStep("new", "orchestrator"), stopStep)
	r.ws.Heads = []workspace.HeadReply{{Commit: "0123456789abcdef0123456789abcdef01234567"}}
	idle := fmt.Errorf("%w: no output for 10m0s; the last line was: sleep 3600", environment.ErrIdle)
	r.cl.Runs = []client.RunReply{{Result: client.Result{SessionID: "s1"}, Err: idle}}
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.wsMethods(), ","); !strings.HasSuffix(got, "Commit,Head,Push") {
		t.Errorf("workspace calls = %s, want the work committed and pushed", got)
	}
	writes := strings.Join(r.api.writes(), "\n")
	if !strings.Contains(writes, `PATCH /issues/CSR-7 {"status":"blocked","expected_status":"in_progress"}`) {
		t.Errorf("writes = %s, want a move to Blocked", writes)
	}
	for _, want := range []string{"Review this manually", "sleep 3600", "cinderline/CSR-7", "0123456789ab"} {
		if !strings.Contains(writes, want) {
			t.Errorf("the note lacks %q:\n%s", want, writes)
		}
	}
	if n, ok := r.p.readPaused("CSR-7"); !ok || n.Session != "s1" {
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
			r := newStepRun(t, runStep("new", "orchestrator"), stopStep)
			r.api.routes["GET /issues/CSR-7"] = reply{200, issueJSON("CSR-7", status)}
			r.p.Clients["claude"] = &hang{}
			if err := r.run(t); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(r.wsMethods(), ","); !strings.HasSuffix(got, "Commit,Head,Push") {
				t.Errorf("workspace calls = %s", got)
			}
			writes := strings.Join(r.api.writes(), "\n")
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
	cl := &client.Fake{Runs: []client.RunReply{{Result: client.Result{SessionID: "s1", Report: "done"}}}}
	res, err := project(t).turn(context.Background(), working().client(t), cl, "CSR-7", client.Turn{Dir: "/clone"})
	if err != nil || res.Report != "done" || len(cl.Calls()) != 1 {
		t.Fatalf("turn = %+v, %v", res, err)
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
