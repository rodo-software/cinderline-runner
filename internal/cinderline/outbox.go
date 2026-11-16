package cinderline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
)

// Queued is a Next request that could not be delivered: the report of the
// step that ended a run. It is sent again, unchanged, and its answer ignored.
type Queued struct {
	Ref     string      `json:"ref"`
	Request NextRequest `json:"request"`
}

// Queue keeps r for ref in the outbox, also across a restart. The file is a
// cache of a report already decided, not workflow state (decision 0091).
func (c *Client) Queue(ref string, r NextRequest) {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	c.load()
	c.queued = append(c.queued, Queued{Ref: ref, Request: redacted(r)}) // the file must not hold a secret either
	c.save()
}

// Queued returns the refs with requests in the outbox, each once, in order.
func (c *Client) Queued() []string {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	c.load()
	var refs []string
	for _, q := range c.queued {
		if !slices.Contains(refs, q.Ref) {
			refs = append(refs, q.Ref)
		}
	}
	return refs
}

// Replay sends the outbox's requests in order. The first that the app does
// not answer, or answers with a 426, stops the replay: it and the later ones
// wait for the next. One the app refuses for good is dropped and logged.
func (c *Client) Replay(ctx context.Context) {
	c.outMu.Lock()
	defer c.outMu.Unlock() // one replay at a time; Queue waits for it
	c.load()
	if len(c.queued) == 0 {
		return
	}
	n := 0
	for _, q := range c.queued {
		_, err := c.Next(ctx, q.Ref, q.Request)
		var upgrade *UpgradeRequiredError
		if err != nil && (Unavailable(err) || errors.As(err, &upgrade) || ctx.Err() != nil) {
			break
		}
		if err != nil {
			c.Log.Warn("queued report refused, dropped", "issue", q.Ref, "err", err)
		} else {
			c.Log.Info("queued report delivered", "issue", q.Ref)
		}
		n++
	}
	if n > 0 {
		c.queued = c.queued[n:]
		c.save()
	}
}

// load reads the outbox file once; a missing file is an empty outbox. The caller holds outMu.
func (c *Client) load() {
	if c.loaded || c.Outbox == "" {
		return
	}
	c.loaded = true
	b, err := os.ReadFile(c.Outbox)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err == nil {
		err = json.Unmarshal(b, &c.queued)
	}
	if err != nil {
		c.Log.Error("outbox not read", "file", c.Outbox, "err", err)
	}
}

// save writes the outbox file whole, through a rename, so a crash leaves
// the old file or the new one. A failure keeps the requests in memory only.
// The caller holds outMu.
func (c *Client) save() {
	if c.Outbox == "" {
		return
	}
	var err error
	if len(c.queued) == 0 {
		if err = os.Remove(c.Outbox); errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	} else {
		b, _ := json.Marshal(c.queued)
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
