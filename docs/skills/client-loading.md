# How the pinned agent CLIs load skills and plugins

CLR-256, 2026-11-26. This covers the versions pinned in `internal/tools/pins.go`: Codex 0.167.0, Gemini CLI 0.67.0, OpenCode 1.19.5 and Claude Code 2.1.316. It feeds the skill-package mounts (CLR-258; app decision in CL-425).

## Open points

These need a person or a real model turn. `scripts/skills-spike.sh` runs the real-model checks with Docker and real keys, one command per check.

1. **Real-model behaviour.** A fake model checked what each CLI sends and runs, but no real model was asked to use a skill or to act on a hook's context. Run `scripts/skills-spike.sh all`.
2. **Codex full packages need a decision.** Codex runs a package's hooks only as an installed plugin with `features.plugins=true`. That route has three costs:
   - Every turn then tries `github.com`, `api.github.com` and `chatgpt.com`, and no switch for that was found.
   - It needs an install step that writes into `$CODEX_HOME`.
   - Each hook needs a trust hash.

   The other route is to pass the hooks as `-c hooks=…` with plugins off. Codex then sets no plugin root, which ponytail's hook commands need, and the runner would have to rewrite the package's hook commands. **Recommendation:** plain skills only for Codex, until a pin can switch the curated-plugin sync off.
3. **Hooks need their interpreter in the image.** Ponytail's hooks run `node`. The default image has node, but this repository's own `.cinderline/Dockerfile` (golang) does not. There, Claude Code's ponytail hooks fail today with `node: not found` (exit 127). The turn goes on, the skills still load, but the ruleset is not added. The volume's node is kept off `PATH` on purpose (`internal/tools/tools.go`, `RgDir`). Choose between: (a) accept that hooks need the interpreter in the image, or (b) give hook-running turns a way to reach the volume's node.
4. **Codex trust hashes.** It is not checked whether they are the same on another machine or another install path. They held across processes on one machine.
5. **impeccable was not checked.** github.com is refused here, so only ponytail (from the tools volume) and probe packages were used.
6. **Gemini skill names are not namespaced.** Two packages that ship a skill with the same folder name collide in `.agents/skills/<skill>`.

## How it was checked

- **Run.** The pinned binaries from the tools volume (`/opt/cinderline`, versions confirmed with `--version`) ran one headless turn each, with the runner's argv and environment from `internal/client/*.go`. Each pointed at a local fake model through its own base-URL setting: `ANTHROPIC_BASE_URL`, `-c openai_base_url=…`, `GOOGLE_GEMINI_BASE_URL`, and an `@ai-sdk/openai-compatible` provider for OpenCode.
  - The fake logged every request body and answered with either text or one scripted tool call. So "loaded" means the skill, agent or context text was in the request the model got. "Fired" means the hook's side effect happened and its output reached the model.
  - Packages were read-only (mode `a-w`, or ponytail owned by root) and partly symlinked.
  - Codex, Gemini and OpenCode ran behind a logging proxy that refuses every host, so any other traffic was recorded.
  - This container has no Docker, so nothing ran in an agent image, as user 1000 in a real image, or behind the runner's proxy.
- **Read.** Strings and JS source were read from the same binaries: the Codex and OpenCode executables and the Gemini bundle. github.com is refused here, so the upstream source repositories were not read.

## Codex 0.167.0

| # | Question | Answer | How |
|---|---|---|---|
| 1 | Does `codex exec` load skills, and from where? | Yes. Roots in the prompt, in this order: `$CODEX_HOME/skills`, `$HOME/.agents/skills`, `$CODEX_HOME/skills/.system` (Codex's own skills, written there on every start) and `<repo>/.agents/skills` (walked up from the working directory). `~/.codex/skills` is not read when `CODEX_HOME` is elsewhere. `/etc/codex/skills` is not a root: the binary names only `/etc/codex/{config,requirements,managed_config}.toml`. | Run; `/etc` read only (no root here to create it). |
| 1 | Which root can be a read-only mount while `CODEX_HOME=/state/codex`? | `$CODEX_HOME/skills` itself must not be: with it read-only, Codex logs `failed to install system skills … Permission denied` and loses its system skills, though user skills still load. A read-only mount at **`$CODEX_HOME/skills/<pkg>`** works. Codex searches it recursively (`<pkg>/skills/*/SKILL.md` was found) and follows symlinks. Ponytail's same-named copies in `.openclaw/skills` were not listed (it is not clear whether as a dot-folder or as a duplicate). `$HOME/.agents/skills/<pkg>` works too, but `HOME` differs per image (`/home/runner` in the default image, `/tmp` in this repo's). | Run. |
| 1 | Is `$CODEX_HOME/skills` deprecated? | Not in 0.167.0's behaviour: it is root `r0`, with no warning. The deprecation mark is from source reading only. | Run. |
| 2 | Do `-c features.plugins=false` and `-c project_doc_max_bytes=0` switch skills off? | No. Every root above loaded with both flags. `skills.config` only switches a skill on or off by `path` or `name`; it cannot add a root (a `path` outside the roots did not load). | Run. |
| 3 | Can Codex load a local plugin without the github.com sync? | **Only partly.** Installing works offline: `codex plugin marketplace add <dir>` with `.agents/plugins/marketplace.json` entries `{"source":{"source":"local","path":"./<pkg>"}}`, then `codex plugin add <pkg>@<marketplace>`, which copies to `$CODEX_HOME/plugins/cache`. The plugin's skills and hooks need `features.plugins=true`, though, and every turn then tries `github.com`, `api.github.com` and `chatgpt.com`. `--disable remote_plugin,plugin_discovery_prefetch,plugin_sharing,apps,tool_suggest` did not stop it, and no other switch is in the binary. The turn still works with those hosts refused. Ponytail's own `.agents/plugins/marketplace.json` points at github.com `main`, not a pin, so a runner marketplace must be its own. | Run, behind the logging proxy. |
| 3 | Is there a hook mechanism usable headless? | Yes, for `SessionStart`, `PostToolUse`, `UserPromptSubmit`, `SubagentStart` and more. Hooks come from `$CODEX_HOME/hooks.json`, from `-c 'hooks={SessionStart=[{hooks=[{type="command",command="…"}]}]}'` and from enabled plugins. Codex runs a hook only when it is trusted, and skips untrusted ones without a message. Trust can be passed per turn: `-c 'hooks.state={"<key>"={trusted_hash="sha256:…"}}'`. A plugin hook also needs `trusted_content_hash`. `codex app-server` method `hooks/list` gives each hook's key, `currentHash` (for a plugin: `<hash>/<content hash>`) and `trustStatus`. A trusted ponytail plugin hook ran, so Codex sets `CLAUDE_PLUGIN_ROOT` for plugin hooks, and its context reached the model. | Run. |

| Runner should use | |
|---|---|
| Plain skills | Read-only mount of the package at `/state/codex/skills/<pkg>` (`$CODEX_HOME/skills/<pkg>`). No flag. |
| Full package with hooks | Not recommended with 0.167.0 (open point 2). Possible with `features.plugins=true`, a local marketplace, an install step and trust hashes, at the cost of github.com, api.github.com and chatgpt.com traffic on every turn. |

## Gemini CLI 0.67.0

| # | Question | Answer | How |
|---|---|---|---|
| 4 | Does a headless `-p --yolo --skip-trust` turn load skills from `$GEMINI_CLI_HOME/.gemini/skills`? | Yes. It also loads `$GEMINI_CLI_HOME/.agents/skills`, `<repo>/.agents/skills` (only because of `--skip-trust`; without it project skills are skipped) and each extension's `skills/`. A root is searched one level deep only (`SKILL.md`, `*/SKILL.md`), so a whole package mounted as `<root>/<pkg>` is **not** found. Mount each skill folder instead. | Run; depth read in `loadSkillsFromDir` and run with `gemini skills list`. |
| 4 | Does `activate_skill` need a consent it cannot get headless? | No. With `--yolo`, the model's `activate_skill` call succeeded and the next request had the skill body. | Run. |
| 5 | Does `~/.agents/skills` follow `GEMINI_CLI_HOME`? | Yes. `homedir()` returns `GEMINI_CLI_HOME` first, so it is `$GEMINI_CLI_HOME/.agents/skills`. The real `$HOME/.agents/skills` is not read. | Read and run. |
| 6 | Can an extension load from a read-only folder without an install step or network? | Yes. A read-only folder or symlink at `$GEMINI_CLI_HOME/.gemini/extensions/<name>` with `gemini-extension.json` loads with no `.gemini-extension-install.json` and no network: the logging proxy saw nothing. Ponytail's manifest loaded as is. | Run. |
| 6 | Do extension hooks and context files work headless? | Yes. The context file (`contextFileName`; ponytail's is `AGENTS.md`) is in every request. `<ext>/hooks/hooks.json` hooks (`SessionStart`, `BeforeAgent`, `AfterTool`) fired, and SessionStart's `additionalContext` reached the model. `hooksConfig.enabled` is on by default. | Run. |

| Runner should use | |
|---|---|
| Plain skills | One read-only mount per skill folder: `<pkg>/skills/<skill>` at `/state/gemini/.agents/skills/<skill>` (`$GEMINI_CLI_HOME/.agents/skills`). No flag. |
| Full package with hooks | If the package ships `gemini-extension.json`: read-only mount of the package at `/state/gemini/.gemini/extensions/<pkg>`. This brings its skills, context file and `hooks/hooks.json`. No flag. A package without a manifest gets plain skills only. |

## OpenCode 1.19.5

| # | Question | Answer | How |
|---|---|---|---|
| 7 | Does `skills.paths` in `OPENCODE_CONFIG_CONTENT` load skill folders from an absolute path? | Yes. Each path (absolute, relative to the working directory, or `~/`) is searched for `**/SKILL.md`, following symlinks and skipping dot-folders: ponytail's `.openclaw/skills` copies were ignored. The skill reached the model. Skills also load, without config, from `$HOME/.agents/skills`, `$HOME/.claude/skills` and `<repo>/.agents/skills`. | Read in the binary (`skills.paths`, glob `**/SKILL.md`); run with `opencode debug skill` and a turn. |
| 8 | Can a local plugin be named in the inline config by path, without an npm install at run time? | Yes. `"plugin": ["/abs/path/entry.mjs"]`, or a `file://` URL, is imported directly. Only a bare package name goes through the npm install. Ponytail's `.opencode/plugins/ponytail.mjs` loaded: its ruleset was in the system prompt and it registered its skills. Its imports are Node built-ins and files in the package; a plugin that imports npm packages would need them shipped in the package. | Read (`isAbsolute(package) ? file URL : npm add`) and run behind the logging proxy. The only npm traffic was the background install noted below, which happens with or without the plugin. |

| Runner should use | |
|---|---|
| Plain skills | Read-only mount of the package anywhere, plus `"skills": {"paths": ["<mount>/skills"]}` in `OPENCODE_CONFIG_CONTENT`. |
| Full package with hooks | Same mount, plus `"plugin": ["<mount>/<the package's OpenCode entry file>"]` (ponytail: `.opencode/plugins/ponytail.mjs`). The entry file has to be named per package: ponytail's `opencode.json` holds only `$schema`. |

## Claude Code 2.1.316

| # | Question | Answer | How |
|---|---|---|---|
| 9 | `--plugin-dir` repeated? | Works. Three `--plugin-dir` flags loaded all three. | Run. |
| 9 | A folder without a manifest, with `skills/`? | Loads. Its skills are named `<folder>:<skill>`. But a single `--plugin-dir <parent>` loads only the children that have `.claude-plugin/plugin.json`, so give one flag per package. | Run. |
| 9 | Hooks and `agents/` of a plugin under `-p`? | Fire and load. SessionStart (its `additionalContext`), UserPromptSubmit and PostToolUse fired; the agent was in the request. Ponytail's hooks need `node` on `PATH`. Without it they fail with exit 127, the turn goes on and the skills still load. | Run. |
| 9 | Do `CLAUDE_CODE_DISABLE_CLAUDE_MDS=1` and `--strict-mcp-config` leave plugins and skills alone? | Yes. Plugin skills, agents and hooks all loaded with both set, while the repository's `CLAUDE.md` did not reach the model. | Run. |
| 10 | What keeps plugins loading if `--bare` becomes the default for `-p`? | 2.1.316 turns bare mode on only through `--bare` or `CLAUDE_CODE_SIMPLE=1`. Nothing makes it the default, and there is no `--no-bare` or other opt-out in the binary. Under `--bare`, `--plugin-dir` plugins load **nothing**: no skills, agents or hooks, and only Bash, Edit and Read as tools. That is despite the help text saying context can be given with `--plugin-dir`. Bare mode also reads only `ANTHROPIC_API_KEY`, never an OAuth (`setup-token`) token. So no flag keeps plugins today. When a pin makes bare the default, check that version's opt-out. | Run (`--bare` turn); read (help and binary strings). |

| Runner should use | |
|---|---|
| Plain skills | Read-only mount of the package anywhere, plus `--plugin-dir <mount>` per package. A manifest is not needed. |
| Full package with hooks | The same. Hooks run in the agent image and need their interpreter there (open point 3). Never pass `--bare`. |

## Seen on the way, not about packages

Not changed in this issue:

- Every OpenCode 1.19.5 turn, with or without a plugin, tries `registry.npmjs.org` for a "background dependency install" into `$XDG_CONFIG_HOME/opencode`. It does so again on the second turn of the same state, and the turn works with it refused.
- Every Codex 0.167.0 turn with the runner's flags tried `ab.chatgpt.com` in this setup (an API key and `openai_base_url` set to the fake model). It is not checked whether a turn against OpenAI's own API does the same.
