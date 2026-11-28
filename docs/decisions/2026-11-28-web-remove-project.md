# Decision 2026-11-28-web-remove-project

- **Status:** Accepted
- **Date:** 2026-11-28
- **Source:** Willem on CLR-302: option A (the daemon cleans up after the runs). The two defaults stated with the question were not vetoed.

## Decision

1. **Removing a project from the page does not wait for its runs.** `DELETE /api/projects/{key}` saves the config, deletes the secrets, reloads the daemon and answers 204. The daemon removes the project's Docker containers, networks and volumes, its session notes and its outbox on its own once that project's loop has ended. The order is the same as `cinderline remove`, which waits for the runs itself. If the daemon stops first, these stay; the log line that announces the removal names the `docker volume ls --filter label=…` that lists them. A rename does the same for the old ID. It refuses a busy project first, as the CLI does, so the wait is short.
2. **The new key of a rename comes from the instance,** as in `cinderline rename` (decision 2026-11-17-rename-moves-local-names). The route's `new_key`, when sent, must be that key, else 400.
3. **These two routes take the project's key in `{key}`,** with `url` and `workspace` in the body for a key that several projects share, as the CLI's `-url` and `-workspace` do. They do not take the project's ID, which the other `/api/projects/{key}/…` routes take.

## Reason

The page's API runs inside the daemon, and every config write holds one lock. A request that waits for runs that can take hours would block every other write and may time out in the browser. Refusing a busy project would make the page refuse what the CLI allows.
