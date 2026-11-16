package cinderline

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Sync mode (decision 2026-11-16-sync-request): when the handshake offers
// sync, RunHeartbeats sends one POST /sync per wait in place of the
// heartbeat, and Work, ListReview and Watch answer from what it brought
// instead of sending their own requests. They need RunHeartbeats running.

// The waits between syncs when the app sends no interval.
const (
	SyncInterval      = 60 * time.Second // while no turn runs
	SyncWatchInterval = 30 * time.Second // while one does: how often its issue's status is checked
)

// maxWatch caps the refs of one sync request.
const maxWatch = 20

// syncResult is how one sync ended: with the app's answer or an error.
type syncResult struct {
	answer SyncAnswer
	err    error
}

// Syncing reports whether the last GetProject offered sync.
func (c *Client) Syncing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.syncing
}

// setSyncing follows a handshake that offers sync or withdraws it: what the
// other mode's requests left behind is dropped, a waiting Work looks again
// and the loop in RunHeartbeats wakes up. The caller holds mu.
func (c *Client) setSyncing(on bool) {
	c.syncing = on
	c.last, c.fresh = nil, false
	c.etag, c.pollAfter = "", nil
	c.changed()
	c.wakeLoop()
}

// changed wakes every Work that waits for a sync answer. The caller holds mu.
func (c *Client) changed() {
	if c.waiters != nil {
		close(c.waiters)
		c.waiters = nil
	}
}

// wakeLoop ends the wait of the loop in RunHeartbeats, now or when it next waits.
func (c *Client) wakeLoop() {
	select {
	case c.wake <- struct{}{}:
	default: // it is already asked to
	}
}

// Sync does one POST /sync: watch holds the refs of the issues a turn runs
// on now. It counts as the heartbeat.
func (c *Client) Sync(ctx context.Context, watch []string) (SyncAnswer, error) {
	var out SyncAnswer
	in := map[string][]string{"watch": append([]string{}, watch...)} // never null
	err := c.call(ctx, "sync", "POST", "/sync", in, 200, &out)
	return out, err
}

// SyncNow asks for a sync at once instead of after the interval, and makes
// the next Work wait for its answer: one from before may be out of date.
// For the work loop at its start and after a run. It does nothing while the
// app does not offer sync.
func (c *Client) SyncNow() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.syncing {
		return
	}
	c.fresh = false
	c.wakeLoop()
}

// Watch keeps ref in the watch list of the syncs until stop is called, for
// a running turn. The channel gets ref's entry of each sync answer that has
// one, a newer one replacing one not read yet; a failed sync, or an answer
// without the ref, brings nothing. Without sync nothing arrives at all.
func (c *Client) Watch(ref string) (checks <-chan Watched, stop func()) {
	ch := make(chan Watched, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.watchers == nil {
		c.watchers = map[chan Watched]string{}
	}
	c.watchers[ch] = ref
	return ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.watchers, ch)
	}
}

// syncOnce does one sync with the refs watched now and keeps how it ended
// for Work, ListReview and the watchers. A failed sync is logged here too:
// during a run nobody calls Work.
func (c *Client) syncOnce(ctx context.Context) {
	c.mu.Lock()
	refs := make([]string, 0, len(c.watchers))
	for _, ref := range c.watchers {
		refs = append(refs, ref)
	}
	c.mu.Unlock()
	slices.Sort(refs)
	refs = slices.Compact(refs)
	answer, err := c.Sync(ctx, refs[:min(len(refs), maxWatch)])
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		c.Log.Warn("sync failed", "err", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.syncing {
		return // withdrawn meanwhile
	}
	c.last, c.fresh = &syncResult{answer, err}, true
	for _, w := range answer.Watched {
		for ch, ref := range c.watchers {
			if ref != w.Ref {
				continue
			}
			select {
			case <-ch: // not read yet
			default:
			}
			ch <- w // has room: only this code sends, under mu
		}
	}
	c.changed()
}

// syncedWork is Work while the app offers sync: the work of the latest sync
// answer when Work has not returned it yet, else of the next one, or that
// sync's error. ok is false when the app does not offer sync (any more).
func (c *Client) syncedWork(ctx context.Context) (issue *Issue, ok bool, err error) {
	for {
		c.mu.Lock()
		if !c.syncing {
			c.mu.Unlock()
			return nil, false, nil
		}
		if c.fresh {
			c.fresh = false
			r := c.last
			c.mu.Unlock()
			return r.answer.Work, true, r.err
		}
		if c.waiters == nil {
			c.waiters = make(chan struct{})
		}
		wait := c.waiters
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, true, fmt.Errorf("sync: %w", ctx.Err())
		case <-wait:
		}
	}
}

// syncedReview is ListReview while the app offers sync: the Review list of
// the latest sync answer. While there is none, or the latest sync failed,
// it is an error, as a failed list is. ok is as syncedWork's.
func (c *Client) syncedReview() (issues []Issue, ok bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case !c.syncing:
		return nil, false, nil
	case c.last == nil:
		return nil, true, errors.New("listIssues: no sync answer yet")
	}
	return c.last.answer.Review, true, c.last.err
}

// beatWait is how long RunHeartbeats waits before its next request: the
// interval the last successful answer sent, else the default.
func (c *Client) beatWait() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.syncing {
		if d, ok := c.pollAfter[string(PollHeartbeat)]; ok {
			return d
		}
		return HeartbeatInterval
	}
	if d, ok := c.pollAfter[string(PollSync)]; ok {
		return d
	}
	if len(c.watchers) > 0 {
		return cmp.Or(c.SyncWatchWait, SyncWatchInterval)
	}
	return cmp.Or(c.SyncWait, SyncInterval)
}
