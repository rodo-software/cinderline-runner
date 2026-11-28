package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/manage"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

// rename sets a project's key to the one its instance gives it now, asked
// with its stored runner token, and its ID with it: a remove and an add in
// one, that keeps the config entry and the secrets (decision
// 2026-11-17-rename-moves-local-names). The Docker volumes cannot be renamed
// and are removed, so it refuses while they may hold work the forge has not.
// A command, not an update at each poll: Willem wants a rename on this
// machine to be done by hand.
func (c *CLI) rename(args []string) int {
	cfg, i, _, code := c.repoFlags("rename", projectUsage, args, 0)
	if code != 0 {
		return code
	}
	q := cfg.Projects[i]
	status := func() (localapi.Status, error) {
		st, err := (localapi.Client{Socket: c.Paths.Socket}).Status(context.Background())
		if errors.Is(err, localapi.ErrNotRunning) {
			return localapi.Status{}, nil
		}
		return st, err
	}
	old, p, err := manage.RenameProject(context.Background(), c.Paths, c.Secrets, c.exec, c.Version, status, manage.Project{Key: q.Key, URL: q.URL, Workspace: q.Workspace}, "", c.warn)
	if err != nil {
		return c.fail(err)
	}
	if p.ID() == old.ID() {
		fmt.Fprintf(c.Stdout, "%s is already the key of this project at %s\n", p.Key, manage.Where(p))
		return 0
	}
	c.dropLocal(old)
	fmt.Fprintf(c.Stdout, "Renamed %s to %s at %s. Its secrets moved with it. Its Docker volumes are removed: the clones come back with the next run, kept sessions are gone, and a client signed in with `%s login` must sign in again.\n",
		old.ID(), p.ID(), manage.Where(p), name.Name)
	return 0
}
