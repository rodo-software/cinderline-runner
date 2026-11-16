package cinderline

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/redact"
)

// Write is one write to Cinderline that a run decided and must not lose:
// exactly one of a comment, a status move or a usage report (CSR-159).
type Write struct {
	Ref     string              `json:"ref"`
	Comment string              `json:"comment,omitempty"`
	Move    *UpdateIssueRequest `json:"move,omitempty"`
	Usage   []UsageEntry        `json:"usage,omitempty"`
}

// sendWaits are the waits between Send's tries, unless Client.SendWaits is set.
var sendWaits = []time.Duration{2 * time.Second, 8 * time.Second}

// The waits between replays while Cinderline stays down.
const (
	minReplayWait = 5 * time.Second
	maxReplayWait = 5 * time.Minute
)

// outbox is the state of the client's outbox, under outMu.
type outbox struct {
	loaded     bool
	writes     []Write
	replayAt   time.Time
	replayWait time.Duration
	replaying  bool // one Replay at a time: it removes what it delivered by index
}

// Send delivers w, trying again with sendWaits. A write that still fails, or
// one for an issue that has writes waiting already, is kept in the outbox
// file and delivered by Replay, in order per issue, also after a restart.
// The outbox is a cache of a transition already decided, not workflow state
// (decision 0091). A write Cinderline refuses for good is dropped and logged.
func (c *Client) Send(ctx context.Context, w Write) {
	w.Comment = redact.String(w.Comment) // the file must not hold a secret either
	c.outMu.Lock()
	c.load()
	if c.waiting(w.Ref) {
		c.keep(w)
		c.outMu.Unlock()
		return
	}
	c.outMu.Unlock()

	waits := c.sendWaits()
	var err error
	for i := 0; ; i++ {
		if err = c.deliver(ctx, w); err == nil || !transient(err) || i == len(waits) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(waits[i]):
		}
	}
	switch {
	case err == nil:
	case !transient(err):
		c.Log.Warn("write refused, dropped", "issue", w.Ref, "err", err)
	default:
		c.Log.Warn("write failed, kept for a replay", "issue", w.Ref, "err", err)
		c.outMu.Lock()
		c.keep(w)
		c.outMu.Unlock()
	}
}

// Waiting reports whether ref has writes in the outbox.
func (c *Client) Waiting(ref string) bool {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	c.load()
	return c.waiting(ref)
}

// Replay delivers the outbox's writes in order. A write that fails holds
// back the later writes of its issue; the next Replay tries again after a
// wait that doubles while Cinderline stays down. Cheap when nothing waits,
// so the scheduler calls it on every poll.
func (c *Client) Replay(ctx context.Context) {
	c.outMu.Lock()
	c.load()
	if c.out.replaying || len(c.out.writes) == 0 || time.Now().Before(c.out.replayAt) {
		c.outMu.Unlock()
		return
	}
	c.out.replaying = true
	writes := c.out.writes
	c.outMu.Unlock()

	done := map[int]bool{}
	held := map[string]bool{}
	for i, w := range writes {
		if held[w.Ref] {
			continue
		}
		err := c.deliver(ctx, w)
		if ctx.Err() != nil {
			break
		}
		if err != nil && transient(err) {
			held[w.Ref] = true
			continue
		}
		if err != nil {
			c.Log.Warn("write refused, dropped", "issue", w.Ref, "err", err)
		} else {
			c.Log.Info("kept write delivered", "issue", w.Ref)
		}
		done[i] = true
	}

	c.outMu.Lock()
	defer c.outMu.Unlock()
	c.out.replaying = false
	// Send may have appended meanwhile; those come after writes.
	var left []Write
	for i, w := range c.out.writes {
		if !done[i] {
			left = append(left, w)
		}
	}
	c.out.writes = left
	c.save()
	if len(held) == 0 {
		c.out.replayWait = 0
		return
	}
	c.out.replayWait = min(max(c.out.replayWait*2, minReplayWait), maxReplayWait)
	c.out.replayAt = time.Now().Add(c.out.replayWait)
}

// sendWaits are Client.SendWaits, else the default ones.
func (c *Client) sendWaits() []time.Duration {
	if c.SendWaits != nil {
		return c.SendWaits
	}
	return sendWaits
}

func (c *Client) deliver(ctx context.Context, w Write) error {
	switch {
	case w.Move != nil:
		_, err := c.UpdateIssue(ctx, w.Ref, *w.Move)
		return err
	case w.Usage != nil:
		return c.ReportUsage(ctx, w.Ref, w.Usage)
	}
	_, err := c.AddComment(ctx, w.Ref, w.Comment)
	return err
}

// transient reports whether a write may succeed later: no answer, a server
// error, a timeout or a rate limit. Any other answer is final; a 409 means a
// person moved the issue meanwhile.
func transient(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		var conflict *ConflictError
		var upgrade *UpgradeRequiredError
		return !errors.As(err, &conflict) && !errors.As(err, &upgrade)
	}
	return api.StatusCode >= 500 || api.StatusCode == http.StatusRequestTimeout || api.StatusCode == http.StatusTooManyRequests
}

// The helpers below run under outMu.

func (c *Client) waiting(ref string) bool {
	for _, w := range c.out.writes {
		if w.Ref == ref {
			return true
		}
	}
	return false
}

func (c *Client) keep(w Write) {
	c.out.writes = append(c.out.writes, w)
	c.save()
}

// load reads the outbox file once; a missing file is an empty outbox.
func (c *Client) load() {
	if c.out.loaded || c.Outbox == "" {
		return
	}
	c.out.loaded = true
	b, err := os.ReadFile(c.Outbox)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err == nil {
		err = json.Unmarshal(b, &c.out.writes)
	}
	if err != nil {
		c.Log.Error("outbox not read", "file", c.Outbox, "err", err)
	}
}

// save writes the outbox file whole, through a rename, so a crash leaves
// the old file or the new one. A failure keeps the writes in memory only.
func (c *Client) save() {
	if c.Outbox == "" {
		return
	}
	var err error
	if len(c.out.writes) == 0 {
		err = os.Remove(c.Outbox)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	} else {
		var b []byte
		b, _ = json.Marshal(c.out.writes)
		tmp := c.Outbox + ".tmp"
		if err = os.MkdirAll(filepath.Dir(c.Outbox), 0o700); err == nil {
			if err = os.WriteFile(tmp, b, 0o600); err == nil {
				err = os.Rename(tmp, c.Outbox)
			}
		}
	}
	if err != nil {
		c.Log.Error("outbox not written", "file", c.Outbox, "err", err)
	}
}
