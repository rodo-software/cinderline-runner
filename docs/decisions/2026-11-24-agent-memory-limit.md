# A memory limit on every agent container

- **Status:** Accepted
- **Date:** 2026-11-24
- **Source:** Willem on CLR-215 (the decisions in the issue, 2026-11-24).

## Decision

1. Every agent container, an agent turn's and a check's, runs with `--memory` and `--memory-swap` set to the same value, so it gets no swap: going over the limit kills the turn instead of slowing the machine down.
2. The limit is 4 GiB by default.
3. A project may set another in the runner's `config.json`, as `memory` on the project, such as `"8g"` or `"512m"`: a whole number of MiB (`m`) or GiB (`g`), 1024-based as `docker run --memory` reads it, at least `256m`. `cinderline memory show|set SIZE|clear --project KEY` shows, sets and clears it, as `allow-host` does for hosts, and reloads a running runner. It applies with and without a compose file.
4. A `mem_limit` on the project's agent service in its compose file wins over the project's setting, which wins over the default. The file is read from the base branch only, as before, so a pull request cannot change its own limit. `docker compose config --format json` writes `mem_limit` as a byte count in a string (`"2147483648"`, compose-go's `UnitBytes`) and leaves it out when unset; the runner reads that, and a plain number too.
5. The egress proxy gets a fixed 64 MiB, also without swap. Measured on 2026-11-24 (the proxy built natively on linux/arm64, every connection moving 10 MB): 9 MB idle, 23 MB with 100 connections open at once, 41 MB with 400 and 93 MB with 1000, so about 90 KB per open connection. An agent opens far fewer at once.
6. The other services of a compose file keep their own `mem_limit`, or none; the runner sets no default for them. `cpus` and `pids_limit` on the agent service stay unused.

Detecting and reporting a turn killed for running out of memory, and a memory budget for running several runs side by side, are their own issues (CLR-217, CLR-216).

## Reason

Without a limit one run can take all of Docker's memory, and when that runs out the kernel kills a process in any container (another run, a proxy, a database) or the Docker Desktop VM hangs. A limit per container confines a runaway turn to itself, and is the base for running several runs side by side. 4 GiB fits Go `-race` tests and JavaScript builds; 1 to 2 GiB gets build-heavy turns killed. Before this, a `mem_limit` on the agent service passed the compose check but was silently ignored, as the runner starts the agent with its own `docker run`.
