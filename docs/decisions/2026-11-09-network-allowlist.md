# The network allowlist of agent containers

- **Status:** Accepted
- **Date:** 2026-11-09
- **Source:** Decided by Willem on CSR-63 (the questions of CSR-62, asked there), five questions, each with the option proposed by Claude except the third. Settles plan M8's open item and decision 2026-11-05-runner-rewrite-details point 18; replaces point 8 for agent containers.

## Decision

1. **Default list, per run, exact host:port, no wildcards.**
   - The model provider of that run's client only: `api.anthropic.com` for Claude Code, `openrouter.ai` for OpenCode, `api.openai.com` (or the host:port of the configured `openai_base_url`) for Codex, and `generativelanguage.googleapis.com` (or `aiplatform.googleapis.com` for a Vertex key) for Gemini CLI. A Claude Code run cannot reach OpenRouter, and the reverse.
   - The host:port of the project's Cinderline URL, for MCP.
   - The package registries: `repo.packagist.org`, `packagist.org`, `codeload.github.com`, `api.github.com`, `registry.npmjs.org`, `proxy.golang.org`, `sum.golang.org`, all on port 443.
   - No forge: git fetch and push run in the helper container, which holds the git token and is not on the run's network.
2. **Only local config extends it**: a project's optional `allow_hosts` in `config.json`, a list of host:port entries. A list managed from Cinderline is a separate backlog issue, for when a second runner machine needs it.
3. **`host.docker.internal` is never allowed.** The agent container loses `--add-host host.docker.internal:host-gateway`. The runner runs no local model backend (decision 2026-11-11-no-local-model-work).
4. **`packagist.org:443` is a registry.** Composer calls it at the end of every `composer install` and `composer require` (Composer 2.11.1, observed behind a logging proxy).
5. **Runs on a stored sign-in** (no key, CSR-58) also get the provider's sign-in hosts, unchecked against the CLIs: `console.anthropic.com` and `platform.claude.com` for Claude Code; `chatgpt.com` and `auth.openai.com` for Codex; `cloudcode-pa.googleapis.com` and `oauth2.googleapis.com` for Gemini CLI. A missing host shows up as a refused host in the run's log.

## Reason

The allowlist stops a model, or an injected issue body, from sending data anywhere other than the hosts the run needs. Combined with an internal network, it fails closed: a process that ignores the proxy variables has no route and no DNS. It cannot stop data from reaching the run's own provider; the agent CLI is that provider's client. It also cannot stop data sent through an allowed host (a Cinderline comment, a registry). Those land where a person reads them, or with a party already trusted.

- Only the run's own provider: every additional host is another way out.
- Local config only: whoever can add a host can open a way out. An allowlist that can be edited from Cinderline would open one on every runner of a project after a single stolen admin session.
- No `host.docker.internal`: it reaches every port the host machine listens on, not only a model backend.
- `packagist.org`: without it, every Composer run would add a refused host, and the refused count is the detector for a model that probes once in 100,000 turns. Packagist already runs `repo.packagist.org`.
- Sign-in hosts: same provider as the run already trusts. Without them, a Codex run on a ChatGPT sign-in cannot work at all.

Rejected: registries left out (runs fail on `composer install` and `npm ci`); github.com and the forge in the list (an injected prompt could push anywhere); Cinderline-managed extensions now; `host.docker.internal` on its exact port or on any port; packagist.org left out, or left out of the count; sign-in hosts only through `allow_hosts`.
