package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// modelKeys are the model credentials add asks for, in its order, and key set
// stores (decision 2026-11-10-model-keys). The kind is secrets.ClientKeys[client].
var modelKeys = []struct{ client, label string }{
	{"claude", "Anthropic API key or `claude setup-token` token"},
	{"opencode", "OpenRouter key"},
	{"codex", "OpenAI API key"},
	{"gemini", "Gemini API key"},
}

// key sets or replaces one model credential of a project, read as one line
// from stdin in plain text like add's answers. The key is never printed.
func (c *CLI) key(args []string) int {
	f := c.flags("key", "set CLIENT --project KEY [-url URL]")
	project := f.String("project", "", "project key")
	instance := f.String("url", "", "URL of the instance, when several projects share the key")
	if len(args) < 2 || args[0] != "set" {
		f.Usage()
		return 2
	}
	if err := f.Parse(args[2:]); err != nil || *project == "" || f.NArg() > 0 {
		if err == nil {
			f.Usage()
		}
		return 2
	}
	label := ""
	for _, m := range modelKeys {
		if m.client == args[1] {
			label = m.label
		}
	}
	if label == "" {
		return c.fail(fmt.Errorf("unknown client %q; use claude, opencode, codex or gemini", args[1]))
	}
	cfg, err := c.loadConfig()
	if err != nil {
		return c.fail(err)
	}
	p, err := findProject(cfg, *project, *instance)
	if err != nil {
		return c.fail(err)
	}
	fmt.Fprint(c.Stdout, label+": ")
	in := bufio.NewScanner(c.Stdin)
	if !in.Scan() {
		if err := in.Err(); err != nil {
			return c.fail(err)
		}
		return c.fail(errors.New("input ended before the key; nothing was stored"))
	}
	v := strings.TrimSpace(in.Text())
	if v == "" {
		return c.fail(errors.New("empty key; nothing was stored"))
	}
	if err := c.storeKey(p, args[1], label, v); err != nil {
		return c.fail(err)
	}
	if err := (localapi.Client{Socket: c.Paths.Socket}).Reload(context.Background()); err != nil && !errors.Is(err, localapi.ErrNotRunning) {
		fmt.Fprintf(c.Stdout, "Warning: daemon reload failed: %v.\n", err)
	}
	return 0
}

// storeKey stores client's model credential v for p and says where, never what.
func (c *CLI) storeKey(p config.Project, client, label, v string) error {
	store, err := c.Secrets.Set(p, secrets.ClientKeys[client], v)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Stdout, "Stored %s for %s in %s\n", label, client, c.storeName(store))
	return nil
}
