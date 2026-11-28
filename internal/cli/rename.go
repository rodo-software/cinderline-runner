package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/manage"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/run"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
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
	old := cfg.Projects[i]
	token, _, err := c.Secrets.Get(old, secrets.RunnerToken)
	if err != nil {
		return c.fail(fmt.Errorf("%s has no runner token to ask its instance with: %w", old.Key, err))
	}
	proj, err := manage.TokenProject(context.Background(), old, token, c.Version)
	if err != nil {
		return c.fail(fmt.Errorf("%s: %w", old.Key, err))
	}
	if proj.WorkspaceSlug() != old.Workspace {
		return c.fail(fmt.Errorf("%s's runner token is for workspace %s, not %s", old.Key, proj.WorkspaceSlug(), old.Workspace))
	}
	p := old
	p.Key = proj.Key
	p.LocalID = p.Label()
	// An entry renamed by 0.4.1 has the new key and still the old ID.
	if p.ID() == old.ID() {
		fmt.Fprintf(c.Stdout, "%s is already the key of this project at %s\n", p.Key, manage.Where(p))
		return 0
	}
	for j, q := range cfg.Projects {
		if j != i && q.ID() == p.ID() {
			return c.fail(fmt.Errorf("%s at %s already names its Docker volumes and secrets %s; two projects with one name would share them", q.Key, q.URL, p.ID()))
		}
	}
	if err := c.busy(old); err != nil {
		return c.fail(err)
	}

	// Copy, save, then delete: a failure before the save leaves the old names whole.
	var moved []secrets.Kind
	undo := func(err error) int {
		for _, k := range moved {
			c.Secrets.Delete(p, k)
		}
		return c.fail(err)
	}
	for _, k := range secrets.Kinds(old) {
		v, _, err := c.Secrets.Get(old, k)
		if errors.Is(err, secrets.ErrNotFound) {
			continue
		}
		if err == nil {
			moved = append(moved, k) // before Set: it may have stored it and still fail
			_, err = c.Secrets.Set(p, k, v)
		}
		if err != nil {
			return undo(err)
		}
	}
	cfg.Projects[i] = p
	if err := cfg.Save(c.Paths.File); err != nil {
		return undo(err)
	}
	for _, k := range moved {
		if err := c.Secrets.Delete(old, k); err != nil {
			fmt.Fprintln(c.Stderr, "warning:", err)
		}
	}
	c.dropLocal(cfg, old)
	fmt.Fprintf(c.Stdout, "Renamed %s to %s at %s. Its secrets moved with it. Its Docker volumes are removed: the clones come back with the next run, kept sessions are gone, and a client signed in with `%s login` must sign in again.\n",
		old.ID(), p.ID(), manage.Where(p), name.Name)
	return 0
}

// busy says why p's Docker volumes cannot be removed now: they may hold
// work that is not on the forge, or the app is owed a report about it.
func (c *CLI) busy(p config.Project) error {
	st, err := (localapi.Client{Socket: c.Paths.Socket}).Status(context.Background())
	if err != nil && !errors.Is(err, localapi.ErrNotRunning) {
		return err
	}
	for _, s := range st.Projects {
		switch {
		case s.Project == p.Label() && len(s.Issues) > 0:
			return fmt.Errorf("%s is running %s; rename it when the run has ended", p.Key, strings.Join(s.Issues, ", "))
		case s.Project == p.Label() && s.Manager != "":
			return fmt.Errorf("%s is running the Project Manager on %s; rename it when the run has ended", p.Key, s.Manager)
		}
	}
	if _, err := os.Stat(run.OutboxFile(c.Paths.State, p)); err == nil {
		return fmt.Errorf("%s has reports its instance has not received yet; let the runner deliver them, then rename it", p.Key)
	}
	dir := run.PausedDir(c.Paths.State, p)
	entries, _ := os.ReadDir(dir) // no folder: no notes
	var paused []string
	for _, e := range entries {
		var n run.Note
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && json.Unmarshal(b, &n) == nil && n.Origin == "paused" {
			paused = append(paused, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	if len(paused) > 0 {
		return fmt.Errorf("%s has paused runs, whose work may not be pushed: %s; rename it when they have ended", p.Key, strings.Join(paused, ", "))
	}
	// A clone without a kept note, such as one a plain stop cut, may hold
	// work only the runner's tidy pushes (CLR-231).
	dirs, err := (&workspace.Docker{Project: p.ID(), Exec: c.exec}).List(context.Background())
	if err != nil {
		return fmt.Errorf("%s's clones could not be listed to check for unpushed work: %w", p.Key, err)
	}
	var unpushed []string
	for _, d := range dirs {
		ref := path.Base(d)
		var n run.Note
		if b, err := os.ReadFile(filepath.Join(dir, ref+".json")); err != nil || json.Unmarshal(b, &n) != nil || n.Origin != "kept" {
			unpushed = append(unpushed, ref)
		}
	}
	if len(unpushed) > 0 {
		return fmt.Errorf("%s has clones whose work may not be pushed: %s; start the runner (`%s run` or its service) so its tidy pushes them, then rename it", p.Key, strings.Join(unpushed, ", "), name.Name)
	}
	return nil
}
