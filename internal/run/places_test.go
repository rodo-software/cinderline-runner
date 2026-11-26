package run

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
)

// CLR-253, decision 2026-11-25-provider-places in the app repository: the
// runner counts the runs in play on each provider with a limit and tells
// the app in each poll that offers a slot how many more it may start.

// placesIn are a poll's places by provider; nil when it sent none.
func placesIn(req cinderline.PollRequest) map[string]int {
	if req.ProviderPlaces == nil {
		return nil
	}
	out := map[string]int{}
	for _, pl := range req.ProviderPlaces {
		out[pl.Provider] = pl.Free
	}
	return out
}

// placesProject is a fixture keyed key on providers, sharing turns, whose
// Claude Code and OpenCode turns wait at g.
func placesProject(t *testing.T, key string, turns *ProviderTurns, providers []config.Provider, g *gate) *fixture {
	f := newFixture(t)
	f.p.Config.Key, f.p.Turns, f.p.Providers = key, turns, providers
	f.p.Clients["claude"], f.p.Clients["opencode"] = g, g
	return f
}

// jobOn is the poll answer's job for CSR-1, on provider ("": none), with
// its agent on client and model.
func jobOn(provider, client, model string) string {
	extra := ""
	if model != "" {
		extra = `,"model":"` + model + `"`
	}
	p := "null"
	if provider != "" {
		p = `"` + provider + `"`
	}
	return `"job":{"issue":{"ref":"CSR-1","title":"Fix it","body":"Please."},"team":{"name":"Dev","max_minutes":10,"max_turns":50,"agents":[{"name":"Coder","client":"` + client + `"` + extra + `}]},"provider":` + p + `}`
}

// noneLeft fails when turns still holds a run in play or a reserved place.
func noneLeft(t *testing.T, turns *ProviderTurns) {
	t.Helper()
	turns.mu.Lock()
	defer turns.mu.Unlock()
	if len(turns.inPlay) != 0 || len(turns.reserved) != 0 {
		t.Errorf("in play %v, reserved %v; want none", turns.inPlay, turns.reserved)
	}
}

// Each poll that offers a slot sends the free places of the providers with
// a limit, max_agents + lookahead less the runs in play; a provider
// without one is not listed. A started job that names a provider is in
// play on it at once, and when it ends its place is free again.
func TestThePollReportsProviderPlaces(t *testing.T) {
	turns := &ProviderTurns{}
	g := newGate("/runs/CSR-1")
	f := placesProject(t, "CSR", turns, []config.Provider{{Name: "lan", MaxAgents: 1}, {Name: "ahead", MaxAgents: 1, Lookahead: 1}, {Name: "open"}}, g)
	app := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew}})}
	var during []cinderline.PollRequest
	var after map[string]int
	phase := 0
	polls := runLoop(t, f, app, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		switch {
		case phase == 0 && req.Idle:
			phase = 1
			return answer(t, proj, jobOn("ahead", "claude", ""))
		case phase == 1 && g.isStarted("/runs/CSR-1"):
			if during = append(during, req); len(during) == 2 {
				phase = 2
				close(g.release["/runs/CSR-1"])
			}
		case phase == 2 && len(req.Running) == 0:
			after = placesIn(req)
			stop()
		}
		return answer(t, proj, "")
	})
	if got, want := placesIn(polls[0]), map[string]int{"lan": 1, "ahead": 2}; !maps.Equal(got, want) {
		t.Errorf("idle: places %v, want %v", got, want)
	}
	for _, req := range during {
		if got, want := placesIn(req), map[string]int{"lan": 1, "ahead": 1}; !req.ManagerIdle || !maps.Equal(got, want) {
			t.Errorf("during the run on ahead: manager_idle %v, places %v; want the manager's slot offered with %v", req.ManagerIdle, got, want)
		}
	}
	if want := map[string]int{"lan": 1, "ahead": 2}; !maps.Equal(after, want) {
		t.Errorf("after the run: places %v, want %v", after, want)
	}
	noneLeft(t, turns)
}

// Without a provider with a limit, and in a poll that offers no slot, the
// poll sends no places and reserves none.
func TestNoProviderPlacesWithoutALimitOrASlot(t *testing.T) {
	turns := &ProviderTurns{}
	f := placesProject(t, "CSR", turns, []config.Provider{{Name: "open"}}, newGate())
	for _, req := range runLoop(t, f, &fakeApp{}, nil, nil, stopAt(1, answer(t, proj, ""))) {
		if !req.Idle || req.ProviderPlaces != nil {
			t.Errorf("idle %v, places %v; want a slot and no places", req.Idle, req.ProviderPlaces)
		}
	}
	f = placesProject(t, "CSR", turns, []config.Provider{{Name: "lan", MaxAgents: 1}}, newGate())
	f.p.clients = nil // detection has no result yet: no slot is offered
	for _, req := range runLoop(t, f, &fakeApp{}, nil, nil, stopAt(1, answer(t, proj, ""))) {
		if req.Idle || req.ManagerIdle || req.ProviderPlaces != nil {
			t.Errorf("idle %v, manager_idle %v, places %v; want neither slot and no places", req.Idle, req.ManagerIdle, req.ProviderPlaces)
		}
	}
	noneLeft(t, turns)
}

// An answer without a job, and a failed poll, give every reserved place back.
func TestPlacesGivenBackWithoutAJob(t *testing.T) {
	turns := &ProviderTurns{}
	f := placesProject(t, "CSR", turns, []config.Provider{{Name: "lan", MaxAgents: 2}}, newGate())
	var held []map[string]int
	runLoop(t, f, &fakeApp{}, nil, nil, func(n int, _ cinderline.PollRequest, stop func()) reply {
		turns.mu.Lock()
		held = append(held, maps.Clone(turns.reserved["CSR"]))
		turns.mu.Unlock()
		if n == 1 {
			// A place is in one poll at a time: another project's poll next to CSR's gets none.
			if got := placesOf(turns, "WEB", f.p.Providers)["lan"]; got != 0 {
				t.Errorf("WEB next to CSR's outstanding poll: %d, want 0", got)
			}
			turns.unreserve("WEB")
		}
		if n == 2 {
			stop()
			return reply{500, `{"message":"down"}`}
		}
		return answer(t, proj, "")
	})
	for i, h := range held {
		if !maps.Equal(h, map[string]int{"lan": 2}) {
			t.Errorf("poll %d held %v, want both places on lan", i, h)
		}
	}
	noneLeft(t, turns)
}

// pollLog records a loop's polls; answerOf answers each.
type pollLog struct {
	mu    sync.Mutex
	polls []cinderline.PollRequest
}

func (l *pollLog) since(n int) []cinderline.PollRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]cinderline.PollRequest{}, l.polls[min(n, len(l.polls)):]...)
}

func (l *pollLog) count() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.polls) }

// record makes app record its polls in l and answer the nth with answerOf.
func (l *pollLog) record(app *fakeApp, answerOf func(n int) reply) {
	app.onPoll = func(n int, req cinderline.PollRequest) reply {
		l.mu.Lock()
		l.polls = append(l.polls, req)
		l.mu.Unlock()
		return answerOf(n)
	}
}

// waitPolls waits for n more polls after the first from and returns them.
func (l *pollLog) waitPolls(t *testing.T, from, n int) []cinderline.PollRequest {
	t.Helper()
	until(t, "polls", func() bool { return l.count() >= from+n })
	return l.since(from)
}

// A run whose first turn is on a provider that is full (an app that sent
// no provider) still enters play and waits for its turn as before; the
// polls report 0, and log it once, until enough runs have ended.
func TestARunOnAFullProviderEntersPlayAndWaits(t *testing.T) {
	turns := &ProviderTurns{}
	g := newGate("/runs/CSR-1")
	providers := []config.Provider{{Name: "lan", MaxAgents: 1}}
	f := placesProject(t, "CSR", turns, providers, g)
	var logs bytes.Buffer
	var logMu sync.Mutex
	f.p.Log = slog.New(slog.NewTextHandler(&lockedWriter{w: &logs, mu: &logMu}, nil))
	if err := turns.take(context.Background(), "elsewhere", "lan", 1, nil); err != nil { // another project's turn
		t.Fatal(err)
	}
	app := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew}})}
	var polls pollLog
	polls.record(app, func(n int) reply {
		if n == 0 {
			return answer(t, proj, jobOn("", "opencode", "lan/qwen3"))
		}
		return answer(t, proj, "")
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := loopIn(ctx, t, f, app)
	defer func() { cancel(); <-done }()

	waitForStep(t, f.p, "CSR-1")
	until(t, "CSR-1 in play", func() bool { turns.mu.Lock(); defer turns.mu.Unlock(); return turns.inPlay["lan"][f.p.slot(0)] })
	zero := func(what string, reqs []cinderline.PollRequest) {
		t.Helper()
		for _, req := range reqs {
			if got := placesIn(req); !maps.Equal(got, map[string]int{"lan": 0}) {
				t.Errorf("%s: places %v, want lan 0", what, got)
			}
		}
	}
	zero("both runs in play, CSR-1's turn waiting", polls.waitPolls(t, 0, 3))
	if g.isStarted("/runs/CSR-1") {
		t.Fatal("CSR-1's turn started on a full provider")
	}
	turns.done("lan") // the other turn ends; its run goes on
	until(t, "CSR-1's turn", func() bool { return g.isStarted("/runs/CSR-1") })
	zero("CSR-1's turn going", polls.waitPolls(t, polls.count(), 2))
	turns.leave("elsewhere", providers)
	zero("the other run ended, CSR-1 in play", polls.waitPolls(t, polls.count(), 2))
	close(g.release["/runs/CSR-1"])
	until(t, "a poll after CSR-1's run", func() bool {
		for _, req := range polls.since(0) {
			if len(req.Running) == 0 && maps.Equal(placesIn(req), map[string]int{"lan": 1}) {
				return true
			}
		}
		return false
	})
	logMu.Lock()
	defer logMu.Unlock()
	if n := strings.Count(logs.String(), "no free place on model provider"); n != 1 {
		t.Errorf("logged %d times that lan is full, want once:\n%s", n, logs.String())
	}
}

// Two projects polling at the same moment never report the same place:
// what they report together is at most the provider's free places.
func TestTwoLoopsNeverReportTheSamePlace(t *testing.T) {
	turns := &ProviderTurns{}
	providers := []config.Provider{{Name: "lan", MaxAgents: 1, Lookahead: 1}}
	var both sync.WaitGroup
	both.Add(2)
	var mu sync.Mutex
	var reported []int
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dones []chan struct{}
	for _, key := range []string{"CSR", "WEB"} {
		f := placesProject(t, key, turns, providers, newGate())
		app := &fakeApp{}
		app.onPoll = func(n int, req cinderline.PollRequest) reply {
			if n == 0 {
				mu.Lock()
				reported = append(reported, placesIn(req)["lan"])
				mu.Unlock()
				both.Done()
				both.Wait() // both polls are out at once
			}
			return answer(t, proj, "")
		}
		dones = append(dones, loopIn(ctx, t, f, app))
	}
	until(t, "both polls", func() bool { mu.Lock(); defer mu.Unlock(); return len(reported) == 2 })
	cancel()
	for _, d := range dones {
		<-d
	}
	if reported[0]+reported[1] != 2 {
		t.Errorf("reported %v, want 2 places between them", reported)
	}
	noneLeft(t, turns)
}

// A run that ends frees its place, and another project's loop polls at
// once, not after its poll_after.
func TestAFreedPlaceWakesTheOtherLoops(t *testing.T) {
	waitFor = func(d time.Duration) time.Duration { return d } // the answers' 30 seconds
	t.Cleanup(func() { waitFor = func(time.Duration) time.Duration { return time.Millisecond } })
	turns := &ProviderTurns{}
	providers := []config.Provider{{Name: "lan", MaxAgents: 1}}
	g := newGate("/runs/CSR-1")
	a, w := placesProject(t, "CSR", turns, providers, g), placesProject(t, "WEB", turns, providers, newGate())
	appA := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew}})}
	appA.onPoll = func(n int, _ cinderline.PollRequest) reply {
		if n == 0 {
			return answer(t, proj, jobOn("lan", "claude", ""))
		}
		return answer(t, proj, "")
	}
	appW := &fakeApp{}
	var polls pollLog
	polls.record(appW, func(int) reply { return answer(t, proj, "") })
	// CSR drains, so it offers no slot after its run and WEB alone polls for the place.
	drain := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	c := appA.client(t)
	doneA := make(chan struct{})
	go func() { a.p.Loop(ctx, c, drain); close(doneA) }()
	defer func() { cancel(); <-doneA }()
	until(t, "CSR's run", func() bool { return g.isStarted("/runs/CSR-1") })
	close(drain)
	doneW := loopIn(ctx, t, w, appW)
	defer func() { cancel(); <-doneW }()
	if first := polls.waitPolls(t, 0, 1)[0]; !maps.Equal(placesIn(first), map[string]int{"lan": 0}) {
		t.Errorf("WEB during CSR's run: places %v, want lan 0", placesIn(first))
	}
	close(g.release["/runs/CSR-1"])
	start := time.Now()
	next := polls.waitPolls(t, 1, 1)[0] // until gives up after 10 seconds, before poll_after
	if !maps.Equal(placesIn(next), map[string]int{"lan": 1}) || time.Since(start) > 5*time.Second {
		t.Errorf("WEB after CSR's run, %v later: places %v, want lan 1 at once", time.Since(start), placesIn(next))
	}
}

// A reload keeps the count: a run of the older config stays in play while
// it finishes, and a project's loop on the new config counts it.
func TestAReloadKeepsTheCount(t *testing.T) {
	turns := &ProviderTurns{}
	g := newGate("/runs/CSR-1")
	a := placesProject(t, "CSR", turns, []config.Provider{{Name: "lan", MaxAgents: 1}}, g)
	appA := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew}})}
	appA.onPoll = func(n int, _ cinderline.PollRequest) reply {
		if n == 0 {
			return answer(t, proj, jobOn("lan", "claude", ""))
		}
		return answer(t, proj, "")
	}
	drain := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := appA.client(t)
	doneA := make(chan struct{})
	go func() { a.p.Loop(ctx, c, drain); close(doneA) }()
	until(t, "CSR's run", func() bool { return g.isStarted("/runs/CSR-1") })
	close(drain) // the reload: CSR's loop finishes its run on the older config

	// WEB starts again at once on the new config, which raised lan's limit.
	w := placesProject(t, "WEB", turns, []config.Provider{{Name: "lan", MaxAgents: 2}}, newGate())
	appW := &fakeApp{}
	var polls pollLog
	polls.record(appW, func(int) reply { return answer(t, proj, "") })
	doneW := loopIn(ctx, t, w, appW)
	for _, req := range polls.waitPolls(t, 0, 2) {
		if got := placesIn(req); !maps.Equal(got, map[string]int{"lan": 1}) {
			t.Errorf("WEB on the new config next to CSR's run: places %v, want lan 1", got)
		}
	}
	close(g.release["/runs/CSR-1"])
	<-doneA
	cancel()
	<-doneW
	noneLeft(t, turns)
}
