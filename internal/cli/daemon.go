package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/daemon"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

var drainPoll = time.Second // how often stop --drain asks the daemon what it still runs; tests shorten it

// run runs the daemon in the foreground. daemon.Run stops on SIGINT and SIGTERM
// itself and gives localapi.ErrHeld while the service holds the socket.
func (c *CLI) run(args []string) int {
	if !c.noArgs("run", args) {
		return 2
	}
	if c.pathsErr != nil {
		return c.fail(c.pathsErr)
	}
	if err := c.checkDocker(context.Background(), true); err != nil {
		return c.fail(err)
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

// stopDaemon stops the daemon at once, or with --drain once its runs have finished.
func (c *CLI) stopDaemon(args []string) int {
	f := c.flags("stop", "[--drain]")
	drainFlag := f.Bool("drain", false, "let current runs finish before stopping")
	if err := f.Parse(args); err != nil || f.NArg() > 0 {
		if err == nil {
			f.Usage()
		}
		return 2
	}
	if c.pathsErr != nil {
		return c.fail(c.pathsErr)
	}
	if *drainFlag {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := c.drainAndWait(ctx); err != nil {
			return c.fail(err)
		}
		fmt.Fprintln(c.Stdout, "Stopped "+name.Name)
		return 0
	}
	if err := (localapi.Client{Socket: c.Paths.Socket}).Stop(context.Background()); err != nil {
		return c.fail(err)
	}
	fmt.Fprintln(c.Stdout, "Stopped "+name.Name)
	return 0
}

// drainAndWait calls Drain on the local API and then polls Status until the
// daemon's socket is gone and the process has exited. It prints the issues
// still in flight to Stdout when they change. A later job (update) reuses
// this method. It waits for the process separately because the socket closes
// before the process does.
func (c *CLI) drainAndWait(ctx context.Context) error {
	cl := localapi.Client{Socket: c.Paths.Socket}
	st, err := cl.Status(ctx)
	if err != nil {
		return err
	}
	pid := st.PID
	if err := cl.Drain(ctx); err != nil {
		return err
	}
	var last []string
	failed := false
	for {
		st, err := cl.Status(ctx)
		if errors.Is(err, localapi.ErrNotRunning) {
			gone := c.processGone
			if gone == nil {
				gone = processGone
			}
			if pid != 0 {
				for {
					if gone(pid) {
						return nil
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(drainPoll):
					}
				}
			}
			return nil
		}
		switch {
		case err != nil && failed:
			return err
		case err != nil:
			failed = true // the daemon may exit while it answers (EOF); the next poll tells
		default:
			failed = false
			var issues []string
			for _, p := range st.Projects {
				if p.Issue != "" {
					issues = append(issues, p.Issue)
				}
			}
			if len(issues) > 0 && strings.Join(issues, ",") != strings.Join(last, ",") {
				fmt.Fprintln(c.Stdout, "Waiting for "+strings.Join(issues, ", ")+" to finish")
			}
			last = issues
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(drainPoll):
		}
	}
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
