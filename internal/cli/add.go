package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/manage"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// errEOF is stdin ending before an answer.
var errEOF = errors.New("input ended before setup was complete; nothing was stored")

// lineAsk returns an ask that prints prompt and reads one trimmed line from stdin.
func (c *CLI) lineAsk() func(prompt string) (string, error) {
	in := bufio.NewScanner(c.Stdin)
	return func(prompt string) (string, error) {
		fmt.Fprint(c.Stdout, prompt)
		if !in.Scan() {
			if err := in.Err(); err != nil {
				return "", err
			}
			return "", errEOF
		}
		return strings.TrimSpace(in.Text()), nil
	}
}

// add asks for a project's details on stdin, one line per answer, checking
// each as it comes (EMBER-26). Tokens are read in plain text like every
// other answer, so add works from a script.
func (c *CLI) add(args []string) int {
	if !c.parsed(c.flags("add", ""), args, 0) {
		return 2
	}
	ask := c.lineAsk()
	if err := c.checkDocker(context.Background(), false); err != nil {
		return c.fail(err)
	}
	cfg, err := c.loadConfig()
	if err != nil {
		return c.fail(err)
	}
	say := func(format string, a ...any) { fmt.Fprintf(c.Stdout, format+"\n", a...) }

	// URL and token, re-asking the token when it is rejected and the URL when
	// it is not an instance; each answer is checked as manage.AddProject checks it again.
	var in manage.NewProject
	var proj cinderline.Project
	for {
		for in.URL == "" {
			v, err := ask("URL of the " + name.Name + " instance: ")
			if err != nil {
				return c.fail(err)
			}
			if in.URL, err = manage.CheckInstanceURL(v); err != nil {
				say("%v.", err)
			}
		}
		for in.RunnerToken == "" {
			if in.RunnerToken, err = ask("Runner token: "); err != nil {
				return c.fail(err)
			}
		}
		proj, err = manage.Instance(context.Background(), in.URL, in.RunnerToken, c.Version)
		if errors.Is(err, manage.ErrTokenRejected) {
			say("The instance rejected this runner token.")
			in.RunnerToken = ""
			continue
		}
		if err != nil {
			say("%v", err)
			in.URL = ""
			continue
		}
		break
	}
	say("Project %s: %s", proj.Key, proj.Name)
	if _, err := manage.Entry(cfg, in.URL, proj); err != nil {
		return c.fail(err)
	}

	prompt := "Repository: "
	if proj.RepositoryURL != "" {
		prompt = "Repository [" + proj.RepositoryURL + "]: "
	}
	for in.Repository == "" {
		v, err := ask(prompt)
		if err != nil {
			return c.fail(err)
		}
		if in.Repository, err = manage.CheckRepository(proj, v); err != nil {
			say("%v.", err)
		}
	}

	// Stored, not validated against the forge.
	for in.GitToken == "" {
		if in.GitToken, err = ask("Git token: "); err != nil {
			return c.fail(err)
		}
	}

	others := cfg.Projects
	p, stores, err := manage.AddProject(context.Background(), c.Paths, c.Secrets, c.Version, in)
	if err != nil {
		return c.fail(err)
	}
	say("Stored runner token in %s", c.storeName(stores[0]))
	say("Stored git token in %s", c.storeName(stores[1]))

	if len(others) > 0 {
		if code := c.offerCopy(others, p, ask); code != 0 {
			return code
		}
	}
	// Optional; EOF ends them like the copy offer, as the project is already added.
	for i := 0; i < len(modelKeys); {
		m := modelKeys[i]
		v, err := ask(m.label + " for " + m.client + " [empty to skip]: ")
		if errors.Is(err, errEOF) {
			break
		}
		if err != nil {
			return c.fail(err)
		}
		if v != "" {
			if err := c.storeKey(p, secrets.ClientKeys[m.client], m.label+" for "+m.client, v); err != nil {
				say("Not stored: %v.", err) // re-asks, e.g. after a key over the size limit
				continue
			}
		}
		i++
	}

	say(`Check that the repository's CI cannot read secrets from %s/ branches: see "CI secrets" in the README.`, name.Name)

	if p.Workspace == config.DefaultWorkspace {
		say("Added %s at %s", p.Key, p.URL)
	} else {
		say("Added %s in workspace %s at %s", p.Key, p.Workspace, p.URL)
	}
	c.reload()
	return 0
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
