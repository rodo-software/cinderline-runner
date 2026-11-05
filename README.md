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

## Licence

MIT, see [LICENSE](LICENSE) and [NOTICE](NOTICE).
