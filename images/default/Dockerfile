# The default image agent containers run in: today's toolchain, PHP 8.4,
# Composer, Node 22 and git, without agent CLIs, which come from the tools
# volume (plan M4). The runner builds it locally until M5 publishes it.
# Runs as uid 1000, the user the workspace's git runs as, which Claude Code
# also requires for --dangerously-skip-permissions.
FROM php:8.4-cli-bookworm

COPY --from=mlocati/php-extension-installer /usr/bin/install-php-extensions /usr/local/bin/
COPY --from=composer:2 /usr/bin/composer /usr/local/bin/composer

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git unzip \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/* \
    && install-php-extensions bcmath intl pdo_mysql zip \
    && useradd --create-home --uid 1000 runner

USER runner
