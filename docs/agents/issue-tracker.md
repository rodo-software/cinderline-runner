# Issue tracker: Cinderline

Issues and specs for this repo live in Cinderline, project key `CLR`. Use the ember MCP tools (`mcp__ember__*`) for all operations. Every write tool needs `model`: the model you run on. The Forgejo repository's own issue list is not used.

## Conventions

- **Create an issue**: `create_issue` with `project_key: "CLR"`, `status: "backlog"`. Never create in `todo`: the runner starts every todo issue on its own.
- **Read an issue**: `get_issue` with the ref (e.g. `CLR-264`), and `list_comments` for its comments.
- **List issues**: `list_issues` with `project_key: "CLR"` and a `status` filter; `search` for text.
- **Comment on an issue**: `add_comment`.
- **Relate two issues**: `link_issues` (informational only). Put `Part of CLR-<n>` at the top of a child's body.
- **Close**: `add_comment` with the reason, then `update_issue` to `cancelled`. Only a person moves an issue to `done`.
- There are no labels. Triage roles are statuses and folders: see `triage-labels.md`.

## When a skill says "publish to the issue tracker"

Create a `CLR` issue in `backlog`.

## When a skill says "fetch the relevant ticket"

`get_issue` with its ref.

## Wayfinding operations

Used by `/wayfinder`. The **map** is a single issue with **child** issues as tickets.

- **Map**: one Backlog issue whose title starts with `Wayfinder map:`, holding the Notes / Decisions-so-far / Fog body.
- **Child ticket**: a Backlog issue with `Part of CLR-<map>` at the top of its body, linked to the map with `link_issues`. Its title starts with its type: `research:`, `prototype:`, `grilling:` or `task:`.
- **Blocking**: a `Blocked by: CLR-<n>, CLR-<n>` line at the top of the child body. Do not use `wait_for`: it moves the issue to todo, where the runner starts it. A ticket is unblocked when every blocker is done or cancelled.
- **Frontier query**: `list_issues` with status `backlog`, keep the map's children, drop any with an open blocker; first in board order wins.
- **Claim**: `update_issue` to `in_progress` with `expected_status: "backlog"`, the session's first write.
- **Resolve**: `add_comment` with the answer, move the ticket to `review`, then append a pointer to the map's Decisions-so-far.
