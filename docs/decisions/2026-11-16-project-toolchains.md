# A project's own toolchain comes from the `docker` environment

- **Status:** Accepted
- **Date:** 2026-11-16
- **Source:** Decided by Willem on CSR-77, option A of four; proposed by Claude. Ends the stopgap of decision 2026-11-06-default-image-go.

## Decision

1. **A project that needs a toolchain other than the default image's uses the `docker` environment** (EMBER-28, built in CSR-168): a Dockerfile and a `.cinderline/compose.yml` in its repository, whose agent service builds or pulls the image, and an admin sets the project's environment to `docker` in Cinderline. The runner needs no new code for this. An agent image needs `/bin/sh` and a glibc or musl loader; it runs as uid 1000:1000 and needs no agent CLIs, which come from the tools volume. `existing` (EMBER-29) covers developers who already have a Compose or Lando setup, as decided in 2026-11-08-m6-environments.
2. **The runner project moves first (CSR-184):** it gets its own `.cinderline/Dockerfile` and `.cinderline/compose.yml` with Go, staticcheck and govulncheck under today's pins, and switches to `docker`.
3. **After that, the default image becomes slim again (CSR-185):** Go, staticcheck, govulncheck, gcc and libc6-dev leave `images/default/Dockerfile`, which goes back to PHP 8.4, Composer, Node 22 and git, Cinderline's own stack. The image is then published and pinned again.
4. **The README explains it (CSR-186):** a section on giving a project its own toolchain, covering what an agent image needs, with small example files for Go, Python and Node.

Rejected: keeping Go in the default image (B), toolchain volumes of pinned language toolchains checked by hash like the tools volume (C), and devcontainer features or `devcontainer.json` (D), which stay out of the plan (plan section 5).

## Reason

The `docker` environment already lets each project choose its own toolchain, and the image is built or pulled on the developer's own machine, which still holds when Cinderline is offered as SaaS. Any image works because the agent CLIs come from the tools volume. So all that was missing was documentation, plus Go, about 250 MB that every project paid for while only the runner project used it. Toolchain volumes would mean maintaining pins for every language for every customer. Devcontainer support would need the devcontainer CLI (Node) or our own feature installer, and would be a second config format next to compose.
