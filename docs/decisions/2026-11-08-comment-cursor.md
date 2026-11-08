# Comment cursor under the 200-row cap

- **Status:** Accepted
- **Date:** 2026-11-08
- **Source:** Decided by Willem on CSR-114 (option C of three).

## Decision

- `GET /issues` and `GET /issues/{ref}/comments` return at most 200 rows. The runner does not page the issue lists: a list of exactly 200 logs one warning per read ("more than 200 <status> issues; only the first 200 are handled") and is used as it is.
- `GET /issues/{ref}/comments` takes an optional `after` (a comment ID, exclusive): it returns the oldest 200 comments newer than it. Without `after` it returns the newest 200. Both are oldest first.
- The runner reads the cursor without `after` (the newest 200 hold the newest ID) and looks for a person's comment with `after=<cursor>`.

## Reason

Comments are oldest first, so a server that returned the oldest 200 would hide every new comment on an issue past 200, and a person's answer to a long orchestrator issue would never be seen. `after` makes the check exact unless more than 200 comments arrive during one run; newest-200 without it keeps the plain read useful for anything that wants the latest comments. The issue lists are bounded by what a person keeps In progress, Review and To do, so one page is enough until the contract has a page parameter.
