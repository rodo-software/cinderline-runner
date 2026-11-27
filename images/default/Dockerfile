# The default image agent containers run in: PHP 8.4 with bcmath, gd, intl,
# pdo_mysql and zip and no memory_limit on the command line (as setup-php and
# most developer machines run it), Composer, Node 22, git, procps (kill, ps,
# pkill) and a headless Chromium, without agent CLIs, which come from the
# tools volume (plan M4). A project that needs another toolchain uses the
# docker environment with its own image (decision
# 2026-11-16-project-toolchains). Published on GHCR by
# .github/workflows/default-image.yml; the runner pulls it by a pinned digest,
# and builds it locally only when this file changed since the pinned image
# (decision 2026-11-24-default-image-amd64). Runs as uid 1000, the user the
# workspace's git runs as, which Claude Code also requires for
# --dangerously-skip-permissions.
#
# Headless Chromium (CLR-319), for agents that render or screenshot a page
# without downloading a browser: Debian's package, as Google ships no
# linux/arm64 Chrome and Playwright's own download needs the network at run
# time. The variables point Puppeteer and Playwright at it; Playwright still
# needs chromium.launch({ executablePath: process.env.CHROME_PATH }). The
# flags file adds to every launch: --no-sandbox, as Chromium's sandbox needs
# new user namespaces, which the run container's seccomp profile, no
# capabilities and no-new-privileges refuse; the container is the sandbox and
# a renderer gets no more than the agent already has.
# --disable-background-networking, so it asks the egress proxy for no update
# or Google hosts. --disable-dev-shm-usage, as Docker's /dev/shm is 64 MB.
FROM php:8.4-cli-bookworm

COPY --from=mlocati/php-extension-installer /usr/bin/install-php-extensions /usr/local/bin/
COPY --from=composer:2 /usr/bin/composer /usr/local/bin/composer

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git procps unzip \
        chromium fonts-liberation fonts-noto-color-emoji \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/* \
    && install-php-extensions bcmath gd intl pdo_mysql zip \
    && echo 'memory_limit = -1' > /usr/local/etc/php/conf.d/memory-limit.ini \
    && echo 'export CHROMIUM_FLAGS="$CHROMIUM_FLAGS --no-sandbox --disable-background-networking --disable-dev-shm-usage"' > /etc/chromium.d/no-sandbox \
    && useradd --create-home --uid 1000 runner

ENV CHROME_PATH=/usr/bin/chromium \
    PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium \
    PUPPETEER_SKIP_DOWNLOAD=1 \
    PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1

USER runner
