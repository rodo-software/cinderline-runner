package cli

import (
	"fmt"

	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// rename sets a project's key to the one its instance gives it now, asked
// with its stored runner token. Everything local is named by its ID, which
// stays, so nothing is moved or copied (CLR-191). A command, not an update
// at each poll: Willem wants a rename on this machine to be done by hand.
func (c *CLI) rename(args []string) int {
	cfg, i, _, code := c.repoFlags("rename", projectUsage, args, 0)
	if code != 0 {
		return code
	}
	p := &cfg.Projects[i]
	token, _, err := c.Secrets.Get(*p, secrets.RunnerToken)
	if err != nil {
		return c.fail(fmt.Errorf("%s has no runner token to ask its instance with: %w", p.Key, err))
	}
	proj, err := c.tokenProject(*p, token)
	if err != nil {
		return c.fail(fmt.Errorf("%s: %w", p.Key, err))
	}
	if proj.WorkspaceSlug() != p.Workspace {
		return c.fail(fmt.Errorf("%s's runner token is for workspace %s, not %s", p.Key, proj.WorkspaceSlug(), p.Workspace))
	}
	if proj.Key == p.Key {
		fmt.Fprintf(c.Stdout, "%s is already the key of this project at %s\n", p.Key, where(*p))
		return 0
	}
	old := p.Key
	p.LocalID = p.ID() // Load pinned it already; it must never follow the key
	p.Key = proj.Key
	if err := cfg.Save(c.Paths.File); err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.Stdout, "Renamed %s to %s at %s; its secrets, volumes and sessions keep their names\n", old, p.Key, where(*p))
	c.reload()
	return 0
}
