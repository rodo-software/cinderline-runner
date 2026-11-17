# The runner checks each run with the project's check command

- **Status:** Accepted
- **Date:** 2026-11-16
- **Source:** Willem's issue CSR-80, and his "OK go ahead" on CSR-80 (2026-11-16) to the orchestrator's plan, which named points 2 to 4 as defaults. Rests on decisions 0122, 0091 and 0121 and app decision EMBER-144.

## Decision

1. After a finished run on a sub-issue, and after a solo team's run before publish, the runner commits, pushes and runs the project's `check_command` through `sh -c` in the clone, in the agent's environment without credentials, with the team's `max_minutes` as the limit. A failure resumes the agent's own session with a fix prompt, at most three times (a constant), each fix run behind the budget check. The runner's verdict heads the comment. Generic runs and projects without a command get no check.
2. A solo run whose check still fails after three fixes opens no pull request: its branch is pushed, the verdict with the output tail and the agent's report become the comment, and the issue goes to Review.
3. The solo prompt gets the same rule as the worker prompt: it names the command and forbids weakening tests.
4. The optional `kind` and `verdict` fields on usage entries (app decision 2026-11-08-run-timeline-kind-and-verdict) are left for a follow-up issue.

## Reason

Only verified work should reach the orchestrator or a pull request, and a failure is cheapest to fix in the session that made it. A pull request with red checks invites merging broken work, so a solo run that stays red ends in Review without one. The usage fields are not in CSR-80's text and add nothing to the check itself.
