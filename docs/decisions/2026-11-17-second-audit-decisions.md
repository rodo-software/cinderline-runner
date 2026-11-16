# Second audit of runner protocol 2: a stop during publishing, and no agent header

- **Status:** Accepted
- **Date:** 2026-11-17
- **Source:** Decided by Willem on 2026-11-17, after the second audit of the runner and its API. Amends 2026-11-16-runner-protocol-v2 and 0109.

## Decision

1. **A stop while the runner publishes does not merge.** When the poll stops an issue while the runner publishes it, the runner still pushes and opens the pull request, but does not ask the forge to merge it. It reports `publish` / `published` with `merge: null`, even though the step said `merge: true`. For a trusted team the app writes a finish report that says so plainly (the pull request is open; it was not merged automatically because the issue was stopped during publishing; merge it by hand), leaves `merge_status` as it is, and moves the issue to Review as usual. That move is refused harmlessly when a person already moved the issue out of In progress.
2. **The `X-Cinderline-Agent` header is gone.** An MCP request's agent name is always the token's name. The runner's scoped agent tokens are named "<team> / <agent>" already, so the header named nothing a token did not, and a person's token could use it to pose as any agent.

## Reason

1. A person who stops an issue wants the work to halt where it is. Merging it anyway would ship what the person just stopped; dropping the push would lose the work. An open pull request keeps both choices with the person.
2. Since protocol 2 every agent turn runs on its own scoped token, so the header had no remaining use and only an attack surface.
