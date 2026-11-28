package manage

import (
	"errors"
	"fmt"
	"slices"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// findProvider is the index of cfg's provider name, or a refusal.
func findProvider(cfg *config.Config, name string) (int, error) {
	j := slices.IndexFunc(cfg.Providers, func(q config.Provider) bool { return q.Name == name })
	if j < 0 {
		return j, Refused{fmt.Errorf("this runner has no provider %s", name)}
	}
	return j, nil
}

// CheckProvider refuses p as a new provider of cfg: an invalid one, or a
// name cfg already has. The rest, such as the web page's port (CLR-273),
// is the config's Validate when it is saved.
func CheckProvider(cfg *config.Config, p config.Provider) error {
	if err := p.Validate(); err != nil {
		return Refused{err}
	}
	if _, err := findProvider(cfg, p.Name); err == nil {
		return Refused{fmt.Errorf("this runner already has a provider %s; remove it first", p.Name)}
	}
	return nil
}

// AddProvider adds p, a model provider for every project's OpenCode agents
// (decision 2026-11-17-local-model-providers), after CheckProvider, and
// stores key in sec, or deletes one left under its name when key is empty.
// It returns where the key went, "" for none. No error holds the key.
func AddProvider(paths config.Paths, sec *secrets.Secrets, p config.Provider, key string) (secrets.Store, error) {
	cfg, err := Load(paths)
	if err != nil {
		return "", err
	}
	if err := CheckProvider(cfg, p); err != nil {
		return "", err
	}
	// The config first, so a failed save stores no secret.
	old := cfg.Providers
	cfg.Providers = append(slices.Clone(old), p)
	if err := save(paths, cfg); err != nil {
		return "", err
	}
	var store secrets.Store
	kind := secrets.ProviderKey(p.Name)
	if key == "" {
		// A key left under this name must not go to the new URL.
		err = sec.Delete(secrets.Runner, kind)
	} else {
		store, err = sec.Set(secrets.Runner, kind, key)
	}
	if err != nil {
		cfg.Providers = old
		if serr := cfg.Save(paths.File); serr != nil {
			return "", fmt.Errorf("%w; restoring %s: %v", err, paths.File, serr)
		}
		return "", err
	}
	return store, nil
}

// SetProvider sets what is not nil of the provider name's free, max_agents
// and lookahead; its key stays. It says whether that changed the config:
// when not, it writes nothing, so the caller need not reload.
func SetProvider(paths config.Paths, name string, free *bool, maxAgents, lookahead *int) (bool, error) {
	if free == nil && maxAgents == nil && lookahead == nil {
		return false, Refused{errors.New("nothing to set: give free, max_agents or lookahead")}
	}
	cfg, err := Load(paths)
	if err != nil {
		return false, err
	}
	j, err := findProvider(cfg, name)
	if err != nil {
		return false, err
	}
	old, pr := cfg.Providers[j], &cfg.Providers[j]
	if free != nil {
		pr.Free = *free
	}
	if maxAgents != nil {
		pr.MaxAgents = *maxAgents
	}
	if lookahead != nil {
		pr.Lookahead = *lookahead
	}
	if err := pr.Validate(); err != nil {
		return false, Refused{err}
	}
	if *pr == old {
		return false, nil
	}
	if err := save(paths, cfg); err != nil {
		return false, err
	}
	return true, nil
}

// RemoveProvider removes the provider name, then its key from sec.
func RemoveProvider(paths config.Paths, sec *secrets.Secrets, name string) error {
	cfg, err := Load(paths)
	if err != nil {
		return err
	}
	j, err := findProvider(cfg, name)
	if err != nil {
		return err
	}
	cfg.Providers = slices.Delete(slices.Clone(cfg.Providers), j, j+1)
	if err := save(paths, cfg); err != nil {
		return err
	}
	return sec.Delete(secrets.Runner, secrets.ProviderKey(name))
}
