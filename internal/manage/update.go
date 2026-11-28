package manage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
	"github.com/rodo-software/cinderline-runner/scripts"
)

// Exe is the running binary's path, the one Update replaces.
func Exe() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return "", fmt.Errorf("resolve exe: %w", err)
	}
	return exe, nil
}

// Update runs the install script over exe, the binary of version current,
// and has the new binary refresh the tools volume and the default image,
// which only it has the pins for. It returns the new version, current when
// the latest release is the one running. Restarting the runner is the
// caller's: `update` restarts the service, the page re-execs the daemon
// (decision 2026-11-28-update-restart).
func Update(ctx context.Context, x workspace.Exec, exe, current string) (string, error) {
	if err := install(ctx, x, filepath.Dir(exe)); err != nil {
		return "", err
	}
	out, err := x(ctx, nil, exe, "version")
	if err != nil {
		return "", fmt.Errorf("version: %w", err)
	}
	parts := strings.Fields(out)
	if len(parts) != 2 || parts[0] != name.Name {
		return "", fmt.Errorf("version: the new binary answered %q", out)
	}
	if parts[1] == current {
		return current, nil
	}
	if _, err := x(ctx, nil, exe, "update", "--refresh"); err != nil {
		return "", fmt.Errorf("update --refresh: %w", err)
	}
	return parts[1], nil
}

// install runs this OS's install script, which downloads the latest
// release, checks it against checksums.txt and replaces the binary in dir.
func install(ctx context.Context, x workspace.Exec, dir string) error {
	script, argv := scripts.InstallSh, []string{"sh"}
	env := []string{strings.ToUpper(name.Name) + "_INSTALL_DIR=" + dir}
	if runtime.GOOS == "windows" {
		script, argv = scripts.InstallPs1, []string{"powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File"}
		env = append(env, "PSModulePath=") // inherited from PowerShell 7, it hides Get-FileHash from Windows PowerShell
	}
	// A file: PowerShell runs only one named .ps1, and trips over a piped script's blocks.
	f, err := os.CreateTemp("", name.Name+"-install-*.ps1")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(script)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	_, err = x(ctx, env, append(argv, f.Name())...)
	return err
}
