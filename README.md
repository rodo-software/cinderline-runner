# Cinderline runner

The Cinderline runner is a native Go program on a developer's machine that takes To do issues from a Cinderline instance, runs an agent team on each in its own Docker container, pushes a branch and opens a pull request.

Status: milestone M5, distribution and life cycle.

## Supported platforms

`cinderline` is one binary for x86-64 and ARM64. It needs Docker and nothing else on the machine: git runs in a container. Agent containers are Linux on every system; Windows containers are not supported.

| System | Versions | Docker |
|---|---|---|
| macOS | The current and two previous major releases, as Docker Desktop supports them; Apple silicon and Intel | Docker Desktop |
| Linux | Any distribution that runs Docker Engine 20.10 or later; `cinderline service` needs systemd | Docker Engine 20.10 or later, or Docker Desktop |
| Windows | Windows 10 22H2 (build 19045) or Windows 11 23H2 or later, 64-bit; ARM64 only as far as Docker Desktop's ARM build (early access) runs | Docker Desktop with the WSL2 backend |

- Docker Engine 20.10 is the first with `host-gateway`, which agent containers use to reach a Cinderline on the same machine (see below).
- On a Windows build below 19045 every command except `cinderline version` stops with an error naming the build it found. Windows Server is not supported: Docker Desktop does not run on it.
- CI runs the checks on macOS and Windows on GitHub and on Linux on Forgejo; [docs/ci/platforms.yml](docs/ci/platforms.yml) widens that to the oldest and newest images per system and architecture.

## Install

Install the latest `cinderline` binary with one line — no sudo or elevation required. Each script downloads the archive for your OS and architecture, verifies its SHA-256 checksum against the release's `checksums.txt` (refusing on mismatch), and installs the binary.

**macOS / Linux:**

```
curl -fsSL https://raw.githubusercontent.com/rodo-software/cinderline-runner/main/scripts/install.sh | sh
```

Installs to `~/.local/bin/cinderline`. If that directory is not on your `PATH`, the script prints a line to add to your shell profile.

**Windows (PowerShell 5.1+):**

```
irm https://raw.githubusercontent.com/rodo-software/cinderline-runner/main/scripts/install.ps1 | iex
```

Installs to `%LocalAppData%\Programs\cinderline\cinderline.exe`. If that directory is not on your user `PATH`, the script prints a command to add it.

### Run as a service

`cinderline service install` registers a user-level service (a launchd LaunchAgent on macOS, a systemd user unit on Linux, a logon scheduled task on Windows) that runs `cinderline run` at login and starts it now; `cinderline status`, `cinderline logs` and `cinderline stop` talk to it; `cinderline service uninstall` stops and removes it. On Linux it needs systemd. `cinderline stop` stops it until the next login. Linux and Windows restart it after a crash; macOS does not, so check `cinderline status` there.

`cinderline uninstall` asks for confirmation (`-yes` skips it), then removes the service, the runner's containers, networks, volumes and its own image, the config and state folders and every project's secrets. It lists each project's Cinderline URL and key: revoke that runner token in Cinderline yourself. It leaves the binary and prints its path for you to delete.

To pin a specific version, set `CINDERLINE_DOWNLOAD_URL` before running (e.g.
`https://github.com/rodo-software/cinderline-runner/releases/download/v0.1.0`).

## Update

`cinderline update` downloads and verifies the latest release, refreshes the tools volume and default image, replaces the binary, and lets the running runner finish its runs before it stops; `cinderline stop --drain` does only the last part. If the service is installed, `cinderline update` starts it again with the new binary.

Agent containers run the default image, `ghcr.io/rodo-software/cinderline-runner-default`, pulled by a pinned digest. Offline, or with `"build_image": true` in `config.json`, the runner builds it locally from `images/default/Dockerfile` instead.

## Build

```
go build ./cmd/cinderline
./cinderline version
```

## Checks

```
gofmt -l .
go vet ./...
staticcheck ./...
go test -race ./...
govulncheck ./...
```

See [AGENTS.md](AGENTS.md) and [docs/runner-rewrite-plan.md](docs/runner-rewrite-plan.md).

## CI secrets

The runner's agents push to `cinderline/<REF>` branches. Any CI secret that is available to every branch can be read by code an agent wrote and committed. `cinderline add` warns when it detects such secrets.

### GitHub

Move secrets from repository or organisation secrets to an environment (Settings → Environments) whose "Deployment branches and tags" is set to protected branches or selected branches, or has required reviewers enabled. Reference the environment with `environment:` in the workflow.

### GitLab

Mark CI/CD variables as "Protected" (Settings → CI/CD → Variables), for both project and group variables, so that only protected branches and tags receive them. Ensure `cinderline/*` is not a protected branch pattern.

### Forgejo / Gitea

Actions secrets have no branch protection: every workflow on every branch can read the repository's and its organisation's secrets, whatever its trigger. Keep deploy secrets out of both, for example by deploying from a separate repository the runner does not push to.

### Bitbucket Cloud

Repository and workspace variables reach every branch. Move them to deployment environments and restrict which branches or users can deploy (deployment permissions, Premium tier). The check cannot read those permissions via the API, so it warns when deployment variables exist.

## A Cinderline on the same machine

Agent containers reach the host as `host.docker.internal`, so a Cinderline running on the same machine is configured as `http://host.docker.internal:<port>`. On Linux that name points at the Docker bridge address, not at `127.0.0.1`: a backend that listens only on `127.0.0.1` (for example `php artisan serve`) is not reachable from the agents. Let it listen on the bridge address or on all addresses, for example `php artisan serve --host=0.0.0.0`. Docker Desktop on macOS and Windows forwards to the host's loopback, so there it works either way.

## Licence

The runner is fair source, not open source: it is licensed under the [Functional Source License, Version 1.1, MIT Future License](https://fsl.software) (FSL-1.1-MIT); see [LICENSE](LICENSE) and [NOTICE](NOTICE). You may read, modify and run it, also against your own backend for internal use, but not ship or host it as part of a commercial product or service that competes with Cinderline. Each version becomes available under the MIT licence two years after its release. Commercial licences are available from Rodo Software.
