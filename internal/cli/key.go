package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
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
	kind, label := secrets.Kind(n), n
	if kind != secrets.RunnerToken && kind != secrets.GitToken {
		kind, label = secrets.ClientKeys[n], ""
		for _, m := range modelKeys {
			if m.client == n {
				label = m.label + " for " + n
			}
		}
		if label == "" {
			return c.fail(fmt.Errorf("unknown key %q; use %s, %s, claude, opencode, codex or gemini", n, secrets.RunnerToken, secrets.GitToken))
		}
	}
	if args[0] == "delete" {
		if err := c.Secrets.Delete(p, kind); err != nil {
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
	if v == "" {
		return c.fail(errors.New("empty value; nothing was stored"))
	}
	if kind == secrets.RunnerToken {
		if err := c.checkRunnerToken(p, v); err != nil {
			return c.fail(err)
		}
	}
	if err := c.storeKey(p, kind, label, v); err != nil {
		return c.fail(err)
	}
	c.reload()
	return 0
}

// providerKey sets or deletes the runner-wide key of the runner's provider
// pr (decision 2026-11-17-local-model-providers); it takes no --project.
func (c *CLI) providerKey(verb, pr string, args []string) int {
	cfg, _, code := c.providerFlags("key", keyUsage, args, 0)
	if code != 0 {
		return code
	}
	if _, err := providerIndex(cfg, pr); err != nil {
		return c.fail(err)
	}
	kind, label := secrets.ProviderKey(pr), "the key of "+pr
	if verb == "delete" {
		if err := c.Secrets.Delete(secrets.Runner, kind); err != nil {
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
	if v == "" {
		return c.fail(errors.New("empty value; nothing was stored"))
	}
	if err := c.storeKey(secrets.Runner, kind, label, v); err != nil {
		return c.fail(err)
	}
	c.reload()
	return 0
}

// keyList prints which of a project's secrets are set and where, never a value.
func (c *CLI) keyList(args []string) int {
	cfg, i, _, code := c.repoFlags("key", keyUsage, args, 0)
	if code != 0 {
		return code
	}
	p := cfg.Projects[i]
	names := []string{string(secrets.RunnerToken), string(secrets.GitToken)}
	kinds := []secrets.Kind{secrets.RunnerToken, secrets.GitToken}
	for _, m := range modelKeys {
		names, kinds = append(names, m.client), append(kinds, secrets.ClientKeys[m.client])
	}
	for _, r := range p.ExtraRepositories {
		k := secrets.ExtraGitToken(r.URL)
		names, kinds = append(names, string(k)), append(kinds, k)
	}
	for j, k := range kinds {
		_, store, err := c.Secrets.Get(p, k)
		switch {
		case err == nil:
			fmt.Fprintf(c.Stdout, "%-13s set, in %s\n", names[j], c.storeName(store))
		case errors.Is(err, secrets.ErrNotFound):
			fmt.Fprintf(c.Stdout, "%-13s not set\n", names[j])
		default:
			return c.fail(err)
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

// checkRunnerToken checks v against p's instance, as add does, and that it is p's.
func (c *CLI) checkRunnerToken(p config.Project, v string) error {
	proj, err := cinderline.New(p.URL, v, c.Version).Project(context.Background())
	var apiErr *cinderline.APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
		return errors.New("the instance rejected this runner token")
	}
	if err != nil {
		return fmt.Errorf("%s could not be reached: %w", p.URL, err)
	}
	if proj.Key != p.Key || proj.WorkspaceSlug() != p.Workspace {
		return fmt.Errorf("this runner token is for %s in workspace %s, not %s in %s", proj.Key, proj.WorkspaceSlug(), p.Key, p.Workspace)
	}
	return nil
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
