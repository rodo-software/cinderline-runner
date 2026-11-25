package run

import (
	"context"
	"errors"
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
		if err := turns.take(ctx, "lan", 2, waiting); err != nil {
			t.Fatal(err)
		}
	}
	if err := turns.take(ctx, "other", 1, waiting); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := turns.take(ctx, "open", 0, waiting); err != nil {
			t.Fatal(err)
		}
	}
	if waits != 0 {
		t.Fatalf("waited %d times below the limits", waits)
	}

	got := make(chan error, 1)
	go func() { got <- turns.take(ctx, "lan", 2, waiting) }()
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
		if err := turns.take(context.Background(), "lan", 2, nil); err != nil {
			t.Fatal(err)
		}
	}
	got := make(chan error, 1)
	go func() { got <- turns.take(context.Background(), "lan", 1, nil) }()
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
	go func() { got <- turns.take(ctx, "lan", 1, nil) }()
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
	if err := f.p.Turns.take(context.Background(), "lan", 1, nil); err != nil {
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
