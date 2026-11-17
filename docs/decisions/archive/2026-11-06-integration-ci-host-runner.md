# The integration tests run on a host-mode Forgejo runner

- **Status:** Accepted
- **Date:** 2026-11-06
- **Source:** Decided by Willem on CSR-39, option A of three; proposed by Claude. Adds to decision 2026-11-05-ci.

## Decision

The Linux integration tests (`go test -tags integration`, against real Docker and a throwaway Forgejo) run in their own Forgejo workflow on a runner in host mode, `runs-on: linux-host`, on a Linux machine with Docker. The other Linux checks stay on `runs-on: docker`. The job is drafted in `docs/ci/integration.yml`; Willem moves it to `.forgejo/workflows/integration.yml` and registers the runner.

Rejected: `runs-on: docker` with the host's Docker socket, the default bridge and a shared temp path (B), and Docker-in-Docker in the job (C).

## Reason

- The runner's code, which the test runs unchanged, bind-mounts a folder from its state folder into a container and the test reaches the throwaway Forgejo at its bridge IP; both hold directly on a Docker host, as on a developer's machine, with no wiring in the job or the test.
- B would give every job on that runner the host's Docker, which is root on the host.
- C needs privileged containers, extra wiring for the shared path and network, and downloads the tools and builds the default image on every run.
