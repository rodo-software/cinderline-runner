package run

import (
	"context"
	"sync"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
)

// ProviderTurns are the agent turns in flight on each of the runner's model
// providers, over every project, so a provider's max_agents holds the turns
// past it (CLR-243), and the runs in play on each, so a poll tells the app
// how many more it may start (CLR-253, decision 2026-11-25-provider-places
// in the app repository). The zero value is ready; it is safe for
// concurrent use: every project's runs share one, also over a reload.
type ProviderTurns struct {
	mu    sync.Mutex
	busy  map[string]int // by provider: its turns in flight
	freed chan struct{}  // closed, and replaced, when a turn ends

	// A run is in play on a provider from its job, or its first turn on
	// it, until it ends; runs are named as the budget names them (Project.slot).
	inPlay map[string]map[string]bool // by provider: its runs in play
	// reserved are the places each poll in flight offered, by project ID,
	// then by provider: a place is in one outstanding poll at a time.
	reserved map[string]map[string]int
	left     chan struct{} // closed, and replaced, when a run leaves a limited provider
}

// take holds a place for one turn on provider: at once while fewer than
// limit turns are on it, or when limit is 0; otherwise it calls waiting,
// once, and waits for a turn on it to end. It gives up with ctx's cause.
// The limit is the caller's, as its config has it: a reload may lower it
// while turns of the older config are in flight, and they count. The run
// is in play on provider from now on, also while it waits.
func (t *ProviderTurns) take(ctx context.Context, run, provider string, limit int, waiting func()) error {
	t.enter(run, provider)
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

// enter puts run in play on provider, when it is not yet. It never waits
// and may pass the provider's places: a run that goes is not refused.
func (t *ProviderTurns) enter(run, provider string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inPlay == nil {
		t.inPlay = map[string]map[string]bool{}
	}
	if t.inPlay[provider] == nil {
		t.inPlay[provider] = map[string]bool{}
	}
	t.inPlay[provider][run] = true
}

// leave takes run out of play on every provider, as it ended. It says
// whether that freed a place on one of providers with a limit, and then
// wakes every placesFreed.
func (t *ProviderTurns) leave(run string, providers []config.Provider) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	freed := false
	for pr, runs := range t.inPlay {
		if !runs[run] {
			continue
		}
		delete(runs, run)
		if len(runs) == 0 {
			delete(t.inPlay, pr)
		}
		for _, c := range providers {
			freed = freed || c.Name == pr && c.MaxAgents > 0
		}
	}
	if freed {
		t.wakeLocked()
	}
	return freed
}

// placesFreed is closed when a run next leaves a provider with a limit,
// or wake is called.
func (t *ProviderTurns) placesFreed() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.left == nil {
		t.left = make(chan struct{})
	}
	return t.left
}

// wake closes placesFreed without a place freed: a turn started or stopped
// waiting, which its project's next poll reports (CLR-254).
func (t *ProviderTurns) wake() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.wakeLocked()
}

func (t *ProviderTurns) wakeLocked() {
	if t.left != nil {
		close(t.left)
	}
	t.left = make(chan struct{})
}

// reserve holds, for project's poll, every free place of each of providers
// with a limit, in place of what its poll held before, and returns them as
// the poll sends them; nil when no provider has a limit. A place is free
// when it is neither in play nor held by another project's poll.
func (t *ProviderTurns) reserve(project string, providers []config.Provider) []cinderline.ProviderPlace {
	t.mu.Lock()
	defer t.mu.Unlock()
	var places []cinderline.ProviderPlace
	held := map[string]int{}
	for _, pr := range providers {
		if pr.MaxAgents == 0 {
			continue
		}
		free := pr.MaxAgents + pr.Lookahead - len(t.inPlay[pr.Name])
		for other, r := range t.reserved {
			if other != project {
				free -= r[pr.Name]
			}
		}
		free = max(free, 0)
		held[pr.Name] = free
		places = append(places, cinderline.ProviderPlace{Provider: pr.Name, Free: free})
	}
	if places == nil {
		delete(t.reserved, project)
		return nil
	}
	if t.reserved == nil {
		t.reserved = map[string]map[string]int{}
	}
	t.reserved[project] = held
	return places
}

// unreserve gives back what project's poll held.
func (t *ProviderTurns) unreserve(project string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.reserved, project)
}
