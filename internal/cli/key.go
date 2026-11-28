package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/manage"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// modelKeys are the model credentials add asks for, in its order, and key
// stores (decision 2026-11-10-model-keys). The kind is secrets.ClientKeys[client].
var modelKeys = []struct{ client, label string }{
	{"claude", "Anthropic API key or `claude setup-token` token"},
	{"opencode", "OpenRouter key"},
	{"codex", "OpenAI API key"},
	{"gemini", "Gemini API key"},
}

const keyUsage = "list|set NAME|delete NAME " + projectUsage + " | set|delete provider:NAME"

// key lists which secrets a project has, or sets or deletes one: its runner
// token, its git token or a client's model key. A new value is read as one
// line from stdin, without echo on a terminal, and never printed. Only a
// runner token is checked, against the instance, as add does.
func (c *CLI) key(args []string) int {
	if len(args) > 0 && args[0] == "list" {
		return c.keyList(args[1:])
	}
	if len(args) < 2 || (args[0] != "set" && args[0] != "delete") {
		c.flags("key", keyUsage).Usage()
		return 2
	}
	if pr, ok := strings.CutPrefix(args[1], "provider:"); ok {
		return c.providerKey(args[0], pr, args[2:])
	}
	cfg, i, _, code := c.repoFlags("key", keyUsage, args[2:], 0)
	if code != 0 {
		return code
	}
	p, n := cfg.Projects[i], args[1]
	if _, err := manage.KeyKind(n); err != nil {
		return c.fail(err)
	}
	label := n
	for _, m := range modelKeys {
		if m.client == n {
			label = m.label + " for " + n
		}
	}
	if args[0] == "delete" {
		if err := manage.DeleteKey(c.Paths, c.Secrets, p, n); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.Stdout, "Deleted %s of %s\n", label, p.Key)
		c.reload()
		return 0
	}
	v, err := c.secretAsk(label + ": ")
	if err != nil {
		return c.fail(err)
	}
	store, err := manage.SetKey(context.Background(), c.Paths, c.Secrets, c.Version, p, n, v)
	if err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.Stdout, "Stored %s in %s\n", label, c.storeName(store))
	c.reload()
	return 0
}

// providerKey sets or deletes the runner-wide key of the runner's provider
// pr (decision 2026-11-17-local-model-providers); it takes no --project.
func (c *CLI) providerKey(verb, pr string, args []string) int {
	cfg, _, code := c.providerFlags("key", keyUsage, args, 0, nil, nil, nil)
	if code != 0 {
		return code
	}
	if _, err := providerIndex(cfg, pr); err != nil {
		return c.fail(err)
	}
	label := "the key of " + pr
	if verb == "delete" {
		if err := manage.DeleteKey(c.Paths, c.Secrets, config.Project{}, "provider:"+pr); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.Stdout, "Deleted %s\n", label)
		c.reload()
		return 0
	}
	v, err := c.secretAsk("API key for " + pr + ": ")
	if err != nil {
		return c.fail(err)
	}
	store, err := manage.SetKey(context.Background(), c.Paths, c.Secrets, c.Version, config.Project{}, "provider:"+pr, v)
	if err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.Stdout, "Stored %s in %s\n", label, c.storeName(store))
	c.reload()
	return 0
}

// keyList prints which of a project's secrets are set and where, never a value.
func (c *CLI) keyList(args []string) int {
	cfg, i, _, code := c.repoFlags("key", keyUsage, args, 0)
	if code != 0 {
		return code
	}
	ks, err := manage.ListKeys(c.Secrets, cfg.Projects[i])
	if err != nil {
		return c.fail(err)
	}
	for _, k := range ks {
		if k.Set {
			fmt.Fprintf(c.Stdout, "%-13s set, in %s\n", k.Kind, c.storeName(k.Store))
		} else {
			fmt.Fprintf(c.Stdout, "%-13s not set\n", k.Kind)
		}
	}
	return 0
}

// secretAsk prints prompt and reads one line from stdin; on a terminal the
// typed value is not echoed.
func (c *CLI) secretAsk(prompt string) (string, error) {
	f, ok := c.Stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return c.lineAsk()(prompt)
	}
	fmt.Fprint(c.Stdout, prompt)
	b, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(c.Stdout)
	return strings.TrimSpace(string(b)), err
}

// storeKey stores v as p's secret of kind and says where, never what.
func (c *CLI) storeKey(p config.Project, kind secrets.Kind, label, v string) error {
	store, err := c.Secrets.Set(p, kind, v)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Stdout, "Stored %s in %s\n", label, c.storeName(store))
	return nil
}
