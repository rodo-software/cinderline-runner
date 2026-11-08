//go:build !windows

package cli

import (
	"errors"
	"os"

	"github.com/kardianos/service"
)

// nopService satisfies service.Interface; the unit runs the ordinary cinderstack run.
type nopService struct{}

func (nopService) Start(service.Service) error { return nil }
func (nopService) Stop(service.Service) error  { return nil }

func installService(exe string) error {
	s, err := service.New(nopService{}, serviceConfig(exe, os.Getenv("PATH")))
	if err != nil {
		return err
	}
	if err := s.Install(); err != nil {
		return err
	}
	return s.Start()
}

func uninstallService(exe string) error {
	s, err := service.New(nopService{}, serviceConfig(exe, os.Getenv("PATH")))
	if err != nil {
		return err
	}
	_ = s.Stop() // may not be running
	return s.Uninstall()
}

// startService starts the installed service; installed is false, and nothing is done, when there is none.
func startService(exe string) (installed bool, err error) {
	s, err := service.New(nopService{}, serviceConfig(exe, os.Getenv("PATH")))
	if err != nil {
		return false, err
	}
	if _, err := s.Status(); err != nil {
		if errors.Is(err, service.ErrNotInstalled) {
			return false, nil
		}
		return true, err
	}
	return true, s.Start()
}
