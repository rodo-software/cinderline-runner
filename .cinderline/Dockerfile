# This project's agent image in the docker environment (decision
# 2026-11-16-project-toolchains point 2): the pins of images/default/Dockerfile
# for Go, without PHP, Composer and Node. Agent CLIs come from the tools
# volume. The golang bookworm image already holds git, and gcc and libc6-dev
# for go test -race.
FROM golang:1.27.4-bookworm

# GOTOOLCHAIN=local makes a newer go line in go.mod fail loudly instead of
# silently downloading another toolchain; bump the Go version here.
ENV GOTOOLCHAIN=local
RUN GOBIN=/usr/local/bin go install honnef.co/go/tools/cmd/staticcheck@v0.8.2 \
    && GOBIN=/usr/local/bin go install golang.org/x/vuln/cmd/govulncheck@v1.9.0 \
    && go clean -cache -modcache

# The runner runs the image as 1000:1000, a uid without a home here, so the
# Go and staticcheck caches go to /tmp.
ENV HOME=/tmp GOPATH=/tmp/go
