# On "upgrade required" (426) the runner waits an hour; auto-update is a follow-up

- **Status:** Accepted
- **Date:** 2026-11-16
- **Source:** Decided by Willem on CSR-187 (option C: step 1 now, a follow-up issue for step 2).

## Decision

- On a `426` the slot takes no new work, logs once at Error "cinderline needs runner ≥ X; run `cinderline update` and then `cinderline run`", and shows the same text as the project's last error in `cinderline status`.
- For an hour after a `426` (`cinderline.UpgradeWait`) every repeated request of that project waits instead of its own interval: work, heartbeat or sync, review branches and merge checks. After the hour it asks again, so a lowered minimum resumes the slot without a restart.
- A run in flight is not stopped by the runner: its next request gets the `426` like any other.
- Auto-update (drain, the `cinderline update` code path, restart) is not built here. It is a follow-up issue, opt-in and off by default. Willem noted that the repository is public again, so releases carry build provenance attestations (decision 2026-11-16-attest-when-public) that the download can be checked against.

## Reason

- Polling every few seconds after a `426` only loads the app: the minimum version stays until a person updates the runner, and EMBER-223 already tells them with a Slack DM.
- An hour, not a full stop, keeps the runner working if the app lowers the minimum again, at one request per loop per hour.
- Step 1 is small and safe on its own; auto-update replaces code that holds credentials and needs its own review.
