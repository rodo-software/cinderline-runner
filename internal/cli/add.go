package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/rodo-software/cinderstack-runner/internal/cinderstack"
	"github.com/rodo-software/cinderstack-runner/internal/config"
	"github.com/rodo-software/cinderstack-runner/internal/forge"
	"github.com/rodo-software/cinderstack-runner/internal/name"
	"github.com/rodo-software/cinderstack-runner/internal/secrets"
)

// errEOF is stdin ending before an answer.
var errEOF = errors.New("input ended before setup was complete; nothing was stored")

// add asks for a project's details, checking each answer as it comes (EMBER-26).
// Tokens are read in plain text like every other answer: hiding them needs
// golang.org/x/term, a dependency the plan does not allow.
func (c *CLI) add(args []string) int {
	f := c.flags("add", "")
	if err := f.Parse(args); err != nil || f.NArg() > 0 {
		if err == nil {
			f.Usage()
		}
		return 2
	}
	cfg, err := c.loadConfig()
	if err != nil {
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

	// URL and token, re-asking the token when it is rejected and the URL when it is not an instance.
	var instance, token string
	var proj cinderstack.Project
	for {
		for instance == "" {
			v, err := ask("URL of the " + name.Name + " instance: ")
			if err != nil {
				return c.fail(err)
			}
			if u, err := url.Parse(v); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				say("%q is not an absolute http or https URL.", v)
				continue
			}
			instance = strings.TrimRight(v, "/")
		}
		for token == "" {
			if token, err = ask("Runner token: "); err != nil {
				return c.fail(err)
			}
		}
		proj, err = cinderstack.New(instance, token, c.Version).GetProject(context.Background())
		var apiErr *cinderstack.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
			say("The instance rejected this runner token.")
			token = ""
			continue
		}
		if err != nil {
			say("%s could not be reached as a %s instance: %v", instance, name.Name, err)
			instance = ""
			continue
		}
		break
	}
	say("Project %s: %s", proj.Key, proj.Name)
	p := config.Project{URL: instance, Key: proj.Key}
	for _, q := range cfg.Projects {
		if q.URL == p.URL && q.Key == p.Key {
			return c.fail(fmt.Errorf("%s at %s is already added; run `%s remove %s` first", p.Key, p.URL, name.Name, p.Key))
		}
	}

	prompt := "Repository: "
	if proj.RepositoryURL != "" {
		prompt = "Repository [" + proj.RepositoryURL + "]: "
	}
	for p.Repository == "" {
		v, err := ask(prompt)
		if err != nil {
			return c.fail(err)
		}
		if v == "" {
			v = proj.RepositoryURL
		}
		u, err := url.Parse(v)
		if err != nil || !u.IsAbs() || u.Host == "" {
			say("%q is not an absolute URL with a host.", v)
			continue
		}
		if proj.RepositoryURL != "" && !config.SameRepository(v, proj.RepositoryURL) {
			say("%s is not the project's repository, %s.", v, proj.RepositoryURL)
			continue
		}
		p.Repository = v
	}

	// Stored, not validated against the forge.
	var gitToken string
	for gitToken == "" {
		if gitToken, err = ask("Git token: "); err != nil {
			return c.fail(err)
		}
	}

	// The config first, so a failed save stores no secrets.
	others := cfg.Projects
	cfg.Projects = append(append([]config.Project(nil), others...), p)
	if err := cfg.Save(c.Paths.File); err != nil {
		return c.fail(err)
	}
	for _, s := range []struct {
		kind  secrets.Kind
		label string
		value string
	}{{secrets.RunnerToken, "runner token", token}, {secrets.GitToken, "git token", gitToken}} {
		store, err := c.Secrets.Set(p, s.kind, s.value)
		if err != nil {
			c.undoAdd(cfg, p)
			return c.fail(err)
		}
		say("Stored %s in %s", s.label, c.storeName(store))
	}

	if len(others) > 0 {
		if code := c.offerCopy(others, p, ask); code != 0 {
			return code
		}
	}

	build := c.Forge
	if build == nil {
		build = forge.New
	}
	// CI secrets check (EMBER-56): a warning, never fatal.
	var exposed []string
	fg, err := build(proj.Forge, p.Repository, gitToken)
	if err == nil {
		exposed, err = fg.CISecrets(context.Background())
	}
	if err != nil {
		say("Warning: could not check whether CI on the runner's branches can read secrets: %v.", err)
	} else if len(exposed) > 0 {
		say("Warning: CI on any branch, including the runner's %s/ branches, can read: %s.", name.Name, strings.Join(exposed, ", "))
	}
	if err != nil || len(exposed) > 0 {
		say(`Move them to a protected environment or protected branches: see "CI secrets" in the README.`)
	}

	say("Added %s at %s", p.Key, p.URL)
	return 0
}

// undoAdd removes p and its tokens again after a failed Set; errors are printed, the Set error is what fails.
func (c *CLI) undoAdd(cfg *config.Config, p config.Project) {
	for _, k := range []secrets.Kind{secrets.RunnerToken, secrets.GitToken} {
		if err := c.Secrets.Delete(p, k); err != nil {
			fmt.Fprintln(c.Stderr, "error:", err)
		}
	}
	cfg.Projects = without(cfg.Projects, p)
	if err := cfg.Save(c.Paths.File); err != nil {
		fmt.Fprintln(c.Stderr, "error:", err)
	}
}

// offerCopy offers to copy model credentials from one of others to p (decision
// 2026-11-05-runner-rewrite-details point 10). Empty input or EOF means none.
func (c *CLI) offerCopy(others []config.Project, p config.Project, ask func(string) (string, error)) int {
	for i, q := range others {
		fmt.Fprintf(c.Stdout, "%d. %s at %s\n", i+1, q.Key, q.URL)
	}
	for {
		v, err := ask("Copy model credentials from one of these? [number or empty for none] ")
		if errors.Is(err, errEOF) || (err == nil && v == "") {
			return 0
		}
		if err != nil {
			return c.fail(err)
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > len(others) {
			fmt.Fprintf(c.Stdout, "Type a number from 1 to %d, or nothing.\n", len(others))
			continue
		}
		from := others[n-1]
		err = c.Secrets.CopyModelCredentials(from, p)
		if errors.Is(err, secrets.ErrNotFound) {
			fmt.Fprintf(c.Stdout, "%s at %s holds no model credentials; none copied.\n", from.Key, from.URL)
			return 0
		}
		if err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.Stdout, "Copied model credentials from %s at %s\n", from.Key, from.URL)
		return 0
	}
}
