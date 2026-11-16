# Errors reach Cinderline

- **Status:** Accepted
- **Date:** 2026-11-08
- **Source:** Decided by Willem on CSR-60, for the question CSR-55 held open (option C of three).

## Decision

- Every error the plan names (a repository mismatch, a missing key, an unsupported client, failed polls) goes into the runner report: `PUT /report` carries an `errors` list of `kind`, `issue` (for issue-specific errors) and `message`. The list is computed in memory, never stored (D8), and lost on restart.
- Issue-specific errors (a missing key, an unsupported client, found by skip-before-claim) also get one comment on the skipped issue, once per change: a new error, or a different message, comments again; the same error on the next poll does not.
- The report is sent at start and then at most once per heartbeat, only when it differs from the last report the server accepted. A failed poll is never a comment and never a write of its own: it rides on the next report.
- Error texts carry no tokens; URLs in them lose their credentials.

## Reason

The person looking at a stuck issue sees why on the issue itself today, without waiting for the app UI (EMBER-46 to EMBER-51), while runner-wide errors, which have no issue to comment on, still reach Cinderline. Writes stay bounded: runner traffic is the main load on a hosted Cinderline and reporting is rate-limited per token (app decision 2026-11-07-shared-tables-and-scalability, point 16; EMBER-161), so a comment costs one write per issue per new cause and the report one overwrite per heartbeat at most. A restart can repeat one comment per skipped issue, which Willem accepted with the option.
