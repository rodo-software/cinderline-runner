# An issue that only waits is never parked with ask_person

- **Status:** Accepted
- **Date:** 2026-11-14
- **Source:** Decided by Willem on CSR-166 (option A), following EMBER-179 (option A).

## Decision

- The coding prompt tells the agent never to call `ask_person` only to wait, not even with options.
- If the issue waits for other issues, the agent says why with `add_comment`, calls `wait_for` (the issue goes to todo) and ends its turn.
- If it waits for anything else (a deploy, an update from a person), the agent says what it waits for with `add_comment`, moves the issue to blocked with `update_issue` (`expected_status` in_progress) and ends its turn. This is the one status change an agent makes in a run. A person moves the issue back to todo.
- The runner code does not change: after a turn, blocked or todo already means it pauses and keeps the session. The resume prompt now also covers a wait that ended, not only an answered question.

## Reason

An open question in Slack stays open for good, and then every next question must be answered in its own thread (EMBER-179). If the agent ended its turn without a status change, the runner would push, open a pull request and move a waiting issue to Review. Setting Blocked reuses the pause the runner already has, without new runner code.
