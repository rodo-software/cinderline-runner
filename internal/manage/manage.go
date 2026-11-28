// Package manage holds the operations on the runner's config and secrets
// that both the CLI and the local web page call: one write path (decision
// 2026-11-26-local-web-page, Config management). Each loads config.json,
// changes it, validates it and saves it; reloading the daemon is the
// caller's.
package manage

import (
	"errors"
	"io/fs"

	"github.com/rodo-software/cinderline-runner/internal/config"
)

// Refused is a value the config cannot take: the request was wrong, not
// the runner.
type Refused struct{ Err error }

func (r Refused) Error() string { return r.Err.Error() }
func (r Refused) Unwrap() error { return r.Err }

// Load returns config.json, or an empty config when it does not exist yet.
func Load(paths config.Paths) (*config.Config, error) {
	cfg, err := config.Load(paths.File)
	if errors.Is(err, fs.ErrNotExist) {
		return &config.Config{}, nil
	}
	return cfg, err
}

// save writes cfg to config.json, or refuses it when it is not valid.
func save(paths config.Paths, cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return Refused{err}
	}
	return cfg.Save(paths.File)
}

// SetStrictHosts turns strict host blocking on or off for the whole runner:
// when on, Cinderline cannot open a host for an issue's runs (decision
// 2026-11-17-hosts-from-cinderline).
func SetStrictHosts(paths config.Paths, on bool) error {
	cfg, err := Load(paths)
	if err != nil {
		return err
	}
	cfg.StrictHosts = on
	return save(paths, cfg)
}
