# Token use and budget over the runner API: one usage call per run, budget stops push first

- **Status:** Accepted
- **Date:** 2026-11-11
- **Source:** CSR-76. Batching and "never throw work away" were decided by Willem (issue text, 2026-11-07; comment on CSR-76, 2026-11-07). The route's shape was proposed by Claude on CSR-76, as option A. Willem then pointed to the app repository's `RecordUsage`, which the shape follows.

## Decision

1. **Route.** `POST /runs/{ref}/usage` with `{"entries": [...]}` and a `204` answer. It sits next to the agent-token route, because both are scoped to one run. `{ref}` is the run's top-level issue. Each entry names its own `issue` (`{ref}` or one of its sub-issues), the team's `agent` by name (the app labels it "<team> / <agent>", as for agent tokens), the `model` (null for the client's default), the four token counts and `cost_usd` (null: the app estimates it, as `RecordUsage` does). The single-issue answers (`IssueWithComments`) carry `usage`, `budget_usd` and `over_budget`, as the app's `withCost()` and `get_issue` do.
2. **One call per run.** The runner collects the entries of every CLI run of a run: the orchestrator, the workers, the Merge Resolver and failed runs alike. It sends them once when the run ends: after Publish, or when Run ends with an error, a pause or a budget stop. The scheduler's status move after a failure comes right after that call. A stopped run still sends, on a detached context with a 30-second limit. A failed call only logs, as legacy `reportUsage` did.
3. **Budget before every CLI run, counting this run's cost.** Because the run's own entries reach the app only at its end, the check adds their known cost to the app's total: it stops when `over_budget` is set, or when the app's known cost plus this run's known unreported cost reaches `budget_usd`. The budget is always the top-level issue's, which is also what a sub-issue uses. A running CLI is never stopped.
4. **Preserve first, report second.** On a budget stop the runner commits and pushes the clone to `cinderline/<REF>`, then moves the issue from In progress to Blocked with legacy's "Budget reached" comment, keeps the paused note and ends the run as paused. A failed push or move is the run's error, so the scheduler comments it and moves the issue to Review, and the unpushed clone is pushed by the stranded-branch recovery. A stop at publish, before a Merge Resolver turn, ends the publish without a pull request.

## Reason

Hosted Cinderline rate-limits reporting per token (app decision 2026-11-07-shared-tables-and-scalability, point 16; EMBER-161), so a call per CLI run is too many. Without point 3, batching would let a whole run overspend, because the app sees no cost until the run ends. Willem asked that a budget stop never throw work away, so a person can raise the budget and continue from the branch.

## For the app repository

Mirror `docs/runner-api/openapi.yaml`:
- add the `reportUsage` route, storing each entry as `RecordUsage` does, under the entry's agent and model rather than the request's Actor;
- add `usage`, `budget_usd` and `over_budget` to `RunnerIssueResource` for single issues (getIssue, updateIssue, claimIssue);
- update the route test.
