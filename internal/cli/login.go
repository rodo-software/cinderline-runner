package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/tools"
)

func (c *CLI) login(args []string) int {
	if len(args) == 0 || args[0] == "" {
		c.flags("login", "CLIENT "+projectUsage).Usage()
		return 2
	}
	clientName := args[0]
	cfg, i, _, code := c.repoFlags("login", "CLIENT "+projectUsage, args[1:], 0)
	if code != 0 {
		return code
	}
	p := cfg.Projects[i]
	key := p.ID() // names the state volume the credentials go on

	// Validate the client name before any docker call.
	if _, err := client.LoginArgv(clientName, key, "", tools.Mount{}); err != nil {
		return c.fail(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	image, mount, err := c.agentImage(ctx, key)
	if err != nil {
		return c.fail(err)
	}
	if err := client.PrepareState(ctx, c.exec, key, clientName); err != nil {
		return c.fail(err)
	}

	argv, err := client.LoginArgv(clientName, key, image, mount)
	if err != nil {
		return c.fail(err)
	}

	if clientName == "gemini" {
		fmt.Fprintln(c.Stdout, `After "Authentication succeeded", type /quit to finish.`)
	}
	tty := c.tty
	if tty == nil {
		tty = runOnTerminal
	}
	if err := tty(ctx, argv); err != nil {
		return c.fail(fmt.Errorf("%s login: %w", clientName, err))
	}
	// A CLI can end its sign-in with success and still save nothing where
	// the runs look (CLR-199), so success is the file on the volume.
	saved, err := client.LoginExists(ctx, c.exec, key, clientName)
	if err != nil {
		return c.fail(fmt.Errorf("%s login: credentials not checked on %s: %w", clientName, client.StateVolume(key, clientName), err))
	}
	if !saved {
		return c.fail(fmt.Errorf("%s login ended, but its credentials are not on %s; the runs cannot use it, sign in again", clientName, client.StateVolume(key, clientName)))
	}

	// A stored key wins over a login in detection and runs, so the login
	// replaces it (CLR-210, decision 2026-11-19-login-replaces-key).
	kind, replaced := secrets.ClientKeys[clientName], ""
	switch _, _, err := c.Secrets.Get(p, kind); {
	case err == nil:
		if err := c.Secrets.Delete(p, kind); err != nil {
			return c.fail(fmt.Errorf("%s is signed in, but the %s key stored for project %s, which wins over the login, is not deleted: %w; delete it with `%s key delete %s --project %s`",
				clientName, clientName, p.Key, err, name.Name, clientName, p.Key))
		}
		replaced = fmt.Sprintf(" and replace the %s key stored for it, now deleted", clientName)
	case !errors.Is(err, secrets.ErrNotFound):
		return c.fail(fmt.Errorf("%s is signed in, but the %s key stored for project %s, which wins over the login, is not checked: %w", clientName, clientName, p.Key, err))
	}

	fmt.Fprintf(c.Stdout, "%s is signed in for project %s; the credentials are on its %s state volume%s\n", clientName, p.Key, clientName, replaced)
	c.reload()
	return 0
}

// runOnTerminal runs argv on the real terminal: --tty needs it, not
// c.Stdin/c.Stdout.
func runOnTerminal(ctx context.Context, argv []string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// agentImage returns the default image and the tools mount for it, with a
// line on Stdout before each slow first-time step and the image's warnings
// on Stderr, so an interactive command doesn't look hung (CSR-149).
func (c *CLI) agentImage(ctx context.Context, project string) (string, tools.Mount, error) {
	env := &environment.Docker{
		Project: project, Exec: c.exec,
		Log: slog.New(slog.NewTextHandler(c.Stderr, nil)),
		OnFetch: func() {
			fmt.Fprintln(c.Stdout, "Fetching the agent image (first time only, this can take a few minutes)…")
		},
	}
	image, err := env.Image(ctx)
	if err != nil {
		return "", tools.Mount{}, err
	}
	t := &tools.Tools{State: c.Paths.State, Exec: c.exec, HTTP: c.http, OnFill: func() {
		fmt.Fprintln(c.Stdout, "Installing the agent tools (first time only)…")
	}}
	mount, err := t.Mount(ctx, image)
	return image, mount, err
}
