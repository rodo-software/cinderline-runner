# A project is URL, workspace and key; names follow an ID that keeps old ones

- **Status:** Accepted
- **Date:** 2026-11-15
- **Source:** Willem's issue CSR-97, from app decision 2026-11-07-shared-tables-and-scalability points 1 to 3 (EMBER-159). Amends point 4 of decision 2026-11-11-audit-fixes. The `default` slug keeping key-only names (point 3), the refusal of an unfilled namesake (point 2) and point 5 are the implementation's defaults, open to veto.

## Decision

1. `GET project` names the token's workspace, `{id, slug}`; a Cinderline from before workspaces sends none, which means the slug `default`.
2. A config entry is identified by URL, workspace slug and key. `add` stores the slug and refuses an entry with the same three, naming it; an entry without a slug yet at the same URL and key is refused too, as it may be the same project. Commands that take a project key choose with `-url` and `-workspace`.
3. Docker volumes, containers, networks, labels, keychain entries and state files (paused notes, outbox, attachments) are named by the project's ID: `<workspace>-<KEY>`, or the key alone for the `default` workspace and for an entry made before workspaces. Two entries with one ID, such as one workspace and key on two URLs, still share those names: `add` refuses the second and the daemon runs only the first, as point 4 of 2026-11-11-audit-fixes did for a shared key.
4. Upgrade: the daemon fills the slug into an entry that has none, or only `default`, at its first handshake, and sets `names_by_key` so its ID, and with it every name, stays the key. Nothing is renamed or migrated. A slug that is not `default` is never replaced.
5. A linked project is only a configured project in the same workspace, so a namesake in another workspace never gives its code or token to a run.

## Reason

Project keys are unique per workspace, not per host, once hosted Cinderline is one deployment with shared tables. Naming by one ID keeps every place that builds a name a one-word change, and keeping the key as the ID for old and `default` entries means no running project loses its volumes, secrets or outbox, which was the issue's condition.
