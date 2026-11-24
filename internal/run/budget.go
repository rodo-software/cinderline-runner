package run

import (
	"context"
	"sync"
)

// headroom is what the budget leaves of Docker's memory for the egress
// proxies, the git helper containers and dockerd (decision 2026-11-24-agent-memory-limit).
const headroom = 1 << 30

// Budget is the memory the runs of the whole runner share: Docker's total
// memory less headroom (decision 2026-11-24-agent-memory-limit). Each slot
// (a project's team run or its Project Manager run) reserves its run's
// memory while it offers to take a job and while its run goes on. It is safe
// for concurrent use: every project's loop runs in its own goroutine.
type Budget struct {
	total func(ctx context.Context) (int64, error) // Docker's memory

	mu   sync.Mutex
	size int64            // 0: Docker's memory not read yet
	held map[string]int64 // by slot (Project.slot)
}

// NewBudget is a budget of Docker's memory, as total reads it, less
// headroom. It is read at the first reservation that succeeds in reading
// it, so a runner that starts before Docker does still gets one.
func NewBudget(total func(ctx context.Context) (int64, error)) *Budget {
	return &Budget{total: total, held: map[string]int64{}}
}

// reserve holds n bytes for slot, in place of what it held, when they fit
// next to every other slot's reservation, or when nothing else is held: a
// run larger than the whole budget runs alone. It says whether they were
// held; err says why the budget is not known.
func (b *Budget) reserve(ctx context.Context, slot string, n int64) (bool, error) {
	if b.Size() == 0 {
		// Read without the lock, so a slow Docker holds up no other slot and no status.
		total, err := b.total(ctx)
		if err != nil {
			return false, err
		}
		b.mu.Lock()
		b.size = max(total-headroom, 1) // a Docker smaller than the headroom: every run runs alone
		b.mu.Unlock()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var others int64
	alone := true
	for s, m := range b.held {
		if s != slot {
			others, alone = others+m, false
		}
	}
	if !alone && others+n > b.size {
		return false, nil
	}
	b.held[slot] = n
	return true, nil
}

// release frees what slot holds.
func (b *Budget) release(slot string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.held, slot)
}

// Status is the budget and what is held of it, in bytes; size is 0 while
// Docker's memory is not known.
func (b *Budget) Status() (size, reserved int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, n := range b.held {
		reserved += n
	}
	return b.size, reserved
}

// Size is the budget in bytes; 0 while Docker's memory is not known.
func (b *Budget) Size() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.size
}
