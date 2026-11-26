# Skill packages: every turn mounts them read-only, and each client loads them as fully as it can

- **Status:** Accepted
- **Date:** 2026-11-26
- **Source:** Willem, issue CLR-258 (2026-11-26), the runner's side of the app's decision `2026-11-26-skill-packages` (CL-425), built on what CLR-256 found (`docs/skills/client-loading.md`). Point 2 is Willem's choice of 2026-11-26. Point 4 follows the issue's rule. Points 7 to 11 are the implementation's choices in CLR-258, open to Willem's veto in its review. Point 6 is the default while Willem's answer to CLR-256 open point 3 is pending. Replaces point 7 of 2026-11-26-skill-package-fetch.

## Decision

1. **Every turn of a run mounts every package it loaded**: the orchestrator's, the workers' and the Merge Resolver's, for every client. A package's repository is mounted read-only at `/skills/<name>` (`workspace.SkillsPath`), and its root is `/skills/<name>[/<path>]`. The check step does not get the packages.
2. **Claude Code** gets one `--plugin-dir /skills/<name>[/<path>]` per package, so its hooks, subagents and skills load. `--strict-mcp-config` stays, so a package's own MCP servers stay off. Never `--bare`: under it, plugins load nothing (CLR-256).
3. **Gemini CLI.** A package with `gemini-extension.json` in its root is mounted as an extension at `$GEMINI_CLI_HOME/.gemini/extensions/<name>`, which brings its skills, context file and hooks. Any other package is mounted as plain skills, one mount per folder of its `skills/` that holds a `SKILL.md`, at `$GEMINI_CLI_HOME/.agents/skills/<skill>`.
4. **Codex** gets plain skills only: the package's `skills/` at `$CODEX_HOME/skills/<name>`. Its hooks would need the plugin installed with `features.plugins` on, which takes an install step and github.com, api.github.com and chatgpt.com on every turn (CLR-256 open point 2). The issue allows neither.
5. **OpenCode.** Its inline config gets `skills.paths` with each package's `skills/`, and `plugin` with the package's plugin files, each by absolute path, so nothing is installed. The plugin files are those OpenCode itself loads from a project: `.opencode/{plugin,plugins}/*.{ts,js}` (from the 1.19.5 binary). For ponytail that is `index.js`, which re-exports `ponytail.mjs`.
6. **Hooks and scripts need their interpreter in the project's image.** The runner adds nothing to `PATH`, so ponytail's `node` hooks fail in an image without node. The turn goes on, and the skills still load.
7. **The runner reads what a package holds when it checks its volume** (`tools.Skills.Fill`), in the same read-only container that checks the volume's path. It reads three things: the skill folders, the Gemini manifest, and the OpenCode plugin files. It lists only names of letters, digits and `. _ -` that do not start with a dot and are not reached through a symlink, and Go checks each line again. So nothing else of a package becomes a path or a mount, and every subpath stays a folder of the volume.
8. **The client folders the mounts go into are a tmpfs per turn**: `$CODEX_HOME/skills`, `$GEMINI_CLI_HOME/.gemini/extensions` and `$GEMINI_CLI_HOME/.agents/skills`, but only on turns that mount something there.
   - **Why:** docker would otherwise create each mount point in the project's shared state volume, owned by root. An issue's packages would then leave empty folders that other issues' turns see, and Gemini CLI warns about an extension folder without a manifest.
   - Codex writes its system skills into the tmpfs.
   - `PrepareState` creates those folders and their parents for the agent user. Otherwise docker would create a missing parent for root, and the CLI could not write its home.
9. **Gemini CLI skill names clash:** a later package's skill folder that has the same name as an earlier package's plain skill is left out, and the run logs it. Docker refuses two mounts at one place. An extension's skills are Gemini CLI's own to name. (CLR-256 open point 6.)
10. **Nothing is added to the allowlist** for a package. A host that a package's script or hook tries is refused and reported like any other refused host.
11. **The ponytail pin is gone.** This covers `Team.ponytail`, `Turn.Ponytail`, the pinned clone in the tools volume (whose hash changes with it) and its line in the release notes. Ponytail is now a catalog package like any other.

## Reason

Each client's own mechanism keeps a package whole: its skills, its hooks and its reference files, without writing into the clone, which must stay clean for the pull request. Where a client cannot load the whole package without an install step or network, it gets the most it can load safely, at least the skills.

The tmpfs keeps one issue's packages from leaking into the state volume that every issue of the project shares. Reading the package's contents in the runner's check, rather than in a shell in the agent's container, keeps each turn's mounts known before it starts and testable.
