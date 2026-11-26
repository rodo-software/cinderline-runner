# A config change reloads each project on its own, without draining the runner

- **Status:** Accepted
- **Date:** 2026-11-24
- **Source:** Willem on CLR-213 (the wish on 2026-11-19, the two open points on 2026-11-24).

## Decision

Replaces points 1 and 2 of 2026-11-18-drain-before-reload for the commands that change the config or the secrets. `update` still drains and restarts the process; `stop` and `stop --drain` are unchanged.

1. A reload loads the config once; one that fails to load keeps the old one and the command gets the error. Then every project loop drains on its own: one with nothing in flight (no issue run, Project Manager run or upkeep pass) starts again with the new config at once, one with a run in flight finishes it, takes no new work meanwhile, and then starts again with the config that is newest by then. A project the new config adds starts at once; one it removes drains and ends. A loop for a project ID starts only once the old loop holding that ID has ended, as both would use the same Docker names. The process does not exit.
2. The command returns at once and says which issues still run on the old config. Runs already going need not see the change; every run that starts after it does.
3. Only `remove` and `rename` wait, for their own project only, before they remove its Docker volumes, as before with `s` then `y` to stop the whole runner at once, after which the service starts again. `key delete`, `remove-repo`, `remove-provider` and `login` do not wait: a run in flight holds the credentials it read.
4. `status` shows a project that still finishes a run on the old config, also one the new config removed, until its loop has ended.

## Reason

Draining the whole runner for a small config change blocked the command until the longest run ended, kept idle projects from work meanwhile, and left the runner stopped without a service. A change only has to reach the runs that start after it (Willem, 2026-11-19).
