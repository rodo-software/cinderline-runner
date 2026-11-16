# The default image agent containers run in: today's toolchain, PHP 8.4,
# Composer, Node 22, git and Go with staticcheck and govulncheck, without
# agent CLIs, which come from the tools volume (plan M4). Published on GHCR
# by docs/ci/default-image.yml; the runner pulls it by a pinned digest and
# builds it locally when offline, when config.json sets build_image, or when
# this file changed since the pinned image. Runs as uid 1000, the user the
# workspace's git runs as, which Claude Code also requires for
# --dangerously-skip-permissions.
#
# Go is here so this runner's own project can switch in M4 (decision
# 2026-11-06-default-image-go); one image can't hold every project's
# toolchain, which needs a more flexible solution later (CSR-77).
FROM php:8.4-cli-bookworm

COPY --from=mlocati/php-extension-installer /usr/bin/install-php-extensions /usr/local/bin/
COPY --from=composer:2 /usr/bin/composer /usr/local/bin/composer
COPY --from=golang:1.27.4-bookworm /usr/local/go /usr/local/go

# gcc and libc6-dev: go test -race needs cgo.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git unzip gcc libc6-dev \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/* \
    && install-php-extensions bcmath intl pdo_mysql zip \
    && useradd --create-home --uid 1000 runner

# GOTOOLCHAIN=local makes a newer go line in a project's go.mod fail loudly
# instead of silently downloading another toolchain; bump the Go version here.
ENV PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local
RUN GOBIN=/usr/local/bin go install honnef.co/go/tools/cmd/staticcheck@v0.8.2 \
    && GOBIN=/usr/local/bin go install golang.org/x/vuln/cmd/govulncheck@v1.9.0 \
    && rm -rf /root/go /root/.cache/go-build

USER runner
