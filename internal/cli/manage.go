package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// manage is the interactive screen (CSR-172): pick a project to see which
// tokens it holds, never their values, and replace or delete one; or add a
// project. Built on Charm's huh (decision 2026-11-12-manage-cli). A new
// feature is one more entry in the menu it belongs to.
func (c *CLI) manage(args []string) int {
	f := c.flags("manage", "")
	if err := f.Parse(args); err != nil || f.NArg() > 0 {
		if err == nil {
			f.Usage()
		}
		return 2
	}
	if !isTerminal(c.Stdin) || !isTerminal(c.Stdout) {
		return c.fail(fmt.Errorf("manage needs an interactive terminal; from a script use `%s add`, `%s key set` or `%s remove`", name.Name, name.Name, name.Name))
	}
	const add, quit = -1, -2
	for {
		cfg, err := c.loadConfig()
		if err != nil {
			return c.fail(err)
		}
		var opts []huh.Option[int]
		for i, p := range cfg.Projects {
			opts = append(opts, huh.NewOption(p.Key+"  "+p.URL, i))
		}
		opts = append(opts, huh.NewOption("+ Add a project", add), huh.NewOption("Quit", quit))
		choice := opts[0].Value // the value is also where the cursor starts
		err = form(huh.NewSelect[int]().Title(name.Name + " projects").Options(opts...).Value(&choice))
		if errors.Is(err, huh.ErrUserAborted) || (err == nil && choice == quit) {
			return 0
		}
		if err != nil {
			return c.fail(err)
		}
		if choice == add {
			c.addProject(formAsk(false), formAsk(true)) // it prints its own errors; the menu comes back either way
			continue
		}
		if err := c.manageProject(cfg.Projects[choice]); err != nil {
			return c.fail(err)
		}
	}
}

// manageProject lists p's tokens and lets the person replace or delete one,
// until they go back. Only a failing terminal is returned as an error.
func (c *CLI) manageProject(p config.Project) error {
	const back = -1
	for {
		ts := c.tokens(p)
		var opts []huh.Option[int]
		for i, t := range ts {
			opts = append(opts, huh.NewOption(fmt.Sprintf("%-50s %s", t.label, c.tokenStatus(p, t.kind)), i))
		}
		opts = append(opts, huh.NewOption("Back", back))
		choice := 0
		err := form(huh.NewSelect[int]().Title(p.Key + " at " + p.URL).Description("Tokens are never shown.").Options(opts...).Value(&choice))
		if errors.Is(err, huh.ErrUserAborted) || (err == nil && choice == back) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := c.manageToken(p, ts[choice]); err != nil {
			return err
		}
	}
}

// manageToken offers to replace or delete one token.
func (c *CLI) manageToken(p config.Project, t token) error {
	action := "replace"
	err := form(huh.NewSelect[string]().Title(t.label+" of "+p.Key).
		Options(huh.NewOption("Replace", "replace"), huh.NewOption("Delete", "delete"), huh.NewOption("Back", "")).Value(&action))
	if errors.Is(err, huh.ErrUserAborted) {
		return nil
	}
	if err != nil {
		return err
	}
	switch action {
	case "replace":
		for {
			v, err := formAsk(true)("New " + t.label)
			if errors.Is(err, errEOF) || (err == nil && v == "") {
				return nil
			}
			if err != nil {
				return err
			}
			if err := c.replaceToken(p, t, v); err != nil {
				fmt.Fprintf(c.Stdout, "Not stored: %v.\n", err)
				continue
			}
			return nil
		}
	case "delete":
		ok := false
		err := form(huh.NewConfirm().Title("Delete the " + t.label + " of " + p.Key + "?").Description(t.deleted).
			Affirmative("Delete").Negative("Cancel").Value(&ok))
		if errors.Is(err, huh.ErrUserAborted) || (err == nil && !ok) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := c.deleteToken(p, t); err != nil {
			fmt.Fprintln(c.Stderr, "error:", err)
		}
	}
	return nil
}

// token is one secret a project may hold, as manage shows it.
type token struct {
	label   string
	kind    secrets.Kind
	check   func(c *CLI, p config.Project, v string) error // nil: stored unchecked, like add does with the git token
	deleted string                                         // what deleting it means for runs
}

// tokens are the secrets p may hold: the ones add asks for, then one git token per extra repository.
func (c *CLI) tokens(p config.Project) []token {
	const unusable = "The project cannot run without it until you set it again."
	ts := []token{
		{"runner token", secrets.RunnerToken, checkRunnerToken, unusable},
		{"git token", secrets.GitToken, nil, unusable},
	}
	for _, m := range modelKeys {
		ts = append(ts, token{m.client + " key", secrets.ClientKeys[m.client], nil, "Runs with " + m.client + " use its login, if it has one."})
	}
	for _, r := range p.ExtraRepositories {
		ts = append(ts, token{"git token for " + r.Name(), secrets.ExtraGitToken(r.URL), func(c *CLI, p config.Project, v string) error {
			if err := workspace.LsRemote(context.Background(), c.exec, p.Key, r.URL, r.Ref, v); err != nil {
				return fmt.Errorf("this token cannot read %s at %s: %w", r.URL, cmpRef(r.Ref), err)
			}
			return nil
		}, "Runs cannot fetch " + r.URL + " until you set it again."})
	}
	return ts
}

// checkRunnerToken checks v against p's instance, as add does, and that it is p's.
func checkRunnerToken(c *CLI, p config.Project, v string) error {
	proj, err := cinderline.New(p.URL, v, c.Version).GetProject(context.Background())
	var apiErr *cinderline.APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
		return errors.New("the instance rejected this runner token")
	}
	if err != nil {
		return fmt.Errorf("%s could not be reached: %w", p.URL, err)
	}
	if proj.Key != p.Key {
		return fmt.Errorf("this runner token is for project %s, not %s", proj.Key, p.Key)
	}
	return nil
}

// tokenStatus says whether p holds a secret of kind k and where, never its value.
func (c *CLI) tokenStatus(p config.Project, k secrets.Kind) string {
	_, store, err := c.Secrets.Get(p, k)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		return "not set"
	case err != nil:
		return "unreadable: " + err.Error()
	case store == secrets.Keychain:
		return "set, in the keychain"
	}
	return "set, in the fallback file"
}

// replaceToken checks v when t has a check, stores it and reloads a running daemon.
func (c *CLI) replaceToken(p config.Project, t token, v string) error {
	if t.check != nil {
		if err := t.check(c, p, v); err != nil {
			return err
		}
	}
	store, err := c.Secrets.Set(p, t.kind, v)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Stdout, "Stored the %s of %s in %s\n", t.label, p.Key, c.storeName(store))
	c.reload()
	return nil
}

// deleteToken deletes t from both stores and reloads a running daemon.
func (c *CLI) deleteToken(p config.Project, t token) error {
	if err := c.Secrets.Delete(p, t.kind); err != nil {
		return err
	}
	fmt.Fprintf(c.Stdout, "Deleted the %s of %s\n", t.label, p.Key)
	c.reload()
	return nil
}

// form runs one field with its key help shown.
func form(f huh.Field) error {
	return huh.NewForm(huh.NewGroup(f)).Run()
}

// formAsk is add's ask as a form field; secret hides what is typed. Ctrl-C is errEOF.
func formAsk(secret bool) func(prompt string) (string, error) {
	return func(prompt string) (string, error) {
		var v string
		in := huh.NewInput().Title(strings.TrimSuffix(strings.TrimSpace(prompt), ":")).Value(&v)
		if secret {
			in.EchoMode(huh.EchoModePassword)
		}
		if err := form(in); errors.Is(err, huh.ErrUserAborted) {
			return "", errEOF
		} else if err != nil {
			return "", err
		}
		return strings.TrimSpace(v), nil
	}
}

// isTerminal reports whether f is an *os.File on a terminal.
func isTerminal(f any) bool {
	file, ok := f.(*os.File)
	return ok && term.IsTerminal(file.Fd())
}
