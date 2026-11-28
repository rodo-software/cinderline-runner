package manage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/redact"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// findProject returns cfg's project whose ID is id, and its index; IDs are
// unique, as `add` refuses one that is taken.
func findProject(cfg *config.Config, id string) (config.Project, int, error) {
	i := slices.IndexFunc(cfg.Projects, func(p config.Project) bool { return p.ID() == id })
	if i < 0 {
		return config.Project{}, 0, Refused{fmt.Errorf("no project %s", id)}
	}
	return cfg.Projects[i], i, nil
}

// CheckRepo refuses r as a new extra repository of p, in the order
// add-repo asks: its URL, that it is not p's own or already added, then its ref.
func CheckRepo(p config.Project, r config.ExtraRepository) error {
	if err := (config.ExtraRepository{URL: r.URL}).Validate(); err != nil {
		return Refused{err}
	}
	if config.SameRepository(r.URL, p.Repository) {
		return Refused{fmt.Errorf("%s is the project's own repository", r.URL)}
	}
	if j := slices.IndexFunc(p.ExtraRepositories, func(q config.ExtraRepository) bool { return q.Name() == r.Name() }); j >= 0 {
		return Refused{fmt.Errorf("%s is already added as %s; remove it first", p.ExtraRepositories[j].URL, p.ExtraRepositories[j].Name())}
	}
	if err := r.Validate(); err != nil {
		return Refused{err}
	}
	return nil
}

// AddRepo adds r as an extra repository that the project with ID project's
// runs can read (CSR-160), after CheckRepo and a git ls-remote through x
// that token can read its ref, and stores token in sec. It returns where
// the token went. No error holds the token.
func AddRepo(ctx context.Context, log *slog.Logger, paths config.Paths, sec *secrets.Secrets, x workspace.Exec, project string, r config.ExtraRepository, token string) (secrets.Store, error) {
	cfg, err := Load(paths)
	if err != nil {
		return "", err
	}
	p, i, err := findProject(cfg, project)
	if err != nil {
		return "", err
	}
	if err := CheckRepo(p, r); err != nil {
		return "", err
	}
	if token == "" {
		return "", Refused{errors.New("a git token is needed")}
	}
	redact.Add("git token", token) // so the log never shows it either
	if err := workspace.LsRemote(ctx, x, log, p.ID(), r.URL, r.Ref, token); err != nil {
		msg := strings.ReplaceAll(err.Error(), token, "[redacted git token]") // also one shorter than redact.MinLen
		return "", Refused{fmt.Errorf("this token cannot read %s at %s: %s", r.URL, Ref(r.Ref), msg)}
	}

	// The config first, so a failed save stores no secret.
	cfg.Projects[i].ExtraRepositories = append(slices.Clone(p.ExtraRepositories), r)
	if err := save(paths, cfg); err != nil {
		return "", err
	}
	store, err := sec.Set(p, secrets.ExtraGitToken(r.URL), token)
	if err != nil {
		cfg.Projects[i] = p
		if serr := cfg.Save(paths.File); serr != nil {
			return "", fmt.Errorf("%w; restoring %s: %v", err, paths.File, serr)
		}
		return "", err
	}
	return store, nil
}

// RemoveRepo removes the extra repository named by its URL or name from
// the project with ID project, then its token from sec, and returns it.
// The next run deletes its copy.
func RemoveRepo(paths config.Paths, sec *secrets.Secrets, project, urlOrName string) (config.ExtraRepository, error) {
	none := config.ExtraRepository{}
	cfg, err := Load(paths)
	if err != nil {
		return none, err
	}
	p, i, err := findProject(cfg, project)
	if err != nil {
		return none, err
	}
	j := slices.IndexFunc(p.ExtraRepositories, func(r config.ExtraRepository) bool { return r.URL == urlOrName || r.Name() == urlOrName })
	if j < 0 {
		return none, Refused{fmt.Errorf("project %s has no extra repository %s", p.Key, urlOrName)}
	}
	r := p.ExtraRepositories[j]
	cfg.Projects[i].ExtraRepositories = slices.Delete(slices.Clone(p.ExtraRepositories), j, j+1)
	if err := save(paths, cfg); err != nil {
		return none, err
	}
	return r, sec.Delete(p, secrets.ExtraGitToken(r.URL))
}

// Ref is the ref, or what an empty one means.
func Ref(ref string) string {
	if ref == "" {
		return "the default branch"
	}
	return ref
}
