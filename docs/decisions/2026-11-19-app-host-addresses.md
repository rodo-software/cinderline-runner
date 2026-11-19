# App-approved hosts cannot open the runner's private network

- **Status:** Accepted
- **Date:** 2026-11-19
- **Source:** Willem on CLR-205, answers of 2026-11-19: “A: Preserve local access” and “A: Include proxy DNS checks”. Refines decision 2026-11-17-hosts-from-cinderline, point 2.

## Decision

1. Local `allow_hosts` entries keep their existing private-network access. Their existing restrictions on localhost, loopback and `host.docker.internal` stay in place. App approvals do not inherit this permission.
2. Drop app-approved IP literals that are private, link-local unicast or unspecified, as well as the runner-machine addresses already refused, with the existing host-drop log event. Continue the step with the remaining entries.
3. Build the proxy protection for DNS names now too: before connecting to an app-approved target, reject every resolved address that is private, loopback, link-local unicast or unspecified. Apply the check to HTTP forwarding and CONNECT, at the actual socket connection, so a DNS change cannot bypass it. Log refusals by the original host and port for the existing refused-host reporting.
4. Carry app-approved entries separately from provider and local-config entries into the proxy. An explicit local permission for the same host and port retains its existing access, regardless of an app approval for that entry.

## Reason

The machine owner controls local config and may need an internal registry. A project member approving a host in the app need not own the runner or its network. Rejecting only IP literals leaves the same private-network access available through a DNS name, so both protections belong in this fix. Keeping the source of permission preserves owner-configured access while limiting app approvals.
