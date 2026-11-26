# An agent CLI's own network traffic: switch it off, else allow it per client

- **Status:** Accepted
- **Date:** 2026-11-25
- **Source:** Willem, CLR-252; CL-399 shows the problem. Adds to decision 2026-11-09-network-allowlist (in `archive/`) and replaces none of it.

## Decision

An agent CLI's own background traffic is not chosen by the model, and a person should not be asked about it. This covers model catalogs, update checks, plugin syncs and tool downloads.

1. **Switch it off or bundle it where the pinned version allows.** Use a flag, a config key or an env var, or ship the tool in the tools volume. This is the default. Examples: Codex's `features.plugins=false` and `features.api_key_model_discovery=false` (CLR-242), and OpenCode's `OPENCODE_DISABLE_MODELS_FETCH=1` and pinned `rg` (CLR-250, CLR-251).
2. **Only if it cannot be switched off and the client needs it to work**, add the host to that client's default `hosts` in `internal/client/<client>.go`. Add a comment naming the pinned version that needs it and how that was checked. Per client only, never for every run.
3. **Never through `Registries` or `allow_hosts`.** Those are for the project's own work.

## Reason

The proxy sees only host names. Allowing a general host such as `github.com` for normal operation would also let every model reach any page on it, so a switch beats an allowed host. Traffic that is switched off or bundled also makes turns faster and the same for every run.

## The clients checked against it (CLR-252, 2026-11-25)

Codex and OpenCode were already checked (CLR-242, CLR-250, CLR-251). Claude Code 2.1.316 and Gemini CLI 0.67.0 were checked with the same binaries the tools volume pins: the Claude Code linux-arm64 binary matched its pinned SHA-256, and the Gemini npm tarball matched its pinned integrity. Each ran one headless turn with the runner's argv, settings and env, behind a logging TLS-intercepting proxy. That proxy answered the client's own model hosts with a fake model API and refused every other host, first after TLS so the path was logged, then at CONNECT as the runner's proxy does. Real keys and sign-ins were not available. The key runs used a dummy key, and the sign-in runs used dummy credentials in the CLI's own sign-in file.

- **Claude Code 2.1.316.** With a key, a `setup-token` token or a stored sign-in, it reaches only `api.anthropic.com`. That held for a fresh or resumed session, with MCP, `--model`, `--effort` and `--plugin-dir`. Turns already get `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` and `DISABLE_AUTOUPDATER=1`. Without them, the turn adds feature flags (`/api/eval/sdk-…`), `/api/claude_cli/bootstrap`, `/api/claude_code_penguin_mode`, `/api/claude_code/organizations/metrics_enabled` and event logging (`/api/event_logging/v2/batch`), all on `api.anthropic.com`. No other host was reached, so nothing to change.
- **Gemini CLI 0.67.0.** With a Gemini API key or a Vertex express key, it reaches only its provider host. Without `privacy.usageStatisticsEnabled: false`, which turns already get, it also posts usage logs to `play.googleapis.com`. With a stored Google sign-in, it reaches `oauth2.googleapis.com` and `cloudcode-pa.googleapis.com`, both in its `hosts`. It also reaches `www.googleapis.com` for `GET /oauth2/v2/userinfo`, but only when the state volume has no cached Google account (`.gemini/google_accounts.json`). That call fetches the account's email for telemetry, and its failure is ignored: the turn succeeded with it refused. The CLI's own sign-in, which `login` runs on the open network, caches the account, so turns after a normal `login` do not make it. 0.67.0 has no switch for it, and the client does not need it, so it is neither switched off nor allowed (rule 2 does not apply).
