//go:build !windows

package cli

import (
	"os"

	"github.com/kardianos/service"
	"github.com/rodo-software/cinderline-runner/internal/manage"
)

// nopService satisfies service.Interface; the unit runs the ordinary cinderline run.
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
	if installed, err := manage.ServiceInstalled(); !installed || err != nil {
		return installed, err
	}
	s, err := service.New(nopService{}, serviceConfig(exe, os.Getenv("PATH")))
	if err != nil {
		return true, err
	}
	return true, s.Start()
}
