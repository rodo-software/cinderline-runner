# A project key rename keeps the runner's local names

- **Status:** Accepted
- **Date:** 2026-11-17
- **Source:** Willem, on CLR-191, after EMBER became CL and CSR became CLR in the app (app decision 2026-11-12-rename-project-keys).

## Decision

1. Each project entry in `config.json` has a fixed `id` that names its Docker volumes, containers, networks and labels, its keychain entries, its outbox and its session notes. `add` sets it to what the ID was before (`<workspace>-<KEY>`, or the key alone in workspace `default`). An entry without one gets that name when it is loaded, and one with the old `names_by_key` gets the key alone, so no existing name changes.
2. The `key` is a label. The runner does not update it from the poll by itself: Willem wants a rename on a machine to be done by hand. `cinderline rename --project OLD [-url URL] [-workspace SLUG]` asks the instance, with the stored runner token, for the project's key now and writes it. After that, `key set runner-token` compares a token's project with the new key, as before.
3. Fields an older runner wrote (`names_by_key`, `allow_existing`, `merge_poll_seconds`, `build_image`) are ignored with a warning and left out at the next save. Any other unknown field is still refused, so a typo is not silently ignored.

## Reason

1. Naming everything after the key meant that a rename in the app cost the runner its tokens, sign-ins, clones and sessions, or that `key set runner-token` refused. A fixed local name avoids moving keychain entries and copying Docker volumes, which Docker cannot rename.
2. Willem prefers to control when a machine follows a rename.
3. On 2026-11-17 a leftover `names_by_key` stopped 0.4.0 from starting.

## Not covered

A session kept for an issue before a rename is stored under the old issue reference (`CSR-12`), note and clone alike. If the app starts the next run under the new reference (`CLR-12`), the runner does not find it and starts a fresh session. Not checked against the app.
