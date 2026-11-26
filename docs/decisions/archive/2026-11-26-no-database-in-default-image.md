# The default image gets no database server

- **Status:** Accepted
- **Date:** 2026-11-26
- **Source:** Willem, CLR-265 (2026-11-26), option A: keep the default image as it is; a Laravel project with MySQL brings the README's compose file.

## Decision

1. The default image stays without a database server, and the default environment without any other service (decision 2026-11-11-docker-compose-environment holds unchanged).
2. A Laravel project that needs MySQL adds `.cinderline/compose.yml` as in the README's example (the default image as the agent, `mysql:8.0` beside it), and an admin sets the project's environment to `docker` in Cinderline.

## Reason

A server in the default image would start a database in every agent turn and check of every project, about 200 MB more image and 100 to 400 MB of memory inside the 4 GiB limit, also where no database is used, and would need an entrypoint that starts it and waits for it. Oracle's MySQL has no Debian arm64 packages, so the image, built for arm64 and amd64, could only carry MariaDB, not the MySQL 8 that CI runs. The compose file gives real MySQL 8, as CI has it, at no cost to other projects.
