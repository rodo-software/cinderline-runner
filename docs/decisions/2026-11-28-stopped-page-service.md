# Decision 2026-11-28-stopped-page-service

- **Status:** Accepted
- **Date:** 2026-11-28
- **Source:** Willem on CLR-310 (Web 44), option A of the question how the page's stopped panel names the service.

## Decision

1. `GET /api/name` answers `service` next to `name`: the system's own command that starts the installed user service, or `""` when none is installed. That is `systemctl --user start cinderline` on Linux, `launchctl start cinderline` on macOS and `schtasks /run /tn cinderline` on Windows (`manage.ServiceStart`). A service in a failed state counts as installed.
2. Whether the service is installed is one check, `manage.ServiceInstalled`, which the CLI (`startService`, used by `update` and the reload after a stop) and the page both call.
3. Every page asks `/api/name` once it is parsed. When the runner has not answered for 5 s, its stopped panel names `service`, or `cinderline run` when that is empty.
4. The runner gets no command of its own that starts the service.

## Reason

Once the runner has stopped, the page cannot ask anything, so it learns the command while the runner runs. The system's own command starts the service the person installed, on every OS, without adding a CLI command that decision 2026-11-26-local-web-page does not list.
