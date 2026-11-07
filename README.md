# CinderStack runner

The CinderStack runner is a native Go program on a developer's machine that takes To do issues from a CinderStack instance, runs an agent team on each in its own Docker container, pushes a branch and opens a pull request.

Status: milestone M0, the repository skeleton only.

## Install

Install the latest `cinderstack` binary with one line — no sudo or elevation required. Each script downloads the archive for your OS and architecture, verifies its SHA-256 checksum against the release's `checksums.txt` (refusing on mismatch), and installs the binary.

**macOS / Linux:**

```
curl -fsSL https://raw.githubusercontent.com/rodo-software/cinderstack-runner/main/scripts/install.sh | sh
```

Installs to `~/.local/bin/cinderstack`. If that directory is not on your `PATH`, the script prints a line to add to your shell profile.

**Windows (PowerShell 5.1+):**

```
irm https://raw.githubusercontent.com/rodo-software/cinderstack-runner/main/scripts/install.ps1 | iex
```

Installs to `%LocalAppData%\Programs\cinderstack\cinderstack.exe`. If that directory is not on your user `PATH`, the script prints a command to add it.

To pin a specific version, set `CINDERSTACK_DOWNLOAD_URL` before running (e.g.
`https://github.com/rodo-software/cinderstack-runner/releases/download/v0.1.0`).

## Build

```
go build ./cmd/cinderstack
./cinderstack version
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

## A CinderStack on the same machine

Agent containers reach the host as `host.docker.internal`, so a CinderStack running on the same machine is configured as `http://host.docker.internal:<port>`. On Linux that name points at the Docker bridge address, not at `127.0.0.1`: a backend that listens only on `127.0.0.1` (for example `php artisan serve`) is not reachable from the agents. Let it listen on the bridge address or on all addresses, for example `php artisan serve --host=0.0.0.0`. Docker Desktop on macOS and Windows forwards to the host's loopback, so there it works either way.

## Licence

The runner is fair source, not open source: it is licensed under the [Functional Source License, Version 1.1, MIT Future License](https://fsl.software) (FSL-1.1-MIT); see [LICENSE](LICENSE) and [NOTICE](NOTICE). You may read, modify and run it, also against your own backend for internal use, but not ship or host it as part of a commercial product or service that competes with CinderStack. Each version becomes available under the MIT licence two years after its release. Commercial licences are available from Rodo Software.
