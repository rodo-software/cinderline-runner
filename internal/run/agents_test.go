package run

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
)

// CLR-243: a provider's max_agents holds every turn past it until one on
// that provider ends, over every project; other providers and a provider
// without a limit never wait.
func TestProviderTurnsLimit(t *testing.T) {
	var turns ProviderTurns
	ctx := context.Background()
	waits := 0
	waiting := func() { waits++ }
	for range 2 {
		if err := turns.take(ctx, "CSR", "lan", 2, waiting); err != nil {
			t.Fatal(err)
		}
	}
	if err := turns.take(ctx, "CSR", "other", 1, waiting); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := turns.take(ctx, "CSR", "open", 0, waiting); err != nil {
			t.Fatal(err)
		}
	}
	if waits != 0 {
		t.Fatalf("waited %d times below the limits", waits)
	}

	got := make(chan error, 1)
	go func() { got <- turns.take(ctx, "CSR", "lan", 2, waiting) }()
	select {
	case err := <-got:
		t.Fatalf("a third turn on lan started (%v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	turns.done("other") // another provider's turn frees no place on lan
	select {
	case err := <-got:
		t.Fatalf("a third turn on lan started after another provider's ended (%v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	turns.done("lan")
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	if waits != 1 {
		t.Errorf("waiting called %d times, want once", waits)
	}
	if n := turns.busy["lan"]; n != 2 {
		t.Errorf("lan busy %d, want 2", n)
	}
}

// A limit lowered by a reload holds new turns until the turns in flight are
// under it; a cancelled wait takes no place and says why.
func TestProviderTurnsLowerLimitAndCancel(t *testing.T) {
	var turns ProviderTurns
	for range 2 {
		if err := turns.take(context.Background(), "CSR", "lan", 2, nil); err != nil {
			t.Fatal(err)
		}
	}
	got := make(chan error, 1)
	go func() { got <- turns.take(context.Background(), "CSR", "lan", 1, nil) }()
	turns.done("lan")
	select {
	case err := <-got:
		t.Fatalf("a turn started with 1 in flight and a limit of 1 (%v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	turns.done("lan")
	if err := <-got; err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	why := errors.New("stopped")
	go func() { got <- turns.take(ctx, "CSR", "lan", 1, nil) }()
	time.Sleep(20 * time.Millisecond)
	cancel(why)
	if err := <-got; !errors.Is(err, why) {
		t.Errorf("err = %v, want the cause of the cancel", err)
	}
	if n := turns.busy["lan"]; n != 1 {
		t.Errorf("lan busy %d after a cancelled wait, want 1", n)
	}
}

// providerFixture is a project whose OpenCode agent Coder runs on the
// runner's provider lan, at most one turn at a time, with one turn on lan
// held elsewhere.
func providerFixture(t *testing.T, model string) (*fixture, cinderline.Job) {
	t.Helper()
	f := newFixture(t)
	f.p.Clients["opencode"] = f.cl
	f.p.Providers = []config.Provider{{Name: "lan", BaseURL: "http://studio.local:8080/v1", MaxAgents: 1}}
	f.p.Turns = &ProviderTurns{}
	if err := f.p.Turns.take(context.Background(), "elsewhere", "lan", 1, nil); err != nil {
		t.Fatal(err)
	}
	jb := soloJob()
	jb.Team.Agents[0].Client, jb.Team.Agents[0].Model = "opencode", &model
	return f, jb
}

// waitForStep waits until run ref has a step in flight that the poll's stop can end.
func waitForStep(t *testing.T, p *Project, ref string) {
	t.Helper()
	for range 500 {
		p.mu.Lock()
		in := p.stopRuns[ref] != nil
		p.mu.Unlock()
		if in {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no step in flight")
}

// CLR-243: a turn on a provider at its max_agents waits for a place and
// starts once another turn on it ends; it then holds the place until it ends.
func TestATurnWaitsForItsProvider(t *testing.T) {
	f, jb := providerFixture(t, "lan/qwen3")
	f.cl.Runs = []client.RunReply{ok("s1", "Done.")}
	app := &fakeApp{onNext: stepsOf(runNew)}
	c := app.client(t)
	done := make(chan error, 1)
	go func() { _, err := f.p.run(context.Background(), c, proj, jb, 0); done <- err }()
	waitForStep(t, f.p, "CSR-1")
	time.Sleep(50 * time.Millisecond)
	if n := len(f.cl.Calls()); n != 0 {
		t.Fatalf("%d turns started on a full provider", n)
	}
	f.p.Turns.done("lan")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if lasts := app.lasts(); len(f.cl.Calls()) != 1 || len(lasts) != 1 || lasts[0].Outcome != "finished" {
		t.Fatalf("calls %d, lasts %+v", len(f.cl.Calls()), lasts)
	}
	if n := f.p.Turns.busy["lan"]; n != 0 {
		t.Errorf("lan busy %d after the turn, want 0", n)
	}
}

// Only a turn on the runner's own provider waits: OpenRouter's, an unknown
// provider's and another client's do not.
func TestOtherTurnsDoNotWaitForAProvider(t *testing.T) {
	for _, tc := range []struct{ client, model string }{
		{"opencode", "openrouter/qwen/qwen3"},
		{"opencode", "qwen3"},
		{"opencode", "elsewhere/qwen3"},
		{"claude", "lan/qwen3"},
	} {
		f, jb := providerFixture(t, tc.model)
		jb.Team.Agents[0].Client = tc.client
		f.cl.Runs = []client.RunReply{ok("s1", "Done.")}
		if _, err := f.p.run(context.Background(), (&fakeApp{onNext: stepsOf(runNew)}).client(t), proj, jb, 0); err != nil {
			t.Fatal(err)
		}
		if len(f.cl.Calls()) != 1 {
			t.Errorf("%s %s: the turn did not run", tc.client, tc.model)
		}
	}
}

// The poll's stop ends a turn that waits for its provider as stopped,
// without running it or taking a place.
func TestAStopEndsATurnWaitingForItsProvider(t *testing.T) {
	f, jb := providerFixture(t, "lan/qwen3")
	app := &fakeApp{onNext: stepsOf(runNew)}
	c := app.client(t)
	done := make(chan error, 1)
	go func() { _, err := f.p.run(context.Background(), c, proj, jb, 0); done <- err }()
	waitForStep(t, f.p, "CSR-1")
	f.p.stopTurn("CSR-1")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if lasts := app.lasts(); len(f.cl.Calls()) != 0 || len(lasts) != 1 || lasts[0].Outcome != "stopped" {
		t.Fatalf("calls %d, lasts %+v", len(f.cl.Calls()), lasts)
	}
	if n := f.p.Turns.busy["lan"]; n != 1 {
		t.Errorf("lan busy %d, want only the turn held elsewhere", n)
	}
}

// placesOf are the places reserved for poll, by provider, read as the poll sends them.
func placesOf(t *ProviderTurns, poll string, providers []config.Provider) map[string]int {
	out := map[string]int{}
	for _, pl := range t.reserve(poll, providers) {
		out[pl.Provider] = pl.Free
	}
	return out
}

// CLR-253: a provider with max_agents has max_agents + lookahead places for
// runs in play; a provider without a limit has none listed, and with no
// limited provider the list is nil. A run is in play once, on every
// provider it is on, and leaves them all when it ends.
func TestProviderPlaces(t *testing.T) {
	var turns ProviderTurns
	providers := []config.Provider{{Name: "lan", MaxAgents: 1}, {Name: "ahead", MaxAgents: 1, Lookahead: 1}, {Name: "open"}}
	check := func(what string, want map[string]int) {
		t.Helper()
		got := placesOf(&turns, "CSR", providers)
		turns.unreserve("CSR")
		if !maps.Equal(got, want) {
			t.Errorf("%s: places %v, want %v", what, got, want)
		}
	}
	check("idle", map[string]int{"lan": 1, "ahead": 2})
	turns.enter("CSR", "lan")
	turns.enter("CSR", "lan") // once
	turns.enter("CSR", "ahead")
	check("one run on both", map[string]int{"lan": 0, "ahead": 1})
	turns.enter("WEB", "ahead")
	check("two runs on ahead", map[string]int{"lan": 0, "ahead": 0})
	turns.enter("APP", "ahead") // past the cap: a run that goes is never refused
	check("three runs on ahead", map[string]int{"lan": 0, "ahead": 0})
	if left := turns.leave("CSR", providers); !left {
		t.Error("CSR left lan and ahead, both limited, yet no limited place was freed")
	}
	check("CSR ended", map[string]int{"lan": 1, "ahead": 0})
	turns.leave("APP", providers)
	check("APP ended", map[string]int{"lan": 1, "ahead": 1})
	if turns.leave("APP", providers) {
		t.Error("a run that is in play nowhere freed a place")
	}
	turns.enter("WEB", "open")
	turns.leave("WEB", []config.Provider{{Name: "open"}, {Name: "ahead"}})
	check("WEB ended", map[string]int{"lan": 1, "ahead": 2})

	if got := turns.reserve("CSR", []config.Provider{{Name: "open"}}); got != nil {
		t.Errorf("no provider with a limit: %v, want nil", got)
	}
	turns.unreserve("CSR")
	if len(turns.inPlay) != 0 || len(turns.reserved) != 0 {
		t.Errorf("left in play %v, reserved %v; want none", turns.inPlay, turns.reserved)
	}
}

// A place is in one outstanding poll at a time: a second poll gets only
// what the first left, its own earlier reservation is replaced, and an
// unreserve gives the places back.
func TestProviderPlacesReservedByOnePoll(t *testing.T) {
	var turns ProviderTurns
	providers := []config.Provider{{Name: "lan", MaxAgents: 2, Lookahead: 1}}
	turns.enter("APP", "lan")
	if got := placesOf(&turns, "CSR", providers)["lan"]; got != 2 {
		t.Errorf("CSR's poll: %d, want 2", got)
	}
	if got := placesOf(&turns, "CSR", providers)["lan"]; got != 2 {
		t.Errorf("CSR's next poll: %d, want its own 2 again", got)
	}
	if got := placesOf(&turns, "WEB", providers)["lan"]; got != 0 {
		t.Errorf("WEB's poll next to CSR's: %d, want 0", got)
	}
	turns.unreserve("CSR")
	if got := placesOf(&turns, "WEB", providers)["lan"]; got != 2 {
		t.Errorf("WEB's poll after CSR's: %d, want 2", got)
	}
}

// take puts the run in play on the provider at once, also when its turn
// waits, and a run already in play is not counted again.
func TestATurnPutsItsRunInPlay(t *testing.T) {
	var turns ProviderTurns
	providers := []config.Provider{{Name: "lan", MaxAgents: 1}}
	if err := turns.take(context.Background(), "CSR", "lan", 1, nil); err != nil {
		t.Fatal(err)
	}
	turns.done("lan")
	if err := turns.take(context.Background(), "CSR", "lan", 1, nil); err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() { got <- turns.take(context.Background(), "WEB", "lan", 1, nil) }()
	until(t, "WEB in play", func() bool { turns.mu.Lock(); defer turns.mu.Unlock(); return len(turns.inPlay["lan"]) == 2 })
	if p := placesOf(&turns, "X", providers)["lan"]; p != 0 {
		t.Errorf("places %d, want 0", p)
	}
	turns.unreserve("X")
	turns.done("lan")
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	turns.done("lan")
	turns.leave("CSR", providers)
	turns.leave("WEB", providers)
	if len(turns.inPlay) != 0 {
		t.Errorf("in play %v, want none", turns.inPlay)
	}
}

// A run that leaves a limited provider wakes every waiter of placesFreed; one
// that leaves only providers without a limit does not.
func TestLeavingAProviderWakes(t *testing.T) {
	var turns ProviderTurns
	freed := turns.placesFreed()
	turns.enter("CSR", "open")
	turns.leave("CSR", []config.Provider{{Name: "open"}})
	select {
	case <-freed:
		t.Error("woken by a provider without a limit")
	default:
	}
	turns.enter("CSR", "lan")
	turns.leave("CSR", []config.Provider{{Name: "lan", MaxAgents: 1}})
	select {
	case <-freed:
	default:
		t.Error("not woken when a place on lan was freed")
	}
	select {
	case <-turns.placesFreed():
		t.Error("the next channel is closed already")
	default:
	}
}
