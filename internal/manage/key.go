package manage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// KeyInfo is one secret a project may hold: its name, as `key` takes it,
// and whether and where it is stored, never its value.
type KeyInfo struct {
	Kind  string        `json:"kind"`
	Set   bool          `json:"set"`
	Store secrets.Store `json:"-"` // for the CLI's list only
}

// keyName is k as `key` names it: a client for a model credential, else the kind.
func keyName(k secrets.Kind) string {
	for c, ck := range secrets.ClientKeys {
		if ck == k {
			return c
		}
	}
	return string(k)
}

// KeyKind is the kind of secret `key` name sets: runner-token, git-token
// or a client's model key (decision 2026-11-10-model-keys).
func KeyKind(name string) (secrets.Kind, error) {
	if k := secrets.Kind(name); k == secrets.RunnerToken || k == secrets.GitToken {
		return k, nil
	}
	if k, ok := secrets.ClientKeys[name]; ok {
		return k, nil
	}
	return "", Refused{fmt.Errorf("unknown key %q; use %s, %s, claude, opencode, codex or gemini", name, secrets.RunnerToken, secrets.GitToken)}
}

// FindProject is config.json's project whose ID is id.
func FindProject(paths config.Paths, id string) (config.Project, error) {
	cfg, err := Load(paths)
	if err != nil {
		return config.Project{}, err
	}
	p, _, err := findProject(cfg, id)
	return p, err
}

// keyOwner is who holds the secret kind: p, or the runner for
// provider:NAME, which takes the zero project (decision
// 2026-11-17-local-model-providers).
func keyOwner(paths config.Paths, p config.Project, kind string) (config.Project, secrets.Kind, error) {
	pr, ok := strings.CutPrefix(kind, "provider:")
	if !ok {
		if p.Key == "" {
			return config.Project{}, "", Refused{errors.New("the key " + kind + " needs a project")}
		}
		k, err := KeyKind(kind)
		return p, k, err
	}
	if p.Key != "" {
		return config.Project{}, "", Refused{fmt.Errorf("the key of a provider belongs to no project, not %s", p.Key)}
	}
	cfg, err := Load(paths)
	if err != nil {
		return config.Project{}, "", err
	}
	if _, err := findProvider(cfg, pr); err != nil {
		return config.Project{}, "", err
	}
	return secrets.Runner, secrets.ProviderKey(pr), nil
}

// ListKeys says which of p's secrets are set: its runner and git token,
// each client's model key and each extra repository's token.
func ListKeys(sec *secrets.Secrets, p config.Project) ([]KeyInfo, error) {
	var out []KeyInfo
	for _, k := range secrets.Kinds(p) {
		_, store, err := sec.Get(p, k)
		if err != nil && !errors.Is(err, secrets.ErrNotFound) {
			return nil, err
		}
		out = append(out, KeyInfo{keyName(k), err == nil, store})
	}
	return out, nil
}

// SetKey stores value as p's secret kind, or as provider:NAME's key with
// the zero p, and returns where it went. A runner token is first checked
// against p's instance, with this build's version; nothing else is
// checked. A model key wins over a login made before; a later login
// deletes it (decision 2026-11-19-login-replaces-key). No error holds the value.
func SetKey(ctx context.Context, paths config.Paths, sec *secrets.Secrets, version string, p config.Project, kind, value string) (secrets.Store, error) {
	p, k, err := keyOwner(paths, p, kind)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", Refused{errors.New("empty value; nothing was stored")}
	}
	if k == secrets.RunnerToken {
		proj, err := TokenProject(ctx, p, value, version)
		if err != nil {
			return "", err
		}
		if proj.Key != p.Key || proj.WorkspaceSlug() != p.Workspace {
			return "", Refused{fmt.Errorf("this runner token is for %s in workspace %s, not %s in %s", proj.Key, proj.WorkspaceSlug(), p.Key, p.Workspace)}
		}
	}
	return sec.Set(p, k, value)
}

// DeleteKey deletes p's secret kind, or provider:NAME's key with the zero
// p; one that is not stored is no error.
func DeleteKey(paths config.Paths, sec *secrets.Secrets, p config.Project, kind string) error {
	p, k, err := keyOwner(paths, p, kind)
	if err != nil {
		return err
	}
	return sec.Delete(p, k)
}

// TokenProject asks p's instance which project runner token v is for. A
// token the instance refuses is Refused; no error holds v.
func TokenProject(ctx context.Context, p config.Project, v, version string) (cinderline.Project, error) {
	proj, err := cinderline.New(p.URL, v, version).Project(ctx)
	var apiErr *cinderline.APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
		return proj, Refused{errors.New("the instance rejected this runner token")}
	}
	if err != nil {
		msg := err.Error()
		if v != "" {
			msg = strings.ReplaceAll(msg, v, "[redacted runner token]") // an app could echo it
		}
		return proj, fmt.Errorf("%s could not be reached: %s", p.URL, msg)
	}
	return proj, nil
}
