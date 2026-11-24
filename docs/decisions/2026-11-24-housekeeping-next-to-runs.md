# Review upkeep and tidying run next to the runs

- **Status:** Accepted
- **Date:** 2026-11-24
- **Source:** Willem on CLR-222 (parallel runs per lane, the runner side of the app's decision 2026-11-24-lanes), answer "A: next to runs". Amends the rule of CLR-194 that a run, an upkeep pass and tidying never overlap.

## Decision

1. A project runs up to its `max_parallel_runs` team runs at once (1 to 10, default 1), each in its own slot, clone and branch, plus its Project Manager run in its own slot. With more than one, the poll sends `max_parallel` and the runner starts the answer's `jobs`, one per free slot whose memory it reserved; an answer with only `job`, from an app before lanes, starts only while no team run is in flight.
2. The Review upkeep pass and tidying no longer wait until no run is in flight. They run next to the runs and never touch an issue a run holds: the pass skips the Review items of issues in flight, tidying leaves them out of `held` and pushes no clone whose folder or branch is theirs, and a job or manager job for an issue of the pass in flight waits for a later poll.
3. Tidying and a pass still never overlap: tidying is done in the poll, before the pass that poll starts, and not while a pass is in flight. The bare repository stays locked by the workspace for every fetch and push.
4. A poll that asks for the Review list also offers its free slots.

## Reason

With several slots and busy lanes there may never be a moment without a run, so a pass and tidying that waited for one would stall: Review branches would not be kept current or merged and kept sessions not pruned, for as long as the lanes stay busy. Each run, the pass and tidying work in their own issue's clone, so keeping them off each other's issues is enough.
