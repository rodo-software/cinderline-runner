# In progress issues no run holds go to Blocked with a note

- **Status:** Accepted
- **Date:** 2026-11-16
- **Source:** Decided by Willem on CSR-182 (option A twice), for EMBER-143 D.

## Decision

- **When:** the slot looks for such issues in its own loop, not on a timer: in Recover at start, and after each run ends, before the next poll. A failure after a run only logs; Recover at the next start checks again.
- **What:** each In progress issue of the project goes to Blocked (`expected_status` in_progress) with a comment naming its branch `cinderline/<REF>` and, when this machine still has the clone, its last commit. This replaces the reset to To do at start. An issue whose write still waits in the outbox is left alone (CSR-159). A person moves the issue back to To do to run it again.

## Reason

A timer would be one extra `GET /issues?status=in_progress` per project per minute, outside the runner API (lists without `purpose` are not repeated on a timer, and the sync answer holds no In progress list), and it would race the slot's own claim. Between runs the slot holds nothing, so the check needs no guard and no new API. The limit: an issue dragged to In progress while the slot is idle waits until the next run ends or the runner restarts.

Blocked instead of To do, also at start, so nothing reruns without a person looking: one rule everywhere. Recover still pushes stranded branches, so no work is lost. This also covers runs a stop, an update or a config reload interrupts.
