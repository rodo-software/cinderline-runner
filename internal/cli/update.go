package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/manage"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

// update installs the latest release and refreshes the tools (manage.Update),
// and restarts a running runner once its runs have finished.
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
		if exe, err = manage.Exe(); err != nil {
			return c.fail(err)
		}
	}
	newVersion, err := manage.Update(ctx, c.exec, exe, c.Version)
	if err != nil {
		return c.fail(err)
	}
	if newVersion == c.Version {
		fmt.Fprintf(c.Stdout, "Already at %s\n", c.Version)
		return 0
	}
	fmt.Fprintf(c.Stdout, "Updated %s from %s to %s\n", name.Name, c.Version, newVersion)

	err = c.drainAndWait(ctx, true)
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
