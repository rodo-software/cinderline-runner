# Security

## Skill packages

A skill package (app decision 2026-11-26-skill-packages) is code from a public git repository that the issue names. The runner mounts it into every turn of the issue's run, and the agent CLI runs it (decision 2026-11-26-skill-package-mounts). Treat a package like the repository's own code: it runs with everything the agent has.

### What a package can reach

The agent runs a package's code: Claude Code hooks, a Gemini CLI extension's hooks, an OpenCode plugin (which runs inside OpenCode itself), and any script a skill tells the model to run. That code runs in the agent's container, as the agent user `1000:1000`, with the same reach as the agent:

- **The turn's model credential.** This is the one key or sign-in token of the turn's client in the container's environment: an Anthropic key or `setup-token` token, an OpenAI key, a Gemini or Vertex key, an OpenRouter key, or the key of the runner's provider that the model names.
- **The client's state volume.** It holds that client's sessions for the project and its stored sign-in, if the project uses one.
- **The run's scoped Cinderline token** (`CINDERLINE_TOKEN`), on the orchestrator's turns only. Workers and the Merge Resolver run without one (0122). With the token a package can do what the agent can through Cinderline's MCP server: read and write the issues of its run's tree.
- **The issue's clone, read-write.** What a package changes there is committed and pushed as the issue's work, through the push scan, and a person reviews it in the pull request.
- **Read-only:** the project's repo volume, extra and linked repositories, the issue's attachments, the tools volume, the run's other packages and the runner CA's certificate, never its key (see "The runner's CA" in the README). The run's proxy, a separate container, holds the key of the turn's own intermediate CA, valid for 48 hours, never the CA's key.
- **The network, only through the run's egress proxy:** the client's provider, the project's Cinderline host, the package registries, the `allow_hosts` of config, and the hosts a person allowed for the issue (not on a strict runner).
  - These hosts accept data, so a package could send what it reads, including the credential and the token, to any of them.
  - It reaches nothing else: no forge and no host of its own.

It reaches nothing the agent cannot. No other secret enters the container: no forge token, no other client's key and no runner secret (AGENTS.md). The package's fetch ran outside the container, without a token.

### What keeps it in check

- **Pinned commit.** The app resolves a package's tag or branch to a commit once, and the runner fetches exactly that commit and refuses any other (2026-11-26-skill-package-fetch). A moved tag or a force-push upstream cannot change what runs. A new commit needs an admin's refresh in Cinderline, which the app logs.
- **Strict runners.** With `strict_hosts` on, a runner loads only the packages its owner allowed with `cinderline allow-skill`, by name and URL. A catalog entry that later points elsewhere is refused again.
- **Read-only, never in the clone.** A package cannot change itself between turns, cannot change another issue's copy, and never ends up in the pull request. Loading it needs no install step and no network.
- **The proxy.** Nothing is added to the allowlist for a package. A host it tries is refused and reported to Cinderline as a refused host. On a strict runner nobody can open it from Cinderline, and otherwise a person decides.
- **Its own MCP servers stay off.** Claude Code runs with `--strict-mcp-config`, and Codex with `features.plugins=false`, so Codex runs no package hooks at all.
- **The container's limits.** The container runs with no capabilities, `no-new-privileges` and a memory limit without swap, and mounts only its own issue's clone.

### What does not keep it in check

- A package's hooks run on every turn without asking, and the agents run with every permission (`--dangerously-skip-permissions`, `--yolo`, `--auto`, Codex's bypass). Whether a package's code is safe to run is decided when an admin adds it to the catalog and a member attaches it to an issue. On a strict runner it is also decided when the runner's owner allows it.
- The runner does not read or check a package's code. It checks only the fields that become paths and mounts.

## The local web page

The runner serves a web page that shows its runs and changes its config and secrets (decision 2026-11-26-local-web-page; how to use it is in the README, "The web page"). Whoever gets into the page can do almost everything the `cinderline` commands can: add and remove projects, set and delete tokens and keys, open hosts to the agents, and stop or update the runner. These are the checks that keep everyone else out.

### Who can reach the port

- **Only the machine itself.** The page listens on `127.0.0.1` only, on port 7420 unless `web_port` says otherwise, and has no setting for any other address. It speaks plain HTTP, because the traffic does not leave the machine. Reaching it from another machine takes an SSH tunnel, which SSH encrypts, and the token is still needed there.
- **Not the agents.** Agent and check containers are on an internal Docker network whose only way out is the run's egress proxy, and a container's own `127.0.0.1` is not the machine's. The proxy refuses the runner's own machine (`localhost`, loopback addresses, `host.docker.internal`) and reports it as `runner_machine`. The one exception is the exact port of a model provider on the same machine, for a turn that uses it, and the config refuses such a provider on the page's port. `allow_hosts` never takes the runner's own machine, and a host Cinderline opens for an issue never does either. Tests in the proxy and the run prove that agents cannot reach the page's port.
- **Other users of the machine** can connect to the port. For them the token is the lock.

### The token and the cookie

- The token is 32 random bytes, written as hex in the file `web-token` in the state folder, which only your user can read (`0600`). It is made the first time it is needed. `cinderline web --new-token` replaces it. The page reads the file on every request, so a replaced token shuts out every open page at once.
- `cinderline web` prints the page's URL with the token (`http://127.0.0.1:7420/?token=…`) and opens it. With the right token, the page sets a cookie and redirects to `/`, so the token leaves the address bar. The cookie is `HttpOnly`, so scripts cannot read it, and `SameSite=Strict`, so other sites cannot send it. It lasts until the browser is closed. Tokens are compared in constant time.
- Every other request needs that cookie. Without it, or with a wrong token, the answer is `401`, with a line that says to run `cinderline web`.
- The URL with the token is a password: it can stay in the terminal's scrollback and in the browser's history. Anyone who has it and can reach the port gets in. Replace the token with `--new-token` if it leaked.

### Host and origin checks

- **Host.** The `Host` header must be `127.0.0.1:<port>` or `localhost:<port>`, else the answer is `403`. This stops DNS rebinding: a web page on a domain the attacker points at `127.0.0.1` sends its own domain as the Host.
- **Origin.** Every request that is not `GET` or `HEAD` must come from the page itself: an `Origin` header of `http://<that host>`, or `Sec-Fetch-Site: same-origin`, else `403`. Together with the `SameSite=Strict` cookie, this keeps another site open in the same browser from changing anything.

### Secrets are write-only

The page can set and delete a project's runner token, git token and model keys, and a provider's key. It shows whether each one is set, never its value. No request returns a value, and no error message holds one. A value you type goes once, over loopback, to the runner, which stores it in the keychain or the fallback file, the same way `cinderline key set` does.

### What the transcripts hold

To show runs after they ended, the runner keeps a transcript of each run:

- **What is in it.** Each step's outcome. For an agent turn: the prompt Cinderline sent, which includes the issue's text and comments, and every line the agent's CLI printed. Those lines hold the agent's text, its tool calls with their input and results (and so the contents of files and the output of commands the agent ran), and its token counts and cost. The check command's output. The hosts the proxy refused, with the method and URL of the first request where the proxy read it.
- **Redacted.** Every line passes the same redaction as the log before it is stored or sent to the page: every secret the runner holds is replaced by a marker, also in base64, hex and URL-encoded form. Nothing on the page or on disk shows a line unredacted. Redaction is a second line of defence only. It skips values shorter than 16 characters, and it cannot know a secret the runner does not hold, such as one an agent read from the repository.
- **Where it is.** `transcripts/<project>/<issue>/<run>/` in the state folder, one file per step, one JSON object per line. The folders are `0700` and the files `0600`, so only your user can read them. They never go to Cinderline.
- **How long it stays.** As long as `transcript_days` says (14 by default), and only while all transcripts together stay under `transcript_max_size` (1 GB by default). Past that limit, the oldest runs are removed first. With `transcript_days` 0, a run is removed as soon as it ends. `cinderline uninstall` removes the state folder with all transcripts.
- **Live CPU and memory** of a run's containers are read only while a page shows that run, and are kept in memory only.
