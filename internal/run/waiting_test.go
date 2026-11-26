package run

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// CLR-254, decision 2026-11-25-provider-places in the app repository: the
// poll's waiting names the runs whose turn waits now for a place on a
// model provider; left out, none waits.

// lanJob is a solo job on ref whose agent runs OpenCode on the provider lan.
func lanJob(ref string) string {
	return `{"issue":{"ref":"` + ref + `","title":"Fix it","body":"Please."},"team":{"name":"Dev","max_minutes":10,"max_turns":50,"agents":[{"name":"Coder","client":"opencode","model":"lan/qwen3"}]},"provider":null}`
}

// waitsFor is the provider run ref's turn waits for, as the project keeps it; "": none.
func waitsFor(p *Project, ref string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waits[ref]
}

// waitingOn is waiting with one run, ref, on lan.
func waitingOn(ref string) []cinderline.WaitingRun {
	return []cinderline.WaitingRun{{Ref: ref, Provider: "lan"}}
}

// With max_agents 1 and two runs on the provider, the poll after the
// second run's turn starts to wait names it, and only it; the first poll
// after its wait ends names none.
func TestThePollReportsTheRunWaitingForItsProvider(t *testing.T) {
	turns := &ProviderTurns{}
	f := placesProject(t, "CSR", turns, []config.Provider{{Name: "lan", MaxAgents: 1}}, newGate("/runs/CSR-1", "/runs/CSR-2"))
	g := f.p.Clients["opencode"].(*gate)
	f.p.Config.MaxParallelRuns = 2
	f.ws.Prepares = []workspace.PrepareReply{{Dir: "/runs/CSR-1"}, {Dir: "/runs/CSR-2"}}
	app := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runOn("CSR-1")}, "CSR-2": {runOn("CSR-2")}})}
	var before, waiting, after []cinderline.PollRequest
	phase := 0
	runLoop(t, f, app, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		switch phase {
		case 0:
			phase = 1
			return answer(t, proj, `"job":null,"jobs":[`+lanJob("CSR-1")+`]`)
		case 1:
			if g.isStarted("/runs/CSR-1") {
				phase = 2
				return answer(t, proj, `"job":null,"jobs":[`+lanJob("CSR-2")+`]`)
			}
		case 2:
			before = append(before, req) // CSR-2's turn may not wait yet
			if waitsFor(f.p, "CSR-2") != "" {
				phase = 3 // the next poll is built after its wait began
			}
		case 3:
			waiting = append(waiting, req)
			phase = 4
			close(g.release["/runs/CSR-1"])
		case 4:
			if g.isStarted("/runs/CSR-2") {
				phase = 5 // the next poll is built after its wait ended
			}
		case 5:
			after = append(after, req)
			phase = 6
			close(g.release["/runs/CSR-2"])
		case 6:
			if len(req.Running) == 0 {
				stop()
			}
		}
		return answer(t, proj, `"job":null,"jobs":[]`)
	})
	if phase != 6 {
		t.Fatalf("stopped in phase %d", phase)
	}
	for _, req := range before {
		if req.Waiting != nil && !slices.Equal(req.Waiting, waitingOn("CSR-2")) {
			t.Errorf("before CSR-2 waits: waiting %+v, want none or CSR-2 alone", req.Waiting)
		}
	}
	if !slices.Equal(waiting[0].Waiting, waitingOn("CSR-2")) || len(waiting[0].Running) != 2 {
		t.Errorf("while CSR-2's turn waits: waiting %+v, running %v; want CSR-2 alone on lan, both running", waiting[0].Waiting, waiting[0].Running)
	}
	if after[0].Waiting != nil {
		t.Errorf("after CSR-2's wait: waiting %+v, want none", after[0].Waiting)
	}
	if b, err := json.Marshal(after[0]); err != nil || strings.Contains(string(b), `"waiting"`) {
		t.Errorf("a poll with no run waiting sends the field (%v): %s", err, b)
	}
}

// A waiting turn that the poll's stop ends is no longer reported.
func TestAStoppedWaitIsNoLongerReported(t *testing.T) {
	turns := &ProviderTurns{}
	f := placesProject(t, "CSR", turns, []config.Provider{{Name: "lan", MaxAgents: 1}}, newGate("/runs/CSR-1"))
	if err := turns.take(context.Background(), "elsewhere", "lan", 1, nil); err != nil { // another project's turn
		t.Fatal(err)
	}
	app := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew}})}
	var after []cinderline.PollRequest
	phase := 0
	runLoop(t, f, app, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		switch phase {
		case 0:
			phase = 1
			return answer(t, proj, `"job":`+lanJob("CSR-1"))
		case 1:
			if slices.Equal(req.Waiting, waitingOn("CSR-1")) {
				phase = 2
				return answer(t, proj, `"stop":["CSR-1"]`)
			}
		case 2:
			if waitsFor(f.p, "CSR-1") == "" {
				phase = 3 // the next poll is built after the wait ended
			}
		case 3:
			if after = append(after, req); len(after) == 3 {
				stop()
			}
		}
		return answer(t, proj, "")
	})
	if phase != 3 {
		t.Fatalf("stopped in phase %d", phase)
	}
	for _, req := range after {
		if req.Waiting != nil {
			t.Errorf("after the stop: waiting %+v, want none", req.Waiting)
		}
	}
	if lasts := app.lasts(); len(lasts) == 0 || lasts[0].Outcome != "stopped" {
		t.Errorf("lasts %+v, want the turn stopped", lasts)
	}
}

// Two projects on one provider: each project's poll names only its own
// waiting runs.
func TestEachProjectReportsItsOwnWaitingRuns(t *testing.T) {
	turns := &ProviderTurns{}
	providers := []config.Provider{{Name: "lan", MaxAgents: 1}}
	gw := newGate("/runs/CSR-1")
	w := placesProject(t, "WEB", turns, providers, gw)
	a := placesProject(t, "CSR", turns, providers, newGate("/runs/CSR-1"))
	job := func(polls *pollLog, app *fakeApp) {
		polls.record(app, func(n int) reply {
			if n == 0 {
				return answer(t, proj, `"job":`+lanJob("CSR-1"))
			}
			return answer(t, proj, "")
		})
	}
	appW := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew}})}
	appA := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew}})}
	var pollsW, pollsA pollLog
	job(&pollsW, appW)
	job(&pollsA, appA)
	ctx, cancel := context.WithCancel(context.Background())
	doneW := loopIn(ctx, t, w, appW)
	defer func() { cancel(); <-doneW }()
	until(t, "WEB's turn", func() bool { return gw.isStarted("/runs/CSR-1") })
	doneA := loopIn(ctx, t, a, appA)
	defer func() { cancel(); <-doneA }()
	until(t, "CSR's turn waits", func() bool { return waitsFor(a.p, "CSR-1") == "lan" })

	// A poll counted now may have been built before the wait; the next is built after it.
	for _, req := range pollsA.waitPolls(t, pollsA.count()+1, 2) {
		if !slices.Equal(req.Waiting, waitingOn("CSR-1")) {
			t.Errorf("CSR while its turn waits: waiting %+v, want CSR-1 on lan", req.Waiting)
		}
	}
	for _, req := range pollsW.waitPolls(t, 0, pollsW.count()+2) {
		if req.Waiting != nil {
			t.Errorf("WEB, whose turn goes, next to CSR's waiting turn: waiting %+v, want none", req.Waiting)
		}
	}
	close(gw.release["/runs/CSR-1"])
}

// A turn that starts or stops waiting makes its project's loop poll at
// once, not after its poll_after.
func TestAWaitStartAndEndPollAtOnce(t *testing.T) {
	waitFor = func(d time.Duration) time.Duration { return d } // the answers' 30 seconds
	t.Cleanup(func() { waitFor = func(time.Duration) time.Duration { return time.Millisecond } })
	turns := &ProviderTurns{}
	g := newGate("/runs/CSR-1")
	f := placesProject(t, "CSR", turns, []config.Provider{{Name: "lan", MaxAgents: 1}}, g)
	if err := turns.take(context.Background(), "elsewhere", "lan", 1, nil); err != nil { // another project's turn
		t.Fatal(err)
	}
	app := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew}})}
	var polls pollLog
	polls.record(app, func(n int) reply {
		if n == 0 {
			return answer(t, proj, `"job":`+lanJob("CSR-1"))
		}
		return answer(t, proj, "")
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := loopIn(ctx, t, f, app)
	defer func() { cancel(); <-done }()

	// until gives up after 10 seconds, before poll_after.
	if next := polls.waitPolls(t, 0, 2)[1]; !slices.Equal(next.Waiting, waitingOn("CSR-1")) {
		t.Errorf("the poll when the wait began: waiting %+v, want CSR-1 on lan", next.Waiting)
	}
	turns.done("lan") // the other turn ends: CSR-1's turn takes its place
	if next := polls.waitPolls(t, 2, 1)[0]; next.Waiting != nil {
		t.Errorf("the poll when the wait ended: waiting %+v, want none", next.Waiting)
	}
	until(t, "CSR-1's turn", func() bool { return g.isStarted("/runs/CSR-1") })
	close(g.release["/runs/CSR-1"])
}
