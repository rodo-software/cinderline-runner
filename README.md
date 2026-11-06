# CinderStack runner

The CinderStack runner is a native Go program on a developer's machine that takes To do issues from a CinderStack instance, runs an agent team on each in its own Docker container, pushes a branch and opens a pull request.

Status: milestone M0, the repository skeleton only.

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

MIT, see [LICENSE](LICENSE) and [NOTICE](NOTICE).
