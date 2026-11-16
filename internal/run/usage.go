package run

import (
	"context"
	"fmt"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// errBudget stops a run before an agent CLI run once its issue reached its
// budget (EMBER-112). checkBudget has pushed the work, moved the issue to
// Blocked and commented; the caller keeps the paused note and ends the run
// as paused, as after a question.
var errBudget = fmt.Errorf("budget reached: %w", errHalted)

// usageTimeout bounds the usage report at a run's end, also after a stop.
const usageTimeout = 30 * time.Second

// addUsage collects one CLI run's usage under the run's top-level issue top,
// for reportUsage. A nil runState or a run without usage collects nothing.
func (r *runState) addUsage(top, issue, agent, model string, u *client.Usage) {
	if e := usageEntry(issue, agent, model, u); e != nil {
		r.addEntry(top, *e)
	}
}

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

// pendingCost is the known cost of top's entries not reported yet.
func (r *runState) pendingCost(top string) float64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var sum float64
	for _, e := range r.usage[top] {
		if e.CostUSD != nil {
			sum += *e.CostUSD
		}
	}
	return sum
}

// reportUsage sends the entries collected for top in one call, at the end of
// its run: a team's, the merge turns of a solo run, and the turn whose next
// call failed (a solo run's turns travel in next, decision
// 2026-11-15-server-decides-steps point 6) (reporting is rate-limited per token: app decision
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

// checkBudget runs before every agent CLI run of a team's run and of a merge
// turn (a solo run's turns are the app's to judge, decision
// 2026-11-15-server-decides-steps point 6), in top's clone
// dir. When top reached its budget it starts no run: it commits and pushes
// what the clone holds, so no work is lost (preserve first, report second,
// EMBER-143; Willem on CSR-76), moves top to Blocked with a "Budget reached"
// comment and returns errBudget. A running CLI is never stopped (app
// decision 2026-11-05-cost-per-issue-and-budget). The usage of this run is
// reported only at its end, so its known cost is added here. The commit is
// author's, the agent's that was about to run.
func (p Project) checkBudget(ctx context.Context, c *cinderline.Client, ws workspace.Workspace, top, dir, author string) error {
	issue, err := c.GetIssue(ctx, top)
	if err != nil {
		return err
	}
	spent := issue.Usage.CostUSD + p.run.pendingCost(top)
	if !issue.OverBudget && (issue.BudgetUSD == nil || spent < *issue.BudgetUSD) {
		return nil
	}
	// A generic run (nil ws) has nothing to publish (CSR-75).
	if ws != nil {
		if _, err := ws.Commit(ctx, dir, author, top+": "+issue.Title); err != nil {
			return err
		}
		if err := ws.Push(ctx, dir, name.Name+"/"+top); err != nil {
			return err
		}
	}
	move(ctx, c, top, "blocked", "in_progress")
	var budget float64
	if issue.BudgetUSD != nil {
		budget = *issue.BudgetUSD
	}
	// Blocked already: a lost comment must not drop the paused note.
	p.comment(ctx, c, top, fmt.Sprintf("**Budget reached**: this issue has cost $%.2f of its $%.2f budget. Raise the budget on the issue and move it to To do to continue.", spent, budget))
	p.Log.Info("budget reached, blocked", "project", p.Config.Key, "issue", top, "spent_usd", spent, "budget_usd", budget)
	return errBudget
}
