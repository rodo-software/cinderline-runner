//go:build !windows

package cli

import (
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
