# The install scripts put the install folder on the PATH themselves

- **Status:** Accepted
- **Date:** 2026-11-11
- **Source:** Decided by Willem on CSR-158.

## Decision

- When the install folder is not on the PATH, `scripts/install.sh` appends `export PATH="$HOME/.local/bin:$PATH"` to the profile of the user's shell, picked from `$SHELL`: `~/.zshrc` for zsh, `~/.bashrc` for bash (`~/.bash_profile` on macOS), `~/.profile` for any other shell. It adds the line only when the file does not already hold it, and says which file it changed and that a new terminal is needed. For fish and other shells with a different syntax (csh, tcsh, nu, elvish, xonsh, pwsh) it edits nothing and prints the line as before.
- `scripts/install.ps1` adds the install folder to the **user** PATH (`[Environment]::SetEnvironmentVariable(..., 'User')`), never the machine PATH, and says a new terminal is needed.
- `CINDERLINE_NO_MODIFY_PATH=1` opts out in both scripts: nothing is edited and the old instruction is printed. A profile that cannot be written gets the same instruction; the install still exits 0.
- The install location stays as decision 2026-11-05-runner-rewrite-details point 17 says (`~/.local/bin`, `%LocalAppData%\Programs\cinderline`, no sudo). Homebrew stays later (plan, D2).
- `cinderline uninstall` does not remove the line; like the binary, it tells the user to remove it.

## Reason

After the one-line install, `cinderline` was often not found: `~/.local/bin` is not on the default PATH on macOS, and on Linux not until the next login, and users missed the printed line. No folder on macOS is both on the default PATH and writable without sudo, and `cinderline update` must keep working without sudo, so the scripts change the PATH rather than the location.
