# Reload configuration restarts the whole scheduler

- **Status:** Accepted
- **Date:** 2026-11-05
- **Source:** Decided by Willem on CSR-16 (option A); built in CSR-73.

## Decision

Reloading the configuration (`POST /reload` on the local API) restarts the whole scheduler: cancel it, wait for it to return, re-read `config.json` and start again. A run in flight is aborted and recovered at the next slot start, the same as stop then start (decision 2026-11-05-runner-rewrite-details point 14). A config that fails to load keeps the old one: the old slots restart and the reload caller gets the error.

Defaults used with it, which Willem saw and did not object to:

- The log file rotates at 10 MB and keeps 3 old files.
- `/logs` sends the last 100 lines, then follows.
- A project without a runner token gets no slot and shows that as its last error.
- The daemon creates the socket folder 0700.
- A held socket is detected by dialing it; any other file at the socket path is stale and removed.

## Reason

One code path, with the same semantics as stop; a reload is rare.
