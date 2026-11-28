package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/manage"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// remove deletes a project's config entry and its secrets, makes a running
// runner drop it, and removes its Docker containers, networks and volumes
// (the clones and the clients' sign-ins) and its paused notes (decision
// 2026-11-11-audit-fixes). What comes after the config is saved only warns:
// the project is removed by then.
func (c *CLI) remove(args []string) int {
	f := c.flags("remove", "[-url URL] [-workspace SLUG] KEY")
	instance, ws := projectFlags(f)
	if !c.parsed(f, args, 1) {
		return 2
	}
	if _, err := c.loadConfig(); err != nil {
		return c.fail(err)
	}
	p, err := manage.RemoveProject(c.Paths, c.Secrets, manage.Project{Key: f.Arg(0), URL: *instance, Workspace: *ws}, c.warn)
	if err != nil {
		return c.fail(err)
	}
	c.dropLocal(p)
	fmt.Fprintf(c.Stdout, "Removed %s at %s\n", p.Key, p.URL)
	return 0
}

// dropLocal makes a running runner reload, waits until p's loop has
// finished its runs and ended, then removes what is named by p's ID and is
// not in the config or the keychain (manage.DropLocal). It only warns.
func (c *CLI) dropLocal(p config.Project) {
	c.reload()
	// The reload drains the project's loop; its runs hold its volumes.
	if c.waitProject(p) {
		manage.DropLocal(context.Background(), c.Paths, c.exec, p, c.warn)
	}
}

// warn prints err as a warning: what fails once the change is saved.
func (c *CLI) warn(err error) { fmt.Fprintln(c.Stderr, "warning:", err) }

// waitProject waits until a running daemon's loop of p has ended, with s
// then y to stop the daemon at once, which starts the service again after.
// It says false, with a warning, when it stopped waiting before.
func (c *CLI) waitProject(p config.Project) bool {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := c.waitRuns(ctx, true, p.Label())
	switch {
	case errors.Is(err, localapi.ErrNotRunning): // no daemon, no loop
		return true
	case err != nil:
		fmt.Fprintf(c.Stdout, "Warning: stopped waiting (%v): %s still finishes its runs, and its Docker volumes, session notes and outbox are kept; remove its volumes once they have ended, listed with `docker volume ls --filter label=%s`.\n",
			err, p.Key, workspace.ProjectLabel(p.ID()))
		return false
	}
	if _, err := (localapi.Client{Socket: c.Paths.Socket}).Status(ctx); errors.Is(err, localapi.ErrNotRunning) {
		c.startAgain() // s and y stopped it
	}
	return true
}

// projectFlags adds -url and -workspace, which choose between projects that share a key.
func projectFlags(f *flag.FlagSet) (instance, workspace *string) {
	return f.String("url", "", "URL of the instance, when several projects share the key"),
		f.String("workspace", "", "workspace slug, when several projects at one URL share the key")
}
