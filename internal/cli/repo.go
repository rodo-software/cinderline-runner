package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/manage"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// projectUsage is the flags repoFlags parses, for a command's usage.
const projectUsage = "--project KEY [-url URL] [-workspace SLUG]"

// repoFlags parses --project, -url and -workspace and returns the config and the index
// of the project in it; code is non-zero when the command must stop.
func (c *CLI) repoFlags(cmd, usage string, args []string, nargs int) (cfg *config.Config, i int, rest []string, code int) {
	f := c.flags(cmd, usage)
	project := f.String("project", "", "project key")
	instance, ws := projectFlags(f)
	if !c.parsed(f, args, nargs) {
		return nil, 0, nil, 2
	}
	if *project == "" {
		f.Usage()
		return nil, 0, nil, 2
	}
	cfg, err := c.loadConfig()
	if err != nil {
		return nil, 0, nil, c.fail(err)
	}
	p, err := findProject(cfg, *project, *instance, *ws)
	if err != nil {
		return nil, 0, nil, c.fail(err)
	}
	i = slices.IndexFunc(cfg.Projects, func(q config.Project) bool { return q.URL == p.URL && q.Workspace == p.Workspace && q.Key == p.Key })
	return cfg, i, f.Args(), 0
}

// addRepo adds an extra repository that the project's runs can read
// (CSR-160). It asks for the URL, an optional ref and a git token, and checks
// with git ls-remote that the token can read the ref before storing anything.
func (c *CLI) addRepo(args []string) int {
	cfg, i, _, code := c.repoFlags("add-repo", projectUsage, args, 0)
	if code != 0 {
		return code
	}
	p := cfg.Projects[i]
	if err := c.checkDocker(context.Background(), false); err != nil {
		return c.fail(err)
	}
	ask := c.lineAsk()
	say := func(format string, a ...any) { fmt.Fprintf(c.Stdout, format+"\n", a...) }

	var r config.ExtraRepository
	var store secrets.Store
	for {
		var token string
		r = config.ExtraRepository{}
		for r.URL == "" {
			v, err := ask("Repository URL: ")
			if err != nil {
				return c.fail(err)
			}
			if err := manage.CheckRepo(p, config.ExtraRepository{URL: v}); err != nil {
				say("%v.", err)
				continue
			}
			r.URL = v
		}
		for {
			v, err := ask("Branch or tag [empty for the default branch]: ")
			if err != nil {
				return c.fail(err)
			}
			if err := manage.CheckRepo(p, config.ExtraRepository{URL: r.URL, Ref: v}); err != nil {
				say("%v.", err)
				continue
			}
			r.Ref = v
			break
		}
		for token == "" {
			v, err := ask("Git token: ")
			if err != nil {
				return c.fail(err)
			}
			token = v
		}
		var err error
		store, err = manage.AddRepo(context.Background(), slog.New(slog.NewTextHandler(c.Stderr, nil)), c.Paths, c.Secrets, c.exec, p.ID(), r, token)
		if errors.As(err, new(manage.Refused)) { // only the git check is left to refuse: ask everything again
			msg := err.Error()
			say("%s%s", strings.ToUpper(msg[:1]), msg[1:])
			continue
		}
		if err != nil {
			return c.fail(err)
		}
		break
	}
	say("Stored the git token for %s in %s", r.URL, c.storeName(store))
	say("Added %s at %s; runs read it at %s/%s", r.URL, manage.Ref(r.Ref), workspace.ExtraPath, r.Name())
	c.reload()
	return 0
}

// listRepos prints the project's extra repositories, one per line.
func (c *CLI) listRepos(args []string) int {
	cfg, i, _, code := c.repoFlags("list-repos", projectUsage, args, 0)
	if code != 0 {
		return code
	}
	for _, r := range cfg.Projects[i].ExtraRepositories {
		fmt.Fprintf(c.Stdout, "%s\t%s\t%s\n", r.Name(), r.URL, manage.Ref(r.Ref))
	}
	return 0
}

// removeRepo removes an extra repository, named by its URL or name, and its
// token. The next run deletes its copy.
func (c *CLI) removeRepo(args []string) int {
	cfg, i, rest, code := c.repoFlags("remove-repo", projectUsage+" URL|NAME", args, 1)
	if code != 0 {
		return code
	}
	r, err := manage.RemoveRepo(c.Paths, c.Secrets, cfg.Projects[i].ID(), rest[0])
	if err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.Stdout, "Removed %s\n", r.URL)
	c.reload()
	return 0
}

// reload makes a running daemon read the config again; it only warns. Each
// project takes the change once its runs have finished, so the command
// says which still run on the old config (decision 2026-11-24-rolling-reload).
func (c *CLI) reload() {
	cl := localapi.Client{Socket: c.Paths.Socket}
	if err := cl.Reload(context.Background()); err != nil {
		if !errors.Is(err, localapi.ErrNotRunning) {
			fmt.Fprintf(c.Stdout, "Warning: daemon reload failed: %v.\n", err)
		}
		return
	}
	if st, err := cl.Status(context.Background()); err == nil {
		if issues := inFlight(draining(st, "")); len(issues) > 0 {
			fmt.Fprintf(c.Stdout, "%s still run on the old config; each project takes the change once its runs have finished.\n", strings.Join(issues, ", "))
		}
	}
}

// startAgain starts the service, after a stop the person chose ended the
// running daemon, and says so.
func (c *CLI) startAgain() {
	exe, start := c.exe, c.startService
	if start == nil {
		start = startService
	}
	if exe == "" {
		exe, _ = os.Executable() // the service only needs it to install, not to start
	}
	switch installed, err := start(exe); {
	case err != nil:
		fmt.Fprintf(c.Stdout, "Warning: starting the %s service failed: %v.\n", name.Name, err)
	case installed:
		fmt.Fprintf(c.Stdout, "Started the %s service again with the change\n", name.Name)
	default:
		fmt.Fprintf(c.Stdout, "The running %s stopped; start it again with `%s run`\n", name.Name, name.Name)
	}
}
