package run

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// CLR-216, decision 2026-11-24-agent-memory-limit: the runs of every
// project share one memory budget, Docker's memory less 1 GiB.

// budgetOf is a budget of size bytes: Docker reports size plus the headroom.
func budgetOf(size int64) *Budget {
	return NewBudget(func(context.Context) (int64, error) { return size + headroom, nil })
}

func reserved(b *Budget) int64 {
	_, r := b.Status()
	return r
}

func TestBudget(t *testing.T) {
	ctx := context.Background()
	b := budgetOf(6 << 30)
	if size, r := b.Status(); size != 0 || r != 0 {
		t.Errorf("before the first reservation: %d, %d; want 0, 0", size, r)
	}
	if ok, err := b.reserve(ctx, "A", 4<<30); !ok || err != nil {
		t.Fatalf("A = %v, %v", ok, err)
	}
	if ok, _ := b.reserve(ctx, "B", 4<<30); ok {
		t.Error("B fits next to A in 6 GiB")
	}
	if ok, _ := b.reserve(ctx, "B", 2<<30); !ok {
		t.Error("B of 2 GiB does not fit next to A")
	}
	if ok, _ := b.reserve(ctx, "A", 5<<30); ok {
		t.Error("A grew to 5 GiB next to B's 2")
	}
	if ok, _ := b.reserve(ctx, "A", 3<<30); !ok {
		t.Error("A could not shrink")
	}
	if size, r := b.Status(); size != 6<<30 || r != 5<<30 {
		t.Errorf("status = %d, %d; want 6 GiB, 5 GiB", size, r)
	}
	b.release("A")
	b.release("B")
	if ok, _ := b.reserve(ctx, "big", 9<<30); !ok {
		t.Error("a run larger than the budget does not run alone")
	}
	if ok, _ := b.reserve(ctx, "small", 1<<20); ok {
		t.Error("a run started next to one larger than the budget")
	}

	// Docker not up yet: nothing is held, and the next reservation reads again.
	fail := true
	b = NewBudget(func(context.Context) (int64, error) {
		if fail {
			return 0, errors.New("docker is starting")
		}
		return 8 << 30, nil
	})
	if ok, err := b.reserve(ctx, "A", 1<<30); ok || err == nil {
		t.Errorf("without Docker's memory: %v, %v; want refused with the reason", ok, err)
	}
	fail = false
	if ok, err := b.reserve(ctx, "A", 1<<30); !ok || err != nil || b.Size() != 7<<30 {
		t.Errorf("once Docker answers: %v, %v, size %d", ok, err, b.Size())
	}
}

// until waits for cond, failing the test after ten seconds.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// budgetProject is a fixture keyed key, whose runs take 4 GiB of b.
func budgetProject(t *testing.T, key string, b *Budget) *fixture {
	f := newFixture(t)
	f.p.Config.Key, f.p.Budget = key, b
	f.ws.Prepares = []workspace.PrepareReply{{Dir: "/runs/CSR-1"}, {Dir: "/runs/CSR-2"}}
	f.ws.Removes = []error{nil}
	return f
}

// loopIn runs f's loop against app in its own goroutine until ctx ends.
func loopIn(ctx context.Context, t *testing.T, f *fixture, app *fakeApp) (done chan struct{}) {
	c := app.client(t)
	done = make(chan struct{})
	go func() { f.p.Loop(ctx, c, nil); close(done) }()
	return done
}

// Two projects whose runs do not both fit run one at a time: while one
// project's run goes, from its team's slot or its Project Manager's, the
// other offers neither slot, and once it ends the other's run starts. The
// first project's other slot does not fit next to its own run either.
func TestTwoProjectsThatDoNotBothFitRunOneAtATime(t *testing.T) {
	for _, manager := range []bool{false, true} {
		b := budgetOf(6 << 30)
		a, w := budgetProject(t, "CSR", b), budgetProject(t, "WEB", b)
		ga, gw := newGate("/runs/CSR-1", "/runs/CSR-2"), newGate("/runs/CSR-1")
		a.p.Clients["claude"], w.p.Clients["claude"] = ga, gw
		dir, job := "/runs/CSR-1", jobJSON
		if manager {
			a.ws.Prepares = a.ws.Prepares[1:]
			dir, job = "/runs/CSR-2", managerJobJSON
		}

		var mu sync.Mutex
		var phase int // 0: CSR waits for its job; 1: CSR's run goes; 2: CSR's run ended; 3: WEB's run goes
		var csrWhile, webWhile []cinderline.PollRequest
		get := func() int { mu.Lock(); defer mu.Unlock(); return phase }
		appA := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew}, "CSR-2": {managerRun}})}
		appA.onPoll = func(_ int, req cinderline.PollRequest) reply {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case phase == 0 && (manager && req.ManagerIdle || !manager && req.Idle):
				phase = 1
				return answer(t, proj, job)
			case phase == 1 && ga.isStarted(dir):
				csrWhile = append(csrWhile, req)
			case phase == 3:
				csrWhile = append(csrWhile, req)
			}
			return answer(t, proj, "")
		}
		appW := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew}})}
		appW.onPoll = func(_ int, req cinderline.PollRequest) reply {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case phase == 1 && ga.isStarted(dir):
				webWhile = append(webWhile, req)
			case phase == 2 && req.Idle:
				phase = 3
				return answer(t, proj, jobJSON)
			}
			return answer(t, proj, "")
		}
		ctx, cancel := context.WithCancel(context.Background())
		doneA, doneW := loopIn(ctx, t, a, appA), loopIn(ctx, t, w, appW)
		until(t, "CSR's run", func() bool { return ga.isStarted(dir) })
		until(t, "polls of both during CSR's run", func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(csrWhile) >= 3 && len(webWhile) >= 3
		})
		mu.Lock()
		for _, req := range webWhile {
			if req.Idle || req.ManagerIdle {
				t.Errorf("manager %v: WEB offered a slot during CSR's run: idle %v, manager_idle %v", manager, req.Idle, req.ManagerIdle)
			}
		}
		for _, req := range csrWhile {
			if req.Idle || req.ManagerIdle {
				t.Errorf("manager %v: CSR offered its other slot next to its own run: idle %v, manager_idle %v", manager, req.Idle, req.ManagerIdle)
			}
		}
		csrWhile = nil
		phase = 2
		mu.Unlock()
		if st := w.p.Status(); !st.WaitsForMemory {
			t.Errorf("manager %v: WEB's status during CSR's run: %+v, want it waiting for memory", manager, st)
		}
		close(ga.release[dir])
		until(t, "WEB's run", func() bool { return gw.isStarted("/runs/CSR-1") })
		until(t, "polls of CSR during WEB's run", func() bool { mu.Lock(); defer mu.Unlock(); return len(csrWhile) >= 3 })
		mu.Lock()
		for _, req := range csrWhile {
			if req.Idle || req.ManagerIdle {
				t.Errorf("manager %v: CSR offered a slot during WEB's run", manager)
			}
		}
		mu.Unlock()
		if got := reserved(b); got != 4<<30 {
			t.Errorf("manager %v: reserved during WEB's run %d, want its 4 GiB", manager, got)
		}
		close(gw.release["/runs/CSR-1"])
		until(t, "WEB's run to end", func() bool { return len(w.p.Status().Issues) == 0 && get() == 3 })
		cancel()
		<-doneA
		<-doneW
		if got := reserved(b); got != 0 {
			t.Errorf("manager %v: reserved after both runs %d, want 0", manager, got)
		}
	}
}

// A slot reserves its run's memory when it is offered and frees it when the
// answer brings no job. Where only one run fits, the team's and the
// manager's slots take turns at being offered.
func TestReservationReleasedWhenNoJobComes(t *testing.T) {
	b := budgetOf(6 << 30)
	f := budgetProject(t, "CSR", b)
	var during []int64
	polls := runLoop(t, f, &fakeApp{}, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		during = append(during, reserved(b))
		if n == 4 {
			stop()
		}
		return answer(t, proj, "")
	})
	for i, r := range during {
		if r != 4<<30 {
			t.Errorf("poll %d: reserved %d, want one slot's 4 GiB", i, r)
		}
	}
	var offered [][2]bool
	for _, p := range polls {
		offered = append(offered, [2]bool{p.Idle, p.ManagerIdle})
	}
	if want := [][2]bool{{true, false}, {false, true}, {true, false}, {false, true}, {true, false}}; !slices.Equal(offered, want) {
		t.Errorf("idle, manager_idle per poll = %v, want the slots taking turns %v", offered, want)
	}
	if got := reserved(b); got != 0 {
		t.Errorf("reserved after the loop = %d, want 0", got)
	}
	if st := f.p.Status(); st.WaitsForMemory {
		t.Error("a slot only waiting its turn shows as waiting for memory")
	}
}

// A run larger than the whole budget is offered only while nothing else
// holds memory, and runs alone with a warning.
func TestOversizedRunStartsAlone(t *testing.T) {
	b := budgetOf(2 << 30)
	f := budgetProject(t, "CSR", b)
	var logs bytes.Buffer
	var logMu sync.Mutex
	f.p.Log = slog.New(slog.NewTextHandler(&lockedWriter{w: &logs, mu: &logMu}, nil))
	g := newGate("/runs/CSR-1")
	f.p.Clients["claude"] = g
	if ok, _ := b.reserve(context.Background(), "other", 1<<30); !ok {
		t.Fatal("other not reserved")
	}
	app := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew}})}
	var blocked []cinderline.PollRequest
	var while []cinderline.PollRequest
	phase := 0
	runLoop(t, f, app, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		switch {
		case phase == 0:
			blocked = append(blocked, req)
			if len(blocked) == 3 {
				phase = 1
				b.release("other")
			}
		case phase == 1 && req.Idle:
			phase = 2
			return answer(t, proj, jobJSON)
		case phase == 2 && g.isStarted("/runs/CSR-1"):
			if ok, _ := b.reserve(context.Background(), "other", 1<<20); ok {
				t.Error("another slot reserved next to the oversized run")
			}
			while = append(while, req)
			if len(while) == 2 {
				phase = 3
				close(g.release["/runs/CSR-1"])
			}
		case phase == 3 && len(req.Running) == 0:
			stop()
		}
		return answer(t, proj, "")
	})
	if phase != 3 {
		t.Fatalf("stopped in phase %d", phase)
	}
	for _, req := range append(blocked, while...) {
		if req.Idle || req.ManagerIdle {
			t.Errorf("offered a slot next to other memory: idle %v, manager_idle %v", req.Idle, req.ManagerIdle)
		}
	}
	logMu.Lock()
	defer logMu.Unlock()
	// The team's slot starts to wait once; the manager's may start again
	// when the run takes the memory, after a poll where it only had its turn.
	if s := logs.String(); !strings.Contains(s, "runs alone") || strings.Count(s, `msg="waits for memory" project=CSR manager=false`) != 1 ||
		!slices.Contains([]int{1, 2}, strings.Count(s, `msg="waits for memory" project=CSR manager=true`)) {
		t.Errorf("log = %s, want one warning that the run runs alone and a line each time a slot starts to wait", s)
	}
}

// A run whose prepare finds the base's compose file grown past what is left
// of the budget drops its job before claiming it, and the slot is not
// offered again until its new size fits; the status shows the new size.
func TestRunThatGrewPastTheBudgetDropsItsJob(t *testing.T) {
	b := budgetOf(6 << 30)
	f := budgetProject(t, "CSR", b)
	f.p.prepare = func(context.Context, cinderline.Project, string) (int64, error) { return 5 << 30, nil }
	if ok, _ := b.reserve(context.Background(), "other", 2<<30); !ok {
		t.Fatal("other not reserved")
	}
	app := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew}})}
	var after []cinderline.PollRequest
	phase := 0
	runLoop(t, f, app, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		switch {
		case phase == 0 && req.Idle:
			phase = 1
			return answer(t, proj, jobJSON)
		case phase == 1:
			after = append(after, req)
			if len(after) == 3 {
				stop()
			}
		}
		return answer(t, proj, "")
	})
	if len(app.requests()) != 0 {
		t.Errorf("the dropped job called next %d times", len(app.requests()))
	}
	for _, req := range after {
		if req.Idle || req.ManagerIdle || len(req.Report.Errors) != 0 {
			t.Errorf("poll after the drop: idle %v, manager_idle %v, errors %v; want no slot offered and no error", req.Idle, req.ManagerIdle, req.Report.Errors)
		}
	}
	if st := f.p.Status(); st.RunMemory != 5<<30 || !st.WaitsForMemory {
		t.Errorf("status = %+v, want run memory 5 GiB and waiting", st)
	}
	if got := reserved(b); got != 2<<30 {
		t.Errorf("reserved = %d, want only the other slot's 2 GiB", got)
	}
}

// Without Docker's memory no slot is offered, and the status says why.
func TestNoSlotWithoutDockersMemory(t *testing.T) {
	f := budgetProject(t, "CSR", NewBudget(func(context.Context) (int64, error) { return 0, errors.New("docker is not running") }))
	polls := runLoop(t, f, &fakeApp{}, nil, nil, stopAt(2, answer(t, proj, "")))
	for _, req := range polls {
		if req.Idle || req.ManagerIdle {
			t.Errorf("offered a slot without Docker's memory: %v, %v", req.Idle, req.ManagerIdle)
		}
	}
	if st := f.p.Status(); !st.WaitsForMemory {
		t.Errorf("status = %+v, want waiting for memory", st)
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}
