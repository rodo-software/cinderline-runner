# Commands that reload the runner drain it first while runs are in flight

- **Status:** Accepted
- **Date:** 2026-11-18
- **Source:** Willem on CLR-195.

## Decision

1. Every command that makes a running runner reload its config (`add`, `remove`, `rename`, `key set` and `key delete`, `login`, `add-repo`, `remove-repo`, `add-provider`, `remove-provider`, `strict-hosts on|off`) first asks the runner what it runs. With nothing in flight it reloads as before (archive/2026-11-05-reload-config). With a run in flight it drains the runner instead, as `stop --drain`, waits until the process has exited, and starts the service again, which reads the new config; without the service it says to start it with `cinderline run`.
2. While it waits, `s` stops the runner at once, after a `y` to confirm; any other answer keeps waiting. A plain stop cuts the runs, as a reload did. `update`, which already drained, waits the same way.
3. The command's own change is made before the wait, so a Ctrl-C during it keeps the change; the runner still stops once its runs have finished and is not started again.

## Reason

A reload cuts every run in flight and breaks its session; a config change should not cost running work unless the person chooses that.
