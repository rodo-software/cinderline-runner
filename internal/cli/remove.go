package cli

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/run"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// remove deletes a project's secrets and its config entry, makes a running
// runner drop it, and removes its Docker containers, networks and volumes
// (the clones and the clients' sign-ins) and its paused notes (decision
// 2026-11-11-audit-fixes). What comes after the config is saved only warns:
// the project is removed by then.
func (c *CLI) remove(args []string) int {
	f := c.flags("remove", "[-url URL] [-workspace SLUG] KEY")
	instance, ws := projectFlags(f)
	if err := f.Parse(args); err != nil || f.NArg() != 1 {
		if err == nil {
			f.Usage()
		}
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
	for _, k := range secrets.Kinds(p) {
		if err := c.Secrets.Delete(p, k); err != nil {
			return c.fail(err)
		}
	}
	cfg.Projects = without(cfg.Projects, p)
	if err := cfg.Save(c.Paths.File); err != nil {
		return c.fail(err)
	}
	// Before Docker: the reload ends the project's runs, which hold its volumes.
	if err := (localapi.Client{Socket: c.Paths.Socket}).Reload(context.Background()); err != nil && !errors.Is(err, localapi.ErrNotRunning) {
		fmt.Fprintf(c.Stderr, "warning: the running %s did not reload, restart it: %v\n", name.Name, err)
	}
	// Docker names carry only the ID, so a namesake on another instance still uses them.
	shared := slices.ContainsFunc(cfg.Projects, func(q config.Project) bool { return q.ID() == p.ID() })
	if !shared {
		label := "label=" + workspace.ProjectLabel(p.ID())
		for _, step := range [][2][]string{
			{{"ps", "--all", "--quiet", "--filter", label}, {"rm", "--force", "--volumes"}},
			{{"network", "ls", "--quiet", "--filter", label}, {"network", "rm"}},
			{{"volume", "ls", "--quiet", "--filter", label}, {"volume", "rm", "--force"}},
		} {
			if err := c.dockerRemove(step[0], step[1]); err != nil {
				fmt.Fprintf(c.Stderr, "warning: %s's Docker volumes, with its clones and sign-ins, are not all removed; list them with `docker volume ls --filter %s`: %v\n", p.Key, label, err)
				break
			}
		}
	}
	if err := os.RemoveAll(run.PausedDir(c.Paths.State, p)); err != nil {
		fmt.Fprintln(c.Stderr, "warning:", err)
	}
	if err := os.Remove(run.OutboxFile(c.Paths.State, p)); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(c.Stderr, "warning:", err)
	}
	fmt.Fprintf(c.Stdout, "Removed %s at %s\n", p.Key, p.URL)
	return 0
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
