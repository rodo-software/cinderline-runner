package cli

import (
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// providerFlags parses a provider command, which has no --project: a
// provider is the runner's, for every project (decision 2026-11-17-local-model-providers).
// A non-nil free and maxAgents get the command's --free and --max-agents flags.
func (c *CLI) providerFlags(cmd, usage string, args []string, nargs int, free *bool, maxAgents *int) (*config.Config, []string, int) {
	f := c.flags(cmd, usage)
	if free != nil {
		f.BoolVar(free, "free", false, freeHelp)
	}
	if maxAgents != nil {
		f.IntVar(maxAgents, "max-agents", 0, maxAgentsHelp)
	}
	if !c.parsed(f, args, nargs) {
		return nil, nil, 2
	}
	cfg, err := c.loadConfig()
	if err != nil {
		return nil, nil, c.fail(err)
	}
	return cfg, f.Args(), 0
}

// freeHelp is the help of --free (decision 2026-11-19-free-model-providers).
const freeHelp = "the provider's models cost nothing per token, such as a model on your own machine; Cinderline then prices them at $0"

// maxAgentsHelp is the help of --max-agents (CLR-243).
const maxAgentsHelp = "the most agent turns on the provider at once, over every project of this runner; a turn past it waits for one to end; 0 is no limit"

// agentLimit says what a provider's max_agents allows, after "has".
func agentLimit(n int) string {
	switch n {
	case 0:
		return "no agent limit"
	case 1:
		return "at most 1 agent at once"
	}
	return fmt.Sprintf("at most %d agents at once", n)
}

// providerIndex is the index of the runner's provider n, or an error.
func providerIndex(cfg *config.Config, n string) (int, error) {
	j := slices.IndexFunc(cfg.Providers, func(q config.Provider) bool { return q.Name == n })
	if j < 0 {
		return j, fmt.Errorf("this runner has no provider %s; list them with list-providers", n)
	}
	return j, nil
}

// addProvider adds a model provider for every project's OpenCode agents
// and asks for its optional key.
func (c *CLI) addProvider(args []string) int {
	var free bool
	var maxAgents int
	cfg, rest, code := c.providerFlags("add-provider", "[--free] [--max-agents N] NAME URL", args, 2, &free, &maxAgents)
	if code != 0 {
		return code
	}
	pr := config.Provider{Name: rest[0], BaseURL: rest[1], Free: free, MaxAgents: maxAgents}
	if err := pr.Validate(); err != nil {
		return c.fail(err)
	}
	if _, err := providerIndex(cfg, pr.Name); err == nil {
		return c.fail(fmt.Errorf("this runner already has a provider %s; remove it first with remove-provider", pr.Name))
	}
	key, err := c.secretAsk("API key for " + pr.Name + " [empty for none]: ")
	if err != nil {
		return c.fail(err)
	}
	// The config first, so a failed save stores no secret.
	old := cfg.Providers
	cfg.Providers = append(slices.Clone(old), pr)
	if err := cfg.Save(c.Paths.File); err != nil {
		return c.fail(err)
	}
	kind := secrets.ProviderKey(pr.Name)
	if key == "" {
		// A key left under this name must not go to the new URL.
		err = c.Secrets.Delete(secrets.Runner, kind)
	} else {
		err = c.storeKey(secrets.Runner, kind, "the key of "+pr.Name, key)
	}
	if err != nil {
		cfg.Providers = old
		if serr := cfg.Save(c.Paths.File); serr != nil {
			fmt.Fprintln(c.Stderr, "error:", serr)
		}
		return c.fail(err)
	}
	fmt.Fprintf(c.Stdout, "Added %s at %s; OpenCode agents of every project use it as %s/<model id>\n", pr.Name, pr.BaseURL, pr.Name)
	c.reload()
	return 0
}

// listProviders prints the runner's providers, one per line, and whether
// each has a key, never the key.
func (c *CLI) listProviders(args []string) int {
	cfg, _, code := c.providerFlags("list-providers", "", args, 0, nil, nil)
	if code != 0 {
		return code
	}
	for _, pr := range cfg.Providers {
		key := "key set"
		if _, _, err := c.Secrets.Get(secrets.Runner, secrets.ProviderKey(pr.Name)); errors.Is(err, secrets.ErrNotFound) {
			key = "no key"
		} else if err != nil {
			return c.fail(err)
		}
		if pr.Free {
			key += "\tfree"
		}
		switch pr.MaxAgents {
		case 0:
		case 1:
			key += "\tmax 1 agent"
		default:
			key += fmt.Sprintf("\tmax %d agents", pr.MaxAgents)
		}
		fmt.Fprintf(c.Stdout, "%s\t%s\t%s\n", pr.Name, pr.BaseURL, key)
	}
	return 0
}

// setProviderUsage is set-provider's arguments.
const setProviderUsage = "NAME [--free=true|false] [--max-agents N]"

// setProvider marks a provider free or not and sets its agent limit; its key stays.
func (c *CLI) setProvider(args []string) int {
	// NAME comes before the flags, as key's NAME does: flag parsing stops at it.
	f := c.flags("set-provider", setProviderUsage)
	free := f.Bool("free", false, freeHelp)
	maxAgents := f.Int("max-agents", 0, maxAgentsHelp)
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		f.Usage()
		return 2
	}
	if !c.parsed(f, args[1:], 0) {
		return 2
	}
	set := map[string]bool{}
	f.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	if len(set) == 0 {
		f.Usage()
		return 2
	}
	cfg, err := c.loadConfig()
	if err != nil {
		return c.fail(err)
	}
	j, err := providerIndex(cfg, args[0])
	if err != nil {
		return c.fail(err)
	}
	old, pr := cfg.Providers[j], &cfg.Providers[j]
	if set["free"] {
		pr.Free = *free
	}
	if set["max-agents"] {
		pr.MaxAgents = *maxAgents
	}
	if err := pr.Validate(); err != nil {
		return c.fail(err)
	}
	state := "not free"
	if pr.Free {
		state = "free"
	}
	if *pr == old {
		// No reload: it would drain the runs in flight for nothing.
		if set["free"] {
			fmt.Fprintf(c.Stdout, "%s is already %s\n", pr.Name, state)
		}
		if set["max-agents"] {
			fmt.Fprintf(c.Stdout, "%s already has %s\n", pr.Name, agentLimit(pr.MaxAgents))
		}
		return 0
	}
	if err := cfg.Save(c.Paths.File); err != nil {
		return c.fail(err)
	}
	if set["free"] {
		fmt.Fprintf(c.Stdout, "Marked %s %s\n", pr.Name, state)
	}
	if set["max-agents"] {
		fmt.Fprintf(c.Stdout, "%s now has %s\n", pr.Name, agentLimit(pr.MaxAgents))
	}
	c.reload()
	return 0
}

// removeProvider removes a provider and its key.
func (c *CLI) removeProvider(args []string) int {
	cfg, rest, code := c.providerFlags("remove-provider", "NAME", args, 1, nil, nil)
	if code != 0 {
		return code
	}
	j, err := providerIndex(cfg, rest[0])
	if err != nil {
		return c.fail(err)
	}
	cfg.Providers = slices.Delete(slices.Clone(cfg.Providers), j, j+1)
	if err := cfg.Save(c.Paths.File); err != nil {
		return c.fail(err)
	}
	if err := c.Secrets.Delete(secrets.Runner, secrets.ProviderKey(rest[0])); err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.Stdout, "Removed %s\n", rest[0])
	c.reload()
	return 0
}
