# Triage Labels

The skills speak in terms of five canonical triage roles. Cinderline has no labels, so each role is a status, for Backlog together with a folder. Set them with `update_issue` (`status`, `folder_id`; folder ids come from `list_folders`).

| Role in mattpocock/skills | In Cinderline (`CLR`)                  | Meaning                                  |
| ------------------------- | -------------------------------------- | ---------------------------------------- |
| `needs-triage`            | `backlog`, no folder                   | Maintainer needs to evaluate this issue  |
| `needs-info`              | `backlog`, folder `Needs info`         | Waiting on reporter for more information |
| `ready-for-agent`         | `todo`                                 | Fully specified, ready for an AFK agent  |
| `ready-for-human`         | `backlog`, folder `Willem`             | Requires human implementation            |
| `wontfix`                 | `cancelled`, with a comment saying why | Will not be actioned                     |

When a skill mentions a role (e.g. "apply the AFK-ready triage label"), set the status and folder from this table.

Moving an issue to `todo` starts a runner run on it. Do that only when the issue is fully specified.
