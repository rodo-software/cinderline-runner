package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/daemon"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"golang.org/x/term"
)

var drainPoll = time.Second // how often stop --drain asks the daemon what it still runs; tests shorten it

// run runs the daemon in the foreground. daemon.Run stops on SIGINT and SIGTERM
// itself and gives localapi.ErrHeld while the service holds the socket.
func (c *CLI) run(args []string) int {
	f := c.flags("run", "[-v]")
	verbose := f.Bool("v", false, "also log to stderr, and log Debug lines")
	if !c.parsed(f, args, 0) {
		return 2
	}
	if c.pathsErr != nil {
		return c.fail(c.pathsErr)
	}
	if err := c.checkDocker(context.Background(), true); err != nil {
		return c.fail(err)
	}
	o := daemon.Options{Paths: c.Paths, Version: c.Version}
	if *verbose {
		o.Verbose = c.Stderr
	}
	if err := daemon.Run(context.Background(), o); err != nil {
		return c.fail(err)
	}
	return 0
}

// status prints the daemon's version and one line per project.
func (c *CLI) status(args []string) int {
	if !c.parsed(c.flags("status", ""), args, 0) {
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
	if !c.parsed(f, args, 0) {
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
	if !c.parsed(f, args, 0) {
		return 2
	}
	if c.pathsErr != nil {
		return c.fail(c.pathsErr)
	}
	if *drainFlag {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := c.drainAndWait(ctx, false); err != nil {
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
// still in flight to Stdout when they change. It waits for the process
// separately because the socket closes before the process does. With
// keyed, s on Stdin, confirmed with y, stops the daemon at once meanwhile,
// cutting its runs (decision 2026-11-18-drain-before-reload).
func (c *CLI) drainAndWait(ctx context.Context, keyed bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // ends the key reader
	cl := localapi.Client{Socket: c.Paths.Socket}
	st, err := cl.Status(ctx)
	if err != nil {
		return err
	}
	pid := st.PID
	if err := cl.Drain(ctx); err != nil {
		return err
	}
	out := c.Stdout
	var keys <-chan byte
	if keyed {
		var restore func()
		keys, out, restore = c.readKeys(ctx)
		defer restore()
	}
	asking := false
	defer func() {
		if asking {
			fmt.Fprintln(out) // the question went unanswered
		}
	}()
	// pause waits until the next poll, answering keys meanwhile.
	pause := func() error {
		next := time.After(drainPoll)
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-next:
				return nil
			case k, ok := <-keys:
				switch {
				case !ok:
					keys = nil
				case k == 3: // Ctrl-C on a raw terminal, which sends no signal
					return errInterrupted
				case asking:
					asking = false
					if k != 'y' && k != 'Y' {
						fmt.Fprintln(out, "no; still waiting")
						continue
					}
					fmt.Fprintln(out, "yes; stopping now")
					if err := cl.Stop(ctx); err != nil && !errors.Is(err, localapi.ErrNotRunning) {
						fmt.Fprintln(out, "The stop failed, still waiting:", err)
					}
				case k == 's' || k == 'S':
					asking = true
					fmt.Fprint(out, "Stop now, cutting the runs in flight? [y/N] ")
				}
			}
		}
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
			for pid != 0 && !gone(pid) {
				if err := pause(); err != nil {
					return err
				}
			}
			return nil
		}
		switch {
		case err != nil && failed:
			return err
		case err != nil:
			failed = true // the daemon may exit while it answers (EOF); the next poll tells
		case asking: // a line now would break the question's
			failed = false
		default:
			failed = false
			issues := inFlight(st)
			if len(issues) > 0 && strings.Join(issues, ",") != strings.Join(last, ",") {
				line := "Waiting for " + strings.Join(issues, ", ") + " to finish"
				if keys != nil {
					line += "; press s to stop now instead, which cuts them"
				}
				fmt.Fprintln(out, line)
			}
			last = issues
		}
		if err := pause(); err != nil {
			return err
		}
	}
}

var errInterrupted = errors.New("interrupted")

// inFlight is the issues the daemon runs now.
func inFlight(st localapi.Status) []string {
	var issues []string
	for _, p := range st.Projects {
		if p.Issue != "" {
			issues = append(issues, p.Issue)
		}
	}
	return issues
}

// readKeys reads Stdin one byte at a time until ctx is done, with a terminal
// in raw mode so that a key counts without Enter. out is Stdout with the line
// ends a raw terminal needs; restore puts the terminal back.
func (c *CLI) readKeys(ctx context.Context) (keys <-chan byte, out io.Writer, restore func()) {
	out, restore = c.Stdout, func() {}
	if c.Stdin == nil {
		return nil, out, restore
	}
	if f, ok := c.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		if old, err := term.MakeRaw(int(f.Fd())); err == nil {
			out, restore = crlf{c.Stdout}, func() { term.Restore(int(f.Fd()), old) }
		}
	}
	ch := make(chan byte)
	go func() {
		defer close(ch)
		b := make([]byte, 1)
		for {
			// On a terminal this Read outlives the wait; the command ends soon after.
			n, err := c.Stdin.Read(b)
			if n == 1 {
				select {
				case ch <- b[0]:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return ch, out, restore
}

// crlf writes "\r\n" for every "\n", as a raw terminal needs.
type crlf struct{ w io.Writer }

func (c crlf) Write(p []byte) (int, error) {
	if _, err := c.w.Write(bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))); err != nil {
		return 0, err
	}
	return len(p), nil
}
