# A solo team's own agent resolves the merge conflict before the push

- **Status:** Accepted
- **Date:** 2026-11-07
- **Source:** Willem, CSR-78 (the issue text, then option C of the question on Review upkeep). Extends the app repository's decision 2026-11-04-agent-resolves-merge-conflicts.

## Decision

When the base merge before the push conflicts and the team has no `Merge Resolver`, a solo team (only its orchestrator) gets the same single verified attempt:

- The orchestrator runs it, in the session it just worked in and with its scoped token, as for its other turns. The prompt is `MergePrompt`, the one CSR-25 ported.
- The check is the same as for the Merge Resolver: a clean worktree, base and old head both ancestors, no conflict markers, no `Blocked` report. Otherwise the runner resets to the head before the attempt and pushes the conflict as it is.
- Never a retry. The attempt counts against the issue's budget and `max_minutes` like any turn.
- No attempt without that session (the catch-up before a solo team's first turn), and none for a team with workers but no `Merge Resolver`. A team with a `Merge Resolver` is unchanged.
- Review upkeep (`ReviewBranches`) stays git only, as the plan says, for every team. Whether agents resolve conflicts on Review branches is left to a separate issue that decides it for all teams together, including running beside the project's current run.

## Reason

On 2026-11-06 the Opus 5.5 Solo team's conflicts on CSR-31 and CSR-34 were pushed as they were, and Willem resolved them by hand. Both resolutions were mechanical, and the agent that had just done the work had the full context. A person should review work, not do git plumbing. Adding a `Merge Resolver` worker to a solo team is not the answer: a team with workers runs in rounds with sub-issues, which changes how the team works.

Review is left out because the runner keeps no session after publishing (0091). Also, an agent turn in Review upkeep would run beside the project's current run, and resolver teams get no attempt there either.
