package cli

import (
	"errors"
	"fmt"
	"slices"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// providerFlags parses a provider command, which has no --project: a
// provider is the runner's, for every project (decision 2026-11-17-local-model-providers).
func (c *CLI) providerFlags(cmd, usage string, args []string, nargs int) (*config.Config, []string, int) {
	f := c.flags(cmd, usage)
	if !c.parsed(f, args, nargs) {
		return nil, nil, 2
	}
	cfg, err := c.loadConfig()
	if err != nil {
		return nil, nil, c.fail(err)
	}
	return cfg, f.Args(), 0
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
	cfg, rest, code := c.providerFlags("add-provider", "NAME URL", args, 2)
	if code != 0 {
		return code
	}
	pr := config.Provider{Name: rest[0], BaseURL: rest[1]}
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
	cfg, _, code := c.providerFlags("list-providers", "", args, 0)
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
		fmt.Fprintf(c.Stdout, "%s\t%s\t%s\n", pr.Name, pr.BaseURL, key)
	}
	return 0
}

// removeProvider removes a provider and its key.
func (c *CLI) removeProvider(args []string) int {
	cfg, rest, code := c.providerFlags("remove-provider", "NAME", args, 1)
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
