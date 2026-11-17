# The `docker` environment: how the compose file runs

- **Status:** Accepted
- **Date:** 2026-11-11
- **Source:** CSR-168 (Willem moved it to To do), which redoes CSR-52: its code and its record `2026-11-08-docker-compose-environment.md` never reached main. Builds decision 2026-11-08-m6-environments point 1.

## Decision

1. **The default environment stays as it is** for now (CSR-168, "with the default environment unchanged"): the default image, MySQL and the proxy, without compose. Point 1's "the default environment becomes a built-in compose file" and "`docker compose` becomes a prerequisite" wait; only `docker` projects need `docker compose`, and a missing one fails their run with docker's own error.
2. **Per turn:** the base branch's tree is copied from the repo volume (`git archive`, symlinks left out) to a temporary folder; `docker compose config --format json` normalises the file; the check refuses what point 1 lists plus `volumes_from`, `uts: host`, `cgroup: host`, secrets and configs taken from the environment (CSR-52's follow-ups), and, as the same kind of host access, `ipc`/`userns_mode: host`, joining another container's namespaces, other or external networks, external volumes, and host paths outside the repository (build contexts, `env_file`, secret and config files). An override file puts every service on the run's internal network (made by the runner, as for the default environment, with the egress proxy) and labels them for recovery.
3. **The agent runs with `docker run`, not `docker compose run`,** in the agent service's image (built with `docker compose build`, or pulled) with the service's literal environment, the same hardening, user, mounts and secrets-by-name as the default environment. The service's command, entrypoint, user and mounts are not used. The other services start with `up --detach --wait`; `down --volumes` removes them after the turn.
4. **`docker compose` gets a minimal environment:** only the variables the docker CLI needs (PATH, HOME, DOCKER_*, temp and Windows profile folders), so the file's interpolation never sees the runner's environment, and the turn's secrets never pass through compose.
5. **Recovery** removes volumes labelled as compose volumes of the project, besides the containers and networks it already removes.

## Reason

1. CSR-168 asks for the `docker` environment without changing the default; moving the default onto compose is a separate change with its own risk.
2. Reading the normalised config checks what compose will actually do, after `extends`, `include` and interpolation. A path check only holds without symlinks.
3. With `docker compose run`, the agent's secrets would have to be in compose's environment, where interpolation could put them into other services. `docker run` reuses the default environment's hardened agent code, so both kinds share one place where secrets enter.
4. Point 1 and the rule that only the model credential and the scoped token enter a container.
5. A crash between up and down would otherwise leave named volumes behind for good.
