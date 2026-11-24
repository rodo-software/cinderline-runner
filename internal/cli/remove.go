package cli

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/run"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
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
	cfg, err := c.loadConfig()
	if err != nil {
		return c.fail(err)
	}
	p, err := findProject(cfg, f.Arg(0), *instance, *ws)
	if err != nil {
		return c.fail(err)
	}
	// The config first, so a failed save keeps the project's tokens (CLR-233).
	cfg.Projects = without(cfg.Projects, p)
	if err := cfg.Save(c.Paths.File); err != nil {
		return c.fail(err)
	}
	for _, k := range secrets.Kinds(p) {
		if err := c.Secrets.Delete(p, k); err != nil {
			fmt.Fprintln(c.Stderr, "warning:", err)
		}
	}
	c.dropLocal(cfg, p)
	fmt.Fprintf(c.Stdout, "Removed %s at %s\n", p.Key, p.URL)
	return 0
}

// dropLocal makes a running runner reload, waits until p's loop has
// finished its runs and ended, then removes what is named by p's ID and is
// not in the config or the keychain: its Docker containers, networks and
// volumes, its session notes and its outbox. cfg is the saved
// config, without p or with p under another ID. It only warns.
func (c *CLI) dropLocal(cfg *config.Config, p config.Project) {
	c.reload()
	// The reload drains the project's loop; its runs hold its volumes.
	if !c.waitProject(p) {
		return
	}
	// Docker names carry only the ID, so a namesake on another instance still uses them.
	shared := slices.ContainsFunc(cfg.Projects, func(q config.Project) bool { return q.ID() == p.ID() })
	if !shared {
		label := "label=" + workspace.ProjectLabel(p.ID())
		if err := c.removeLabelled(label); err != nil {
			fmt.Fprintf(c.Stderr, "warning: %s's Docker volumes, with its clones and sign-ins, are not all removed; list them with `docker volume ls --filter %s`: %v\n", p.Key, label, err)
		}
	}
	if err := os.RemoveAll(run.PausedDir(c.Paths.State, p)); err != nil {
		fmt.Fprintln(c.Stderr, "warning:", err)
	}
	if err := os.Remove(run.OutboxFile(c.Paths.State, p)); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(c.Stderr, "warning:", err)
	}
}

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

// findProject returns the one project with key, at instance and in ws when
// those are not empty; a key in several places needs them.
func findProject(cfg *config.Config, key, instance, ws string) (config.Project, error) {
	want := strings.TrimRight(instance, "/")
	var matches []config.Project
	for _, p := range cfg.Projects {
		if p.Key == key && (want == "" || p.URL == want) && (ws == "" || p.Workspace == ws) {
			matches = append(matches, p)
		}
	}
	switch {
	case len(matches) == 0 && (want != "" || ws != ""):
		return config.Project{}, fmt.Errorf("no project %s at %s in workspace %s", key, cmp.Or(want, "any URL"), cmp.Or(ws, "any"))
	case len(matches) == 0:
		return config.Project{}, fmt.Errorf("no project %s", key)
	case len(matches) > 1:
		var places []string
		for _, p := range matches {
			places = append(places, where(p))
		}
		return config.Project{}, fmt.Errorf("several projects have key %s; choose one with -url or -workspace: %s", key, strings.Join(places, ", "))
	}
	return matches[0], nil
}
