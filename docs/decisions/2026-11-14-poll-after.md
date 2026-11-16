Superseded by decision 2026-11-16-runner-protocol-v2

# The app sets the runner's polling intervals with a Cinderline-Poll-After header

- **Status:** Accepted
- **Date:** 2026-11-14
- **Source:** Decided by Willem on CSR-174 (option A), for EMBER-209/EMBER-210.

## Decision

- Every successful answer (2xx, and a `304` to `GET /work`) to a request the runner repeats on a timer may carry `Cinderline-Poll-After`: whole seconds to wait before sending that request again, counted from the answer. These requests are `GET /work`, `POST /heartbeat` (the report sync follows it), and `GET /issues?status=review` for the review-branch and merge checks.
- The runner adds `purpose=review` or `purpose=merge` to the two Review checks, so the app can tell them apart and the runner keeps one interval for each.
- The runner clamps the value to 1 s to 1 h and adds jitter of up to a quarter of the interval, at most 3 s.
- An answer without the header, or with one that is not an integer, means the runner's own default: `IdleWait` doubling to `MaxIdleWait` for work, 60 s for the heartbeat, 5 min for review branches, and `merge_poll_seconds` (60 s) for merges. Error answers leave the last interval as it was, and the existing backoff after errors stays.

## Reason

Willem wants to tune runner traffic from the app without a runner release (EMBER-209). A header also reaches a `304` without a body. Keeping the old defaults when the header is missing lets a new runner talk to an older app.
