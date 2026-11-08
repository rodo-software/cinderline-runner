package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// uninstall removes the service, the runner's containers, networks, volumes
// and images, and the config and state folders with the secrets in them.
// Docker failure stops it before anything is deleted, so a second uninstall
// can finish the job once Docker runs.
func (c *CLI) uninstall(args []string) int {
	f := c.flags("uninstall", "[-yes]")
	yes := f.Bool("yes", false, "proceed without asking for confirmation")
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
	exe, err := os.Executable()
	if err != nil {
		return c.fail(err)
	}
	if !*yes {
		fmt.Fprint(c.Stdout, "This removes "+name.Name+"'s service, containers, volumes, images, config and secrets. Type yes to continue: ")
		sc := bufio.NewScanner(c.Stdin)
		sc.Scan()
		switch strings.ToLower(strings.TrimSpace(sc.Text())) {
		case "yes", "y":
		default:
			fmt.Fprintln(c.Stderr, "Cancelled")
			return 1
		}
	}
	removeService := c.removeService
	if removeService == nil {
		removeService = uninstallService
	}
	if err := removeService(exe); err != nil {
		fmt.Fprintln(c.Stderr, "warning: could not remove the service (it may not be installed):", err)
	}
	label := "label=" + name.Name + ".project"
	steps := [][2][]string{
		{{"ps", "--all", "--quiet", "--filter", label}, {"rm", "--force", "--volumes"}},
		{{"network", "ls", "--quiet", "--filter", label}, {"network", "rm"}},
		{{"volume", "ls", "--quiet", "--filter", label}, {"volume", "rm", "--force"}},
		{{"volume", "ls", "--quiet", "--filter", "name=" + name.Name + "-tools-"}, {"volume", "rm", "--force"}},
		{{"image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference=" + name.Name + "-default"}, {"image", "rm", "--force"}},
	}
	for _, step := range steps {
		if err := c.dockerRemove(step[0], step[1]); err != nil {
			return c.fail(fmt.Errorf("start Docker and run %s uninstall again: %w", name.Name, err))
		}
	}
	for _, p := range cfg.Projects {
		for _, k := range []secrets.Kind{secrets.RunnerToken, secrets.GitToken, secrets.AnthropicCredential, secrets.OpenRouterKey, secrets.OpenAIKey, secrets.GeminiKey} {
			if err := c.Secrets.Delete(p, k); err != nil {
				return c.fail(err)
			}
		}
	}
	for _, dir := range []string{c.Paths.Dir, c.Paths.State} {
		if err := os.RemoveAll(dir); err != nil {
			return c.fail(err)
		}
	}
	fmt.Fprintf(c.Stdout, "Removed %s's service, containers, volumes, images, config and secrets\n", name.Name)
	if len(cfg.Projects) > 0 {
		fmt.Fprintln(c.Stdout, "Revoke the runner token of each project in Cinderline:")
		for _, p := range cfg.Projects {
			fmt.Fprintf(c.Stdout, "  %s at %s\n", p.Key, p.URL)
		}
	}
	fmt.Fprintf(c.Stdout, "Delete the binary yourself: %s\n", exe)
	return 0
}

// dockerRemove lists what list finds and removes it; a list that names
// nothing runs no rm argument.
func (c *CLI) dockerRemove(list, rm []string) error {
	exec := c.exec
	if exec == nil {
		exec = workspace.RunCommand
	}
	out, err := exec(context.Background(), nil, append([]string{"docker"}, list...)...)
	if err != nil {
		return err
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return nil
	}
	_, err = exec(context.Background(), nil, append(append([]string{"docker"}, rm...), ids...)...)
	return err
}
