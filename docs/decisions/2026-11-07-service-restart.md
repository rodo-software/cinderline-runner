# Service restarts only on failure

- **Status:** Accepted
- **Date:** 2026-11-07
- **Source:** Decided by Willem on CSR-43.

## Decision

- The user-level service restarts the runner only when it fails: systemd `Restart=on-failure`, launchd `KeepAlive` false, the Windows task's `RestartOnFailure`.
- `cinderstack stop` therefore stops the runner until the next login on all three platforms.
- On macOS a crashed runner stays down until the next login or `cinderstack run`, because the library writes `KeepAlive` only as a boolean.

## Reason

- Decision 2026-11-05-runner-rewrite-details point 14 says `stop` stops at once. With `KeepAlive` true or `Restart=always`, the supervisor restarts the runner seconds after `stop`, and it takes up the issue it was on again.
- It is the smallest change and behaves the same on all three platforms. The macOS crash gap shows in `cinderstack status` and can be closed with the update work in CSR-44.
