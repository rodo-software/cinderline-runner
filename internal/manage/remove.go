package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/run"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// RemoveProject deletes the project which names from config.json, then its
// secrets from sec, and returns it. A secret that is not deleted goes to
// warn: the project is removed by then. What its ID names outside the
// config and the keychain is DropLocal's, once its loop has ended.
func RemoveProject(paths config.Paths, sec *secrets.Secrets, which Project, warn func(error)) (config.Project, error) {
	cfg, err := Load(paths)
	if err != nil {
		return config.Project{}, err
	}
	p, err := FindByKey(cfg, which.Key, which.URL, which.Workspace)
	if err != nil {
		return config.Project{}, err
	}
	// The config first, so a failed save keeps the project's tokens (CLR-233).
	cfg.Projects = slices.DeleteFunc(cfg.Projects, func(q config.Project) bool {
		return q.URL == p.URL && q.Workspace == p.Workspace && q.Key == p.Key
	})
	if err := cfg.Save(paths.File); err != nil {
		return config.Project{}, err
	}
	for _, k := range secrets.Kinds(p) {
		if err := sec.Delete(p, k); err != nil {
			warn(err)
		}
	}
	return p, nil
}

// RenameProject sets the key of the project which names to the one its
// instance gives it now, asked with its stored runner token, and its ID
// with it: a remove and an add in one, that keeps the config entry and the
// secrets (decision 2026-11-17-rename-moves-local-names). newKey, when not
// empty, must be that key. It refuses while Busy, with the daemon's status
// that status answers once the instance has. It copies the secrets to the
// new ID, saves, then deletes the old ones; one not deleted goes to warn.
// It returns the project before and after: with one ID it had the key
// already, and nothing changed. The old ID's Docker volumes, notes and
// outbox are DropLocal's, once its loop has ended.
func RenameProject(ctx context.Context, paths config.Paths, sec *secrets.Secrets, x workspace.Exec, version string, status func() (localapi.Status, error), which Project, newKey string, warn func(error)) (old, p config.Project, err error) {
	cfg, err := Load(paths)
	if err != nil {
		return old, p, err
	}
	old, err = FindByKey(cfg, which.Key, which.URL, which.Workspace)
	if err != nil {
		return old, p, err
	}
	i := slices.IndexFunc(cfg.Projects, func(q config.Project) bool {
		return q.URL == old.URL && q.Workspace == old.Workspace && q.Key == old.Key
	})
	token, _, err := sec.Get(old, secrets.RunnerToken)
	if err != nil {
		return old, p, Refused{fmt.Errorf("%s has no runner token to ask its instance with: %w", old.Key, err)}
	}
	proj, err := TokenProject(ctx, old, token, version)
	if err != nil {
		return old, p, fmt.Errorf("%s: %w", old.Key, err)
	}
	if proj.WorkspaceSlug() != old.Workspace {
		return old, p, Refused{fmt.Errorf("%s's runner token is for workspace %s, not %s", old.Key, proj.WorkspaceSlug(), old.Workspace)}
	}
	if newKey != "" && newKey != proj.Key {
		return old, p, Refused{fmt.Errorf("the instance gives %s the key %s, not %s", old.Key, proj.Key, newKey)}
	}
	p = old
	p.Key = proj.Key
	p.LocalID = p.Label()
	// An entry renamed by 0.4.1 has the new key and still the old ID.
	if p.ID() == old.ID() {
		return old, p, nil
	}
	for j, q := range cfg.Projects {
		if j != i && q.ID() == p.ID() {
			return old, p, Refused{fmt.Errorf("%s at %s already names its Docker volumes and secrets %s; two projects with one name would share them", q.Key, q.URL, p.ID())}
		}
	}
	st, err := status()
	if err != nil {
		return old, p, err
	}
	if err := Busy(ctx, paths, x, st, old); err != nil {
		return old, p, err
	}

	// Copy, save, then delete: a failure before the save leaves the old names whole.
	var moved []secrets.Kind
	undo := func(err error) (config.Project, config.Project, error) {
		for _, k := range moved {
			sec.Delete(p, k)
		}
		return old, p, err
	}
	for _, k := range secrets.Kinds(old) {
		v, _, err := sec.Get(old, k)
		if errors.Is(err, secrets.ErrNotFound) {
			continue
		}
		if err == nil {
			moved = append(moved, k) // before Set: it may have stored it and still fail
			_, err = sec.Set(p, k, v)
		}
		if err != nil {
			return undo(err)
		}
	}
	cfg.Projects[i] = p
	if err := cfg.Save(paths.File); err != nil {
		return undo(err)
	}
	for _, k := range moved {
		if err := sec.Delete(old, k); err != nil {
			warn(err)
		}
	}
	return old, p, nil
}

// Busy says why p's Docker volumes cannot be removed now: they may hold
// work that is not on the forge, or the app is owed a report about it. st
// is the daemon's status, the zero one when no daemon runs.
func Busy(ctx context.Context, paths config.Paths, x workspace.Exec, st localapi.Status, p config.Project) error {
	for _, s := range st.Projects {
		switch {
		case s.Project == p.Label() && len(s.Issues) > 0:
			return Refused{fmt.Errorf("%s is running %s; rename it when the run has ended", p.Key, strings.Join(s.Issues, ", "))}
		case s.Project == p.Label() && s.Manager != "":
			return Refused{fmt.Errorf("%s is running the Project Manager on %s; rename it when the run has ended", p.Key, s.Manager)}
		}
	}
	if _, err := os.Stat(run.OutboxFile(paths.State, p)); err == nil {
		return Refused{fmt.Errorf("%s has reports its instance has not received yet; let the runner deliver them, then rename it", p.Key)}
	}
	dir := run.PausedDir(paths.State, p)
	entries, _ := os.ReadDir(dir) // no folder: no notes
	var paused []string
	for _, e := range entries {
		var n run.Note
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && json.Unmarshal(b, &n) == nil && n.Origin == "paused" {
			paused = append(paused, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	if len(paused) > 0 {
		return Refused{fmt.Errorf("%s has paused runs, whose work may not be pushed: %s; rename it when they have ended", p.Key, strings.Join(paused, ", "))}
	}
	// A clone without a kept note, such as one a plain stop cut, may hold
	// work only the runner's tidy pushes (CLR-231).
	dirs, err := (&workspace.Docker{Project: p.ID(), Exec: x}).List(ctx)
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
		return Refused{fmt.Errorf("%s has clones whose work may not be pushed: %s; start the runner (`%s run` or its service) so its tidy pushes them, then rename it", p.Key, strings.Join(unpushed, ", "), name.Name)}
	}
	return nil
}

// DropLocal removes what p's ID names outside config.json and the
// keychain: its Docker containers, networks and volumes (the clones and
// the clients' sign-ins, decision 2026-11-11-audit-fixes) unless a project
// in config.json still has the ID, its session notes and its outbox. Its
// runs hold them: call it once p's loop has ended. It only warns.
func DropLocal(ctx context.Context, paths config.Paths, x workspace.Exec, p config.Project, warn func(error)) {
	// Docker names carry only the ID, so a namesake on another instance still uses them.
	if cfg, err := Load(paths); err != nil {
		warn(err)
	} else if !slices.ContainsFunc(cfg.Projects, func(q config.Project) bool { return q.ID() == p.ID() }) {
		label := "label=" + workspace.ProjectLabel(p.ID())
		if err := RemoveLabelled(ctx, x, label); err != nil {
			warn(fmt.Errorf("%s's Docker volumes, with its clones and sign-ins, are not all removed; list them with `docker volume ls --filter %s`: %w", p.Key, label, err))
		}
	}
	if err := os.RemoveAll(run.PausedDir(paths.State, p)); err != nil {
		warn(err)
	}
	if err := os.Remove(run.OutboxFile(paths.State, p)); err != nil && !errors.Is(err, os.ErrNotExist) {
		warn(err)
	}
}

// RemoveLabelled removes the containers, networks and volumes with label,
// which names one project or, without a value, all of them.
func RemoveLabelled(ctx context.Context, x workspace.Exec, label string) error {
	for _, step := range [][2][]string{
		{{"ps", "--all", "--quiet", "--filter", label}, {"rm", "--force", "--volumes"}},
		{{"network", "ls", "--quiet", "--filter", label}, {"network", "rm"}},
		{{"volume", "ls", "--quiet", "--filter", label}, {"volume", "rm", "--force"}},
	} {
		if err := DockerRemove(ctx, x, step[0], step[1], nil); err != nil {
			return err
		}
	}
	return nil
}

// DockerRemove lists what list finds and removes it, only what keep says
// when it is set; a list that names nothing runs no rm argument.
func DockerRemove(ctx context.Context, x workspace.Exec, list, rm []string, keep func(string) bool) error {
	out, err := x(ctx, nil, append([]string{"docker"}, list...)...)
	if err != nil {
		return err
	}
	ids := strings.Fields(out)
	if keep != nil {
		ids = slices.DeleteFunc(ids, func(id string) bool { return !keep(id) })
	}
	slices.Sort(ids)
	ids = slices.Compact(ids) // an image with two tags is listed twice
	if len(ids) == 0 {
		return nil
	}
	_, err = x(ctx, nil, append(append([]string{"docker"}, rm...), ids...)...)
	return err
}
