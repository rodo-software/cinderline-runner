# Decision 2026-11-28-skill-downloads

- **Status:** Accepted
- **Date:** 2026-11-28
- **Source:** Willem on CLR-318: option A of question 1 (downloads live in the Cinderline catalog) and option A of question 2 (one entry per file and arch, no placeholders; the app side filed as a CL issue). The defaults stated with question 2 were not vetoed.

## Decision

1. **A skill package's downloads are declared in the catalog,** by an admin, as for the package's URL and ref. They are never guessed and never read from the package's repository.
2. **The contract: `downloads` on the job's `SkillPackage`,** optional, at most 10 entries of `{arch, path, url, sha256, executable}`:
   - `arch`: `amd64` or `arm64`, as the Docker engine names it. The runner fetches only the entries for its engine's arch. There are no `{os}` or `{libc}` placeholders: the OS is always linux, and one package volume is shared by projects whose images may differ in libc.
   - `path`: relative to the package's root, with the rules of the package's `path` (at most 200, clean, relative, without `..`, of letters, digits and `. _ @ + - /`). The runner refuses one reached through a symlink, or one that is a symlink or a folder.
   - `url`: with the rules of the package's `url` (https, without user info, query or fragment, at most 500).
   - `sha256`: 64 lower-case hex, required. A download that doesn't match is refused.
   - `executable`: mode 755, else 644.
3. **The runner fetches each download once, when it fills the volume,** over its own network, not through a run's network filter or a project's allowed hosts. Each file is fetched and checked before the volume is touched, then written into the package's root after the git fetch, and the `complete` marker comes last. Each file is capped at 200 MB and each download at 10 minutes.
4. **The volume's name covers the downloads.** With downloads, the package's root and its downloads are in the volume's hash, so changing them gives a new volume. Without them the name is as before.
5. **A failed download is a failed fill.** The package is `failed` in `workspace.skills` and the run goes on without it, as for any failed fill. Whether a run should stop for it is the app's call.
6. **A strict runner refuses a package with downloads** (`refused_strict`), even one `allow-skill` allows: the allowance names the package's URL, not the URLs of its downloads, and strict runners fetch nothing that Cinderline alone names (2026-11-17-blocked-hosts). This applies the existing rule; Willem has not decided it on its own (see CLR-318's report).
7. **Downloads only, no setup command.** Running a package's own install step at fill time would run third-party code with network access and without hashes.
8. **Order.** The runner reads `downloads` first and ignores none it lacks. The app adds the field to its catalog and contract (the CL issue); `docs/runner-api/openapi.yaml` is then copied here.

## Reason

Impeccable's launcher runs a compiled engine from a GitHub release, which it downloads on first use. A run's network filter blocks that, and allowing the host per project would download it again on every run. In the catalog an admin vouches for what the runner fetches, and impeccable works unchanged, since its launcher looks for the engine at `scripts/bin/linux-<x64|arm64>/impeccable` first. One flat entry per file and arch needs no templating in the app, the runner or the form.
