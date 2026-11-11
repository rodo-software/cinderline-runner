package run

import (
	"context"
	"errors"
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
var errBudget = errors.New("budget reached")

// usageTimeout bounds the usage report at a run's end, also after a stop.
const usageTimeout = 30 * time.Second

// addUsage collects one CLI run's usage under the run's top-level issue top,
// for reportUsage. A nil runState or a run without usage collects nothing.
func (r *runState) addUsage(top, issue, agent, model string, u *client.Usage) {
	if r == nil || u == nil {
		return
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.usage == nil {
		r.usage = map[string][]cinderline.UsageEntry{}
	}
	r.usage[top] = append(r.usage[top], e)
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
// its run (reporting is rate-limited per token: app decision
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
	if err := c.ReportUsage(ctx, top, entries); err != nil {
		p.Log.Warn("usage not reported", "project", p.Config.Key, "issue", top, "entries", len(entries), "err", err)
	}
}

// checkBudget runs before every agent CLI run of top's run, in top's clone
// dir. When top reached its budget it starts no run: it commits and pushes
// what the clone holds, so no work is lost (preserve first, report second,
// EMBER-143; Willem on CSR-76), moves top to Blocked with a "Budget reached"
// comment and returns errBudget. A running CLI is never stopped (app
// decision 2026-11-05-cost-per-issue-and-budget). The usage of this run is
// reported only at its end, so its known cost is added here.
func (p Project) checkBudget(ctx context.Context, c *cinderline.Client, ws workspace.Workspace, top, dir string) error {
	issue, err := c.GetIssue(ctx, top)
	if err != nil {
		return err
	}
	spent := issue.Usage.CostUSD + p.run.pendingCost(top)
	if !issue.OverBudget && (issue.BudgetUSD == nil || spent < *issue.BudgetUSD) {
		return nil
	}
	if _, err := ws.Commit(ctx, dir, top+": "+issue.Title); err != nil {
		return err
	}
	if err := ws.Push(ctx, dir, name.Name+"/"+top); err != nil {
		return err
	}
	if _, err := c.UpdateIssue(ctx, top, cinderline.UpdateIssueRequest{Status: "blocked", ExpectedStatus: "in_progress"}); err != nil {
		return err
	}
	var budget float64
	if issue.BudgetUSD != nil {
		budget = *issue.BudgetUSD
	}
	// Blocked already: a lost comment must not drop the paused note.
	p.comment(ctx, c, top, fmt.Sprintf("**Budget reached**: this issue has cost $%.2f of its $%.2f budget. Raise the budget on the issue and move it to To do to continue.", spent, budget))
	p.Log.Info("budget reached, blocked", "project", p.Config.Key, "issue", top, "spent_usd", spent, "budget_usd", budget)
	return errBudget
}
