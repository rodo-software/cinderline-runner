package manage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/redact"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// NewProject is what `add` asks for a project. Its key and workspace are
// not asked: they come from the instance.
type NewProject struct {
	URL         string `json:"url"`
	RunnerToken string `json:"runner_token"`
	// "" is the repository the instance names, as an empty answer in add, so
	// the page adds in one request that sends the token once (Willem, CLR-301).
	Repository string `json:"repository"`
	GitToken   string `json:"git_token"`
}

// ErrTokenRejected is the instance refusing the runner token; add asks it again.
var ErrTokenRejected = errors.New("the instance rejected this runner token")

// CheckInstanceURL refuses a URL the runner token may not be sent to
// (decision 2026-11-19-https-instance) and returns it without a trailing slash.
func CheckInstanceURL(v string) (string, error) {
	if err := config.CheckInstanceURL(v); err != nil {
		return "", Refused{fmt.Errorf("%q %w", v, err)}
	}
	return strings.TrimRight(v, "/"), nil
}

// Instance asks the instance at url, checked first, which project token is
// for. A refused token is Refused with ErrTokenRejected, any other failure
// Refused too: add asks the URL again. No error holds the token.
func Instance(ctx context.Context, url, token, version string) (cinderline.Project, error) {
	url, err := CheckInstanceURL(url)
	if err != nil {
		return cinderline.Project{}, err
	}
	if token == "" {
		return cinderline.Project{}, Refused{errors.New("a runner token is needed")}
	}
	proj, err := cinderline.New(url, token, version).Project(ctx)
	var apiErr *cinderline.APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
		return proj, Refused{ErrTokenRejected}
	}
	if err != nil {
		msg := strings.ReplaceAll(err.Error(), token, "[redacted runner token]") // an app could echo it
		return proj, Refused{fmt.Errorf("%s could not be reached as a %s instance: %s", url, name.Name, msg)}
	}
	return proj, nil
}

// Entry is the config entry for the instance's project proj at url, or
// why cfg cannot take it.
func Entry(cfg *config.Config, url string, proj cinderline.Project) (config.Project, error) {
	p := config.Project{URL: url, Key: proj.Key, Workspace: proj.WorkspaceSlug()}
	p.LocalID = p.Label() // stays until `rename`, whatever the key becomes in the app
	for _, q := range cfg.Projects {
		if q.URL == p.URL && q.Key == p.Key && q.Workspace == p.Workspace {
			return p, Refused{fmt.Errorf("%s at %s is already added, in workspace %s; run `%s remove -url %s -workspace %s %s` first", p.Key, p.URL, q.Workspace, name.Name, q.URL, q.Workspace, q.Key)}
		}
		// Docker volumes are named by the ID (decision 2026-11-11-audit-fixes, CSR-97).
		if q.ID() == p.ID() {
			return p, Refused{fmt.Errorf("%s at %s already names its Docker volumes and secrets %s, perhaps its key before a rename; two projects with one name would share them", q.Key, q.URL, p.ID())}
		}
	}
	return p, nil
}

// CheckRepository is the repository v names for proj: "" is the one proj
// names, and another one is refused.
func CheckRepository(proj cinderline.Project, v string) (string, error) {
	if v == "" {
		v = proj.RepositoryURL
	}
	if err := config.CheckRepositoryURL(v); err != nil {
		return "", Refused{err}
	}
	if proj.RepositoryURL != "" && !config.SameRepository(v, proj.RepositoryURL) {
		return "", Refused{fmt.Errorf("%s is not the project's repository, %s", v, proj.RepositoryURL)}
	}
	return v, nil
}

// AddProject adds the project in, with the checks add makes in its order:
// the URL before the runner token is sent, the token against the
// instance, that the project is not added yet, the repository, the git
// token (stored, not checked against the forge). It stores both tokens in
// sec and returns the project and where the runner and the git token went.
// No error holds a token.
func AddProject(ctx context.Context, paths config.Paths, sec *secrets.Secrets, version string, in NewProject) (config.Project, [2]secrets.Store, error) {
	var stores [2]secrets.Store
	cfg, err := Load(paths)
	if err != nil {
		return config.Project{}, stores, err
	}
	proj, err := Instance(ctx, in.URL, in.RunnerToken, version)
	if err != nil {
		return config.Project{}, stores, err
	}
	p, err := Entry(cfg, strings.TrimRight(in.URL, "/"), proj)
	if err != nil {
		return config.Project{}, stores, err
	}
	if p.Repository, err = CheckRepository(proj, in.Repository); err != nil {
		return config.Project{}, stores, err
	}
	if in.GitToken == "" {
		return config.Project{}, stores, Refused{errors.New("a git token is needed")}
	}
	redact.Add("runner token", in.RunnerToken) // so the log never shows them either
	redact.Add("git token", in.GitToken)

	// The config first, so a failed save stores no secrets.
	others := cfg.Projects
	cfg.Projects = append(slices.Clone(others), p)
	if err := save(paths, cfg); err != nil {
		return config.Project{}, stores, err
	}
	for i, s := range []struct {
		kind  secrets.Kind
		value string
	}{{secrets.RunnerToken, in.RunnerToken}, {secrets.GitToken, in.GitToken}} {
		if stores[i], err = sec.Set(p, s.kind, s.value); err != nil {
			return config.Project{}, stores, undoAdd(paths, sec, cfg, others, p, err)
		}
	}
	return p, stores, nil
}

// undoAdd removes p and its tokens again after err, a failed Set, and
// returns err with what failed in the undo.
func undoAdd(paths config.Paths, sec *secrets.Secrets, cfg *config.Config, others []config.Project, p config.Project, err error) error {
	for _, k := range []secrets.Kind{secrets.RunnerToken, secrets.GitToken} {
		if derr := sec.Delete(p, k); derr != nil {
			err = fmt.Errorf("%w; deleting the %s again: %v", err, k, derr)
		}
	}
	cfg.Projects = others
	if serr := cfg.Save(paths.File); serr != nil {
		err = fmt.Errorf("%w; restoring %s: %v", err, paths.File, serr)
	}
	return err
}
