package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// repoFlags parses --project and -url and returns the config and the index
// of the project in it; code is non-zero when the command must stop.
func (c *CLI) repoFlags(cmd, usage string, args []string, nargs int) (cfg *config.Config, i int, rest []string, code int) {
	f := c.flags(cmd, usage)
	project := f.String("project", "", "project key")
	instance := f.String("url", "", "URL of the instance, when several projects share the key")
	if err := f.Parse(args); err != nil || *project == "" || f.NArg() != nargs {
		if err == nil {
			f.Usage()
		}
		return nil, 0, nil, 2
	}
	cfg, err := c.loadConfig()
	if err != nil {
		return nil, 0, nil, c.fail(err)
	}
	p, err := findProject(cfg, *project, *instance)
	if err != nil {
		return nil, 0, nil, c.fail(err)
	}
	i = slices.IndexFunc(cfg.Projects, func(q config.Project) bool { return q.URL == p.URL && q.Key == p.Key })
	return cfg, i, f.Args(), 0
}

// addRepo adds an extra repository that the project's runs can read
// (CSR-160). It asks for the URL, an optional ref and a git token, and checks
// with git ls-remote that the token can read the ref before storing anything.
func (c *CLI) addRepo(args []string) int {
	cfg, i, _, code := c.repoFlags("add-repo", "--project KEY [-url URL]", args, 0)
	if code != 0 {
		return code
	}
	p := cfg.Projects[i]
	if err := c.checkDocker(context.Background(), false); err != nil {
		return c.fail(err)
	}
	in := bufio.NewScanner(c.Stdin)
	ask := func(prompt string) (string, error) {
		fmt.Fprint(c.Stdout, prompt)
		if !in.Scan() {
			if err := in.Err(); err != nil {
				return "", err
			}
			return "", errEOF
		}
		return strings.TrimSpace(in.Text()), nil
	}
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
		if err := workspace.LsRemote(context.Background(), c.exec, p.Key, r.URL, r.Ref, token); err != nil {
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
	cfg, i, _, code := c.repoFlags("list-repos", "--project KEY [-url URL]", args, 0)
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
	cfg, i, rest, code := c.repoFlags("remove-repo", "--project KEY [-url URL] URL|NAME", args, 1)
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

// reload makes a running daemon read the config again; it only warns.
func (c *CLI) reload() {
	if err := (localapi.Client{Socket: c.Paths.Socket}).Reload(context.Background()); err != nil && !errors.Is(err, localapi.ErrNotRunning) {
		fmt.Fprintf(c.Stdout, "Warning: daemon reload failed: %v.\n", err)
	}
}
