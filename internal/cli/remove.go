package cli

import (
	"fmt"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// remove deletes a project's four secrets and its config entry.
func (c *CLI) remove(args []string) int {
	f := c.flags("remove", "[-url URL] KEY")
	instance := f.String("url", "", "URL of the instance, when several projects share the key")
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
	p, err := findProject(cfg, f.Arg(0), *instance)
	if err != nil {
		return c.fail(err)
	}
	for _, k := range []secrets.Kind{secrets.RunnerToken, secrets.GitToken, secrets.AnthropicCredential, secrets.OpenRouterKey, secrets.OpenAIKey, secrets.GeminiKey} {
		if err := c.Secrets.Delete(p, k); err != nil {
			return c.fail(err)
		}
	}
	cfg.Projects = without(cfg.Projects, p)
	if err := cfg.Save(c.Paths.File); err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.Stdout, "Removed %s at %s\n", p.Key, p.URL)
	return 0
}

// findProject returns the one project with key, at instance when that is not
// empty; a key on several instances needs instance.
func findProject(cfg *config.Config, key, instance string) (config.Project, error) {
	want := strings.TrimRight(instance, "/")
	var matches []config.Project
	for _, p := range cfg.Projects {
		if p.Key == key && (want == "" || p.URL == want) {
			matches = append(matches, p)
		}
	}
	switch {
	case len(matches) == 0 && want != "":
		return config.Project{}, fmt.Errorf("no project %s at %s", key, want)
	case len(matches) == 0:
		return config.Project{}, fmt.Errorf("no project %s", key)
	case len(matches) > 1:
		var urls []string
		for _, p := range matches {
			urls = append(urls, p.URL)
		}
		return config.Project{}, fmt.Errorf("projects on several instances have key %s; choose one with -url: %s", key, strings.Join(urls, ", "))
	}
	return matches[0], nil
}
