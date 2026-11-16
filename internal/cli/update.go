package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
	"github.com/rodo-software/cinderline-runner/scripts"
)

// update runs the install script over this binary, has the new binary
// refresh the tools volume and default image, which only it has the pins
// for, and restarts a running runner once its runs have finished.
func (c *CLI) update(args []string) int {
	f := c.flags("update", "[--refresh]")
	refresh := f.Bool("refresh", false, "only refresh the tools volume and default image; update runs the new binary with it")
	if !c.parsed(f, args, 0) {
		return 2
	}
	if c.pathsErr != nil {
		return c.fail(c.pathsErr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *refresh {
		if _, _, err := c.agentImage(ctx, ""); err != nil {
			return c.fail(err)
		}
		fmt.Fprintln(c.Stdout, "Refreshed the tools volume and the default image")
		return 0
	}

	exe := c.exe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err == nil {
			exe, err = filepath.EvalSymlinks(exe)
		}
		if err != nil {
			return c.fail(fmt.Errorf("resolve exe: %w", err))
		}
	}
	x := c.exec
	if err := install(ctx, x, filepath.Dir(exe)); err != nil {
		return c.fail(err)
	}
	out, err := x(ctx, nil, exe, "version")
	if err != nil {
		return c.fail(fmt.Errorf("version: %w", err))
	}
	parts := strings.Fields(out)
	if len(parts) != 2 || parts[0] != name.Name {
		return c.fail(fmt.Errorf("version: the new binary answered %q", out))
	}
	newVersion := parts[1]
	if newVersion == c.Version {
		fmt.Fprintf(c.Stdout, "Already at %s\n", c.Version)
		return 0
	}
	fmt.Fprintf(c.Stdout, "Updated %s from %s to %s\n", name.Name, c.Version, newVersion)
	if _, err := x(ctx, nil, exe, "update", "--refresh"); err != nil {
		return c.fail(fmt.Errorf("update --refresh: %w", err))
	}

	err = c.drainAndWait(ctx)
	if errors.Is(err, localapi.ErrNotRunning) {
		return 0 // a runner someone stopped stays stopped
	}
	if err != nil {
		return c.fail(fmt.Errorf("the drain failed: %w", err))
	}
	start := c.startService
	if start == nil {
		start = startService
	}
	installed, err := start(exe)
	if err != nil {
		return c.fail(fmt.Errorf("starting the service failed: %w", err))
	}
	if installed {
		fmt.Fprintf(c.Stdout, "Started the %s service with %s\n", name.Name, newVersion)
	} else {
		fmt.Fprintf(c.Stdout, "The running %s finished its runs and stopped; start it again with `%s run`\n", name.Name, name.Name)
	}
	return 0
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
