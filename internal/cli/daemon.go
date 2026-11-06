package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rodo-software/cinderstack-runner/internal/daemon"
	"github.com/rodo-software/cinderstack-runner/internal/localapi"
	"github.com/rodo-software/cinderstack-runner/internal/name"
)

// run runs the daemon in the foreground. daemon.Run stops on SIGINT and SIGTERM
// itself and gives localapi.ErrHeld while the service holds the socket.
func (c *CLI) run(args []string) int {
	if !c.noArgs("run", args) {
		return 2
	}
	if c.pathsErr != nil {
		return c.fail(c.pathsErr)
	}
	if err := daemon.Run(context.Background(), daemon.Options{Paths: c.Paths, Version: c.Version}); err != nil {
		return c.fail(err)
	}
	return 0
}

// status prints the daemon's version and one line per project.
func (c *CLI) status(args []string) int {
	if !c.noArgs("status", args) {
		return 2
	}
	if c.pathsErr != nil {
		return c.fail(c.pathsErr)
	}
	s, err := localapi.Client{Socket: c.Paths.Socket}.Status(context.Background())
	if err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.Stdout, "%s %s is running\n", name.Name, s.Version)
	if len(s.Projects) == 0 {
		fmt.Fprintln(c.Stdout, "no projects; add one with "+name.Name+" add")
	}
	for _, p := range s.Projects {
		parts := []string{"not connected"}
		if p.Connected {
			parts[0] = "connected"
		}
		if p.LastPoll.IsZero() {
			parts = append(parts, "never polled")
		} else {
			parts = append(parts, "last poll "+p.LastPoll.Local().Format(time.DateTime))
		}
		if p.Issue != "" {
			parts = append(parts, "working on "+p.Issue)
		}
		if p.LastError != "" {
			parts = append(parts, "last error "+p.LastErrorAt.Local().Format(time.DateTime)+": "+p.LastError)
		}
		fmt.Fprintf(c.Stdout, "%s: %s\n", p.Project, strings.Join(parts, ", "))
	}
	return 0
}

// logs prints the log's tail and then follows it until Ctrl-C or the daemon stops.
func (c *CLI) logs(args []string) int {
	f := c.flags("logs", "[-n LINES]")
	n := f.Int("n", 100, "number of lines to print before following")
	if err := f.Parse(args); err != nil || f.NArg() > 0 {
		if err == nil {
			f.Usage()
		}
		return 2
	}
	if c.pathsErr != nil {
		return c.fail(c.pathsErr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := (localapi.Client{Socket: c.Paths.Socket}).Logs(ctx, *n, c.Stdout); err != nil {
		return c.fail(err)
	}
	return 0
}

// stopDaemon stops the daemon at once (decision 2026-11-05-runner-rewrite-details point 14).
func (c *CLI) stopDaemon(args []string) int {
	if !c.noArgs("stop", args) {
		return 2
	}
	if c.pathsErr != nil {
		return c.fail(c.pathsErr)
	}
	if err := (localapi.Client{Socket: c.Paths.Socket}).Stop(context.Background()); err != nil {
		return c.fail(err)
	}
	fmt.Fprintln(c.Stdout, "Stopped "+name.Name)
	return 0
}

// noArgs reports whether args is empty, printing the command's usage when it is not.
func (c *CLI) noArgs(cmd string, args []string) bool {
	f := c.flags(cmd, "")
	if err := f.Parse(args); err != nil || f.NArg() > 0 {
		if err == nil {
			f.Usage()
		}
		return false
	}
	return true
}
