# Security: what a skill package can reach

A skill package (app decision 2026-11-26-skill-packages) is code from a public git repository that the issue names. The runner mounts it into every turn of the issue's run, and the agent CLI runs it (decision 2026-11-26-skill-package-mounts). Treat a package like the repository's own code: it runs with everything the agent has.

## What a package can reach

The agent runs a package's code: Claude Code hooks, a Gemini CLI extension's hooks, an OpenCode plugin (which runs inside OpenCode itself), and any script a skill tells the model to run. That code runs in the agent's container, as the agent user `1000:1000`, with the same reach as the agent:

- **The turn's model credential.** This is the one key or sign-in token of the turn's client in the container's environment: an Anthropic key or `setup-token` token, an OpenAI key, a Gemini or Vertex key, an OpenRouter key, or the key of the runner's provider that the model names.
- **The client's state volume.** It holds that client's sessions for the project and its stored sign-in, if the project uses one.
- **The run's scoped Cinderline token** (`CINDERLINE_TOKEN`), on the orchestrator's turns only. Workers and the Merge Resolver run without one (0122). With the token a package can do what the agent can through Cinderline's MCP server: read and write the issues of its run's tree.
- **The issue's clone, read-write.** What a package changes there is committed and pushed as the issue's work, through the push scan, and a person reviews it in the pull request.
- **Read-only:** the project's repo volume, extra and linked repositories, the issue's attachments, the tools volume and the run's other packages.
- **The network, only through the run's egress proxy:** the client's provider, the project's Cinderline host, the package registries, the `allow_hosts` of config, and the hosts a person allowed for the issue (not on a strict runner).
  - These hosts accept data, so a package could send what it reads, including the credential and the token, to any of them.
  - It reaches nothing else: no forge and no host of its own.

It reaches nothing the agent cannot. No other secret enters the container: no forge token, no other client's key and no runner secret (AGENTS.md). The package's fetch ran outside the container, without a token.

## What keeps it in check

- **Pinned commit.** The app resolves a package's tag or branch to a commit once, and the runner fetches exactly that commit and refuses any other (2026-11-26-skill-package-fetch). A moved tag or a force-push upstream cannot change what runs. A new commit needs an admin's refresh in Cinderline, which the app logs.
- **Strict runners.** With `strict_hosts` on, a runner loads only the packages its owner allowed with `cinderline allow-skill`, by name and URL. A catalog entry that later points elsewhere is refused again.
- **Read-only, never in the clone.** A package cannot change itself between turns, cannot change another issue's copy, and never ends up in the pull request. Loading it needs no install step and no network.
- **The proxy.** Nothing is added to the allowlist for a package. A host it tries is refused and reported to Cinderline as a refused host. On a strict runner nobody can open it from Cinderline, and otherwise a person decides.
- **Its own MCP servers stay off.** Claude Code runs with `--strict-mcp-config`, and Codex with `features.plugins=false`, so Codex runs no package hooks at all.
- **The container's limits.** The container runs with no capabilities, `no-new-privileges` and a memory limit without swap, and mounts only its own issue's clone.

## What does not keep it in check

- A package's hooks run on every turn without asking, and the agents run with every permission (`--dangerously-skip-permissions`, `--yolo`, `--auto`, Codex's bypass). Whether a package's code is safe to run is decided when an admin adds it to the catalog and a member attaches it to an issue. On a strict runner it is also decided when the runner's owner allows it.
- The runner does not read or check a package's code. It checks only the fields that become paths and mounts.
