package run

import (
	"context"
	"sync"
)

// ProviderTurns are the agent turns in flight on each of the runner's model
// providers, over every project, so a provider's max_agents holds the turns
// past it (CLR-243). The zero value is ready; it is safe for concurrent use:
// every project's runs share one.
type ProviderTurns struct {
	mu    sync.Mutex
	busy  map[string]int // by provider: its turns in flight
	freed chan struct{}  // closed, and replaced, when a turn ends
}

// take holds a place for one turn on provider: at once while fewer than
// limit turns are on it, or when limit is 0; otherwise it calls waiting,
// once, and waits for a turn on it to end. It gives up with ctx's cause.
// The limit is the caller's, as its config has it: a reload may lower it
// while turns of the older config are in flight, and they count.
func (t *ProviderTurns) take(ctx context.Context, provider string, limit int, waiting func()) error {
	for {
		t.mu.Lock()
		if t.busy == nil {
			t.busy, t.freed = map[string]int{}, make(chan struct{})
		}
		if limit == 0 || t.busy[provider] < limit {
			t.busy[provider]++
			t.mu.Unlock()
			return nil
		}
		freed := t.freed
		t.mu.Unlock()
		if waiting != nil {
			waiting()
			waiting = nil
		}
		select {
		case <-freed:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// done frees the place a turn on provider held.
func (t *ProviderTurns) done(provider string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.busy[provider]--; t.busy[provider] <= 0 {
		delete(t.busy, provider)
	}
	close(t.freed) // wakes every waiter; those of other providers wait again
	t.freed = make(chan struct{})
}
