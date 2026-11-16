package run

import (
	"context"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
)

// usageTimeout bounds the usage report at a run's end, also after a stop.
const usageTimeout = 30 * time.Second

// addEntry collects e under top, for reportUsage; a nil runState collects nothing.
func (r *runState) addEntry(top string, e cinderline.UsageEntry) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.usage == nil {
		r.usage = map[string][]cinderline.UsageEntry{}
	}
	r.usage[top] = append(r.usage[top], e)
}

// usageEntry is one CLI run's usage on issue; nil without usage.
func usageEntry(issue, agent, model string, u *client.Usage) *cinderline.UsageEntry {
	if u == nil {
		return nil
	}
	e := cinderline.UsageEntry{
		Issue: issue, Agent: agent,
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
		CostUSD: u.CostUSD,
	}
	if model != "" {
		e.Model = &model
	}
	return &e
}

// reportUsage sends the entries collected for top in one call, at the end of
// its run: the turn whose next call failed, as every other turn's usage
// travels in next (decision 2026-11-15-server-decides-steps points 6 and 8;
// reporting is rate-limited per token: app decision
// 2026-11-07-shared-tables-and-scalability point 16). A stopped run still
// reports what it has. A failure only logs, as legacy reportUsage: the
// entries are lost, the run is not.
func (p Project) reportUsage(ctx context.Context, c *cinderline.Client, top string) {
	if p.run == nil {
		return
	}
	p.run.mu.Lock()
	entries := p.run.usage[top]
	delete(p.run.usage, top)
	p.run.mu.Unlock()
	if len(entries) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageTimeout)
	defer cancel()
	c.Send(ctx, cinderline.Write{Ref: top, Usage: entries})
}
