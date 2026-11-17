# `rename` moves a project to the new key's local names

- **Status:** Accepted
- **Date:** 2026-11-17
- **Source:** Willem, 2026-11-17, after renaming EMBER to CL and CSR to CLR with 0.4.1 and still seeing the old names on his machine: "I would rather change the ID as well", and "If remove then add fixes it cleanly, can't we just have a rename command that does that in the background?" Claude designed the rest.

## Decision

1. `cinderline rename --project OLD` sets the entry's `key` to the one the instance gives, as before, and its `id` to what `add` would give that key (`<workspace>-<KEY>`, or the key alone in workspace `default`). It is a `remove` and an `add` in one that keeps what can be kept.
2. **Kept:** the config entry with all its settings, and every secret of the project (runner token, git tokens, model keys), stored again under the new `id`. The new names are written first and the old ones deleted after the config is saved, so a failure halfway leaves the old names whole.
3. **Removed:** the Docker containers, networks and volumes of the old `id`, and its session notes, exactly as `remove` does. The clones are fetched again with the next run, kept sessions start fresh, and a client signed in with `login` must sign in again. Volumes are not copied.
4. **Refused** while the volumes may hold work that is not on the forge, or the app is owed a report: the project runs an issue, has a paused run, or has an outbox. Also when another project on this machine already has the new `id`.
5. An entry with the new key and the old `id`, as 0.4.1's `rename` left it, is moved by the same command.
6. Still by hand only: the runner changes neither `key` nor `id` by itself.

## What this replaces

Decision `2026-11-17-project-key-rename`, point 1 in part and point 2's "writes it": the `id` is no longer fixed for ever, and `rename` moves the local names. That an entry gets its `id` when it is loaded, that old `names_by_key` entries keep the key alone, and point 3 stay.

## Reason

One name per project on a machine is easier to read: with a fixed `id`, volumes, keychain entries and some log lines kept saying EMBER for a project called CL. Copying volumes was the reason to keep the `id`; removing them instead costs a clone and a sign-in once, which Willem accepts, and needs no new code path that can fail halfway through a copy.

## Not covered

- The attachments folder of the old `id` in the state folder stays behind; `remove` leaves it too. It holds the files of the last run step only.
- The session of an issue paused before the rename is not carried over: `rename` refuses until that run has ended.
