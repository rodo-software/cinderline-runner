# A runner-wide default for max_parallel_runs, and the `parallel` command

- **Status:** Accepted
- **Date:** 2026-11-25
- **Source:** Willem, CLR-249 (2026-11-25): points 1 to 3. The command's name, `parallel`, was left open in the issue with `parallel` recommended; Claude took the recommendation, vetoable. Claude designed the rest; each point is vetoable.

## Decision

1. `config.json` may hold `max_parallel_runs` at its top: the most team runs at once of every project that sets none of its own. Not set, it is 1.
2. A project's own `max_parallel_runs` is optional and overrides the runner-wide one; not set, the runner-wide one applies. The Project Manager's run is not counted, as before (app decision 2026-11-24-lanes).
3. Both are 1 to 10 (`config.MaxParallel`, the contract's limit on `max_parallel`); `Validate` refuses anything else, and a file holding such a value does not load.
4. `cinderline parallel show|set N|clear [--project KEY]`, shaped like `memory` and `allow-host`: without `--project` it means the runner-wide value, with it that project's own. `show` without `--project` prints the runner-wide value and each project's effective value with where it comes from; `clear` removes the field, so the runner-wide value, or 1, applies again.
5. A change reloads a running runner. Under decision 2026-11-24-rolling-reload each project takes its new number of slots once its runs in flight have finished, and the command says so.

## Reason

Setting `max_parallel_runs` meant editing `config.json` by hand, once per project. A runner-wide default sets it for every project at once, and the command makes it a one-liner, as `memory` and `allow-host` do for their settings.
