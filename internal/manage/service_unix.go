//go:build !windows

package manage

import (
	"errors"

	"github.com/kardianos/service"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

type nopService struct{}

func (nopService) Start(service.Service) error { return nil }
func (nopService) Stop(service.Service) error  { return nil }

// ServiceInstalled reports whether the user service that runs the runner at
// login is installed; a service in a failed state is, with its error.
// Status needs only the service's name and that it is the user's.
func ServiceInstalled() (bool, error) {
	s, err := service.New(nopService{}, &service.Config{Name: name.Name, Option: service.KeyValue{"UserService": true}})
	if err != nil {
		return false, err
	}
	if _, err := s.Status(); err != nil {
		if errors.Is(err, service.ErrNotInstalled) {
			return false, nil
		}
		return true, err
	}
	return true, nil
}
