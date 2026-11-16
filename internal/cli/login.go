package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/environment"
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
	// --tty needs the real terminal, not c.Stdin/c.Stdout.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return c.fail(fmt.Errorf("%s login: %w", clientName, err))
	}

	fmt.Fprintf(c.Stdout, "%s is signed in for project %s; the credentials are on its %s state volume\n", clientName, p.Key, clientName)
	c.reload()
	return 0
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
