package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
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
	var token string
	for {
		r, token = config.ExtraRepository{}, ""
		for r.URL == "" {
			v, err := ask("Repository URL: ")
			if err != nil {
				return c.fail(err)
			}
			if err := (config.ExtraRepository{URL: v}).Validate(); err != nil {
				say("%v.", err)
				continue
			}
			if config.SameRepository(v, p.Repository) {
				say("%s is the project's own repository.", v)
				continue
			}
			if j := slices.IndexFunc(p.ExtraRepositories, func(q config.ExtraRepository) bool { return q.Name() == (config.ExtraRepository{URL: v}).Name() }); j >= 0 {
				say("%s is already added as %s; remove it first with remove-repo.", p.ExtraRepositories[j].URL, p.ExtraRepositories[j].Name())
				continue
			}
			r.URL = v
		}
		for {
			v, err := ask("Branch or tag [empty for the default branch]: ")
			if err != nil {
				return c.fail(err)
			}
			if err := (config.ExtraRepository{URL: r.URL, Ref: v}).Validate(); err != nil {
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
		if err := workspace.LsRemote(context.Background(), c.exec, p.ID(), r.URL, r.Ref, token); err != nil {
			say("This token cannot read %s at %s: %v", r.URL, cmpRef(r.Ref), err)
			continue
		}
		break
	}

	// The config first, so a failed save stores no secret.
	cfg.Projects[i].ExtraRepositories = append(slices.Clone(p.ExtraRepositories), r)
	if err := cfg.Save(c.Paths.File); err != nil {
		return c.fail(err)
	}
	store, err := c.Secrets.Set(p, secrets.ExtraGitToken(r.URL), token)
	if err != nil {
		cfg.Projects[i] = p
		if serr := cfg.Save(c.Paths.File); serr != nil {
			fmt.Fprintln(c.Stderr, "error:", serr)
		}
		return c.fail(err)
	}
	say("Stored the git token for %s in %s", r.URL, c.storeName(store))
	say("Added %s at %s; runs read it at %s/%s", r.URL, cmpRef(r.Ref), workspace.ExtraPath, r.Name())
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
		fmt.Fprintf(c.Stdout, "%s\t%s\t%s\n", r.Name(), r.URL, cmpRef(r.Ref))
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
	p := cfg.Projects[i]
	j := slices.IndexFunc(p.ExtraRepositories, func(r config.ExtraRepository) bool { return r.URL == rest[0] || r.Name() == rest[0] })
	if j < 0 {
		return c.fail(fmt.Errorf("project %s has no extra repository %s", p.Key, rest[0]))
	}
	r := p.ExtraRepositories[j]
	cfg.Projects[i].ExtraRepositories = slices.Delete(slices.Clone(p.ExtraRepositories), j, j+1)
	if err := cfg.Save(c.Paths.File); err != nil {
		return c.fail(err)
	}
	if err := c.Secrets.Delete(p, secrets.ExtraGitToken(r.URL)); err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.Stdout, "Removed %s\n", r.URL)
	c.reload()
	return 0
}

// cmpRef is the ref, or what an empty one means.
func cmpRef(ref string) string {
	if ref == "" {
		return "the default branch"
	}
	return ref
}

// reload makes a running daemon read the config again; it only warns. A
// reload cuts the runs in flight, so while there are any it drains the
// daemon instead, as stop --drain, and starts the service again (decision 2026-11-18-drain-before-reload).
func (c *CLI) reload() {
	cl := localapi.Client{Socket: c.Paths.Socket}
	st, err := cl.Status(context.Background())
	if err == nil {
		if issues := inFlight(st); len(issues) > 0 {
			c.drainAndStart(issues)
			return
		}
		err = cl.Reload(context.Background())
	}
	if err != nil && !errors.Is(err, localapi.ErrNotRunning) {
		fmt.Fprintf(c.Stdout, "Warning: daemon reload failed: %v.\n", err)
	}
}

// drainAndStart lets the daemon finish issues, or stops it at once when the
// person asks, and then starts the service again, which reads the new config.
func (c *CLI) drainAndStart(issues []string) {
	fmt.Fprintf(c.Stdout, "%s runs %s; it takes the change once they have finished, by stopping and starting again.\n", name.Name, strings.Join(issues, ", "))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := c.drainAndWait(ctx, true); err != nil && !errors.Is(err, localapi.ErrNotRunning) {
		fmt.Fprintf(c.Stdout, "Warning: stopped waiting (%v): %s still stops once its runs have finished and is not started again; start it then with `%s run` or its service.\n", err, name.Name, name.Name)
		return
	}
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
