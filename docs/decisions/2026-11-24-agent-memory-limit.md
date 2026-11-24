# A memory limit on every agent container

- **Status:** Accepted
- **Date:** 2026-11-24
- **Source:** Willem on CLR-215 and CLR-216 (the decisions in the issues, 2026-11-24).

## Decision

1. Every agent container, an agent turn's and a check's, runs with `--memory` and `--memory-swap` set to the same value, so it gets no swap: going over the limit kills the turn instead of slowing the machine down.
2. The limit is 4 GiB by default.
3. A project may set another in the runner's `config.json`, as `memory` on the project, such as `"8g"` or `"512m"`: a whole number of MiB (`m`) or GiB (`g`), 1024-based as `docker run --memory` reads it, at least `256m`. `cinderline memory show|set SIZE|clear --project KEY` shows, sets and clears it, as `allow-host` does for hosts, and reloads a running runner. It applies with and without a compose file.
4. A `mem_limit` on the project's agent service in its compose file wins over the project's setting, which wins over the default. The file is read from the base branch only, as before, so a pull request cannot change its own limit. `docker compose config --format json` writes `mem_limit` as a byte count in a string (`"2147483648"`, compose-go's `UnitBytes`) and leaves it out when unset; the runner reads that, and a plain number too.
5. The egress proxy gets a fixed 64 MiB, also without swap. Measured on 2026-11-24 (the proxy built natively on linux/arm64, every connection moving 10 MB): 9 MB idle, 23 MB with 100 connections open at once, 41 MB with 400 and 93 MB with 1000, so about 90 KB per open connection. An agent opens far fewer at once.
6. The other services of a compose file keep their own `mem_limit`, or none; the runner sets no default for them. `cpus` and `pids_limit` on the agent service stay unused.

Detecting and reporting a turn killed for running out of memory is its own issue (CLR-217).

### A memory budget for runs side by side (CLR-216)

7. Limiting the runs that go at once is the runner's responsibility: it is a fact about this machine, not workflow policy. The runner has one budget for the whole process, shared by every project: Docker's total memory (`docker info`, `MemTotal`: the VM's size on Docker Desktop, the host's memory on Linux) less 1 GiB of headroom for the egress proxies, the git helper containers and dockerd. It is read at the first slot that would be offered, and again until Docker answers, so a runner that starts before Docker Desktop still gets it; until then no slot is offered. It is then kept until the runner restarts.
8. A run's size is its agent container's limit (point 4) plus each other service of the compose file: its `mem_limit`, or 512 MiB when it has none. Without a compose file it is the agent's limit alone. The file is read from the base branch at every run's start, as before; until a run of the project has read it, the runner counts the agent's limit from the project's setting alone.
9. A slot (a project's team slot, or its Project Manager slot) says `idle` or `manager_idle` in its poll only when its run fits in what is left of the budget. It reserves that memory when it sends the poll and frees it when the answer brings no job, so two project loops cannot offer the same memory. A run holds its reservation from its start to its end, also between turns.
10. When a run's start finds a size other than the one reserved (the base's compose file changed since a run last read it), the reservation takes the new size. When that no longer fits, the run drops its job before its first `next`, as one taken back: nothing is claimed yet (2026-11-16-runner-protocol-v2), and the next poll offers the slot only once its new size fits. So reservations never add up to more than the budget.
11. Where a project's two slots are both free and only one run fits, they take turns at being offered, so the team's slot does not shut out the Project Manager's for good.
12. A run larger than the whole budget is offered only when nothing else holds memory, and the runner logs a warning when it starts.
13. A slot that waits for memory is just not idle to the app: no contract change. The runner logs it once per slot when it starts to wait, not at every poll, and `cinderline status` shows the budget, what is reserved, each project's run size and which projects wait for memory.
14. Only today's slots: one team run per project and the Project Manager slot. Several runs within one project would need a contract change and are not part of this.

**Known limit.** On macOS the runner sees only the Docker VM's size. If other apps fill the Mac's own memory, macOS still swaps, and the runner can neither see nor prevent that.

## Reason

Without a limit one run can take all of Docker's memory, and when that runs out the kernel kills a process in any container (another run, a proxy, a database) or the Docker Desktop VM hangs. A limit per container confines a runaway turn to itself, and is the base for running several runs side by side. 4 GiB fits Go `-race` tests and JavaScript builds; 1 to 2 GiB gets build-heavy turns killed. Before this, a `mem_limit` on the agent service passed the compose check but was silently ignored, as the runner starts the agent with its own `docker run`. Limits per container alone do not stop several runs from together needing more than Docker has, and then the kernel kills a process in any container or the Docker Desktop VM hangs or swaps; the budget makes the reserved limits never add up to more than the machine has. Reserving at the poll, not at the run's start, is what keeps two loops from offering the same memory at once.
