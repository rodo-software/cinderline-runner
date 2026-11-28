# Decision 2026-11-28-update-restart

- **Status:** Accepted
- **Date:** 2026-11-28
- **Source:** Willem on CLR-303 (Web 37), option A of the question how the daemon restarts after an update started from the page.

## Decision

1. An update started from the local page runs in the daemon: it runs the install script over the running binary and has the new binary refresh the tools volume and the default image (`manage.Update`, shared with `cinderline update`).
2. Then the daemon drains with a restart mark. Once its runs have finished, `daemon.Run` returns `daemon.ErrRestart`, and `cinderline run` runs the new binary in its place with its own arguments: `syscall.Exec` on Unix, keeping the PID, so systemd, launchd and a terminal keep the runner; on Windows it starts the new process and exits (untested while the Windows checks are off).
3. A stop during that drain wins: the runner stays stopped.
4. `cinderline update` keeps its own way: it drains the daemon over the socket, waits for its process to exit and starts the service.

## Reason

When the page starts the update, nothing outside the daemon is left to start it again once it has exited, and the service restarts only a failure (decision 2026-11-07-service-restart). A child process the daemon started would be killed with it by systemd's cgroup or launchd's process group; exiting as a failure would not restart on macOS and would log a failure that is none. Re-exec needs no service and keeps the process the supervisor watches.
