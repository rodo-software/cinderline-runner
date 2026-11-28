package manage

import (
	"fmt"
	"slices"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

// The functions here manage allow_hosts, of the project with ID project
// or, for an empty one, of all current and future projects of this runner.
// The egress proxy matches hosts in lower case, so entries are stored and
// compared in lower case.

// AlreadyAllowed is AddHost's refusal of an entry that is there already:
// Host as stored, in the project with key Project, or runner-wide for "".
type AlreadyAllowed struct{ Project, Host string }

func (a AlreadyAllowed) Error() string {
	if a.Project == "" {
		return "all projects already allow " + a.Host
	}
	return "project " + a.Project + " already allows " + a.Host
}

// CheckAddHost refuses what AddHost would refuse, and writes nothing: the
// CLI asks its confirmation only for an add that would go through.
func CheckAddHost(paths config.Paths, project, hostPort string) error {
	cfg, err := Load(paths)
	if err != nil {
		return err
	}
	_, err = checkAdd(cfg, project, hostPort)
	return err
}

// AddHost adds hostPort to the allow_hosts of the project, or runner-wide.
func AddHost(paths config.Paths, project, hostPort string) error {
	cfg, err := Load(paths)
	if err != nil {
		return err
	}
	hosts, err := checkAdd(cfg, project, hostPort)
	if err != nil {
		return err
	}
	*hosts = append(slices.Clone(*hosts), strings.ToLower(hostPort))
	return save(paths, cfg)
}

// RemoveHost removes hostPort from the allow_hosts of the project, or
// runner-wide.
func RemoveHost(paths config.Paths, project, hostPort string) error {
	cfg, err := Load(paths)
	if err != nil {
		return err
	}
	hosts, key, err := hostList(cfg, project)
	if err != nil {
		return err
	}
	j := slices.IndexFunc(*hosts, func(e string) bool { return strings.EqualFold(e, hostPort) })
	if j < 0 {
		now := "none"
		if len(*hosts) > 0 {
			now = strings.Join(*hosts, ", ")
		}
		if key == "" {
			return Refused{fmt.Errorf("runner-wide allow_hosts has no entry %s; its entries: %s", hostPort, now)}
		}
		return Refused{fmt.Errorf("project %s has no allow_hosts entry %s; its entries: %s", key, hostPort, now)}
	}
	*hosts = slices.Delete(slices.Clone(*hosts), j, j+1)
	return save(paths, cfg)
}

// checkAdd returns the allow_hosts hostPort would join, or why it may not.
func checkAdd(cfg *config.Config, project, hostPort string) (*[]string, error) {
	hosts, key, err := hostList(cfg, project)
	if err != nil {
		return nil, err
	}
	hp := strings.ToLower(hostPort)
	switch {
	case !config.ValidHostPort(hp):
		return nil, Refused{fmt.Errorf("%q must be host:port, without wildcards", hostPort)}
	case !config.AllowedHost(hp):
		return nil, Refused{fmt.Errorf("%q is the runner's own machine, which allow_hosts never opens; add a model provider instead with `%s add-provider NAME URL`", hostPort, name.Name)}
	}
	if j := slices.IndexFunc(*hosts, func(e string) bool { return strings.EqualFold(e, hp) }); j >= 0 {
		return nil, Refused{AlreadyAllowed{key, (*hosts)[j]}}
	}
	return hosts, nil
}

// hostList returns the allow_hosts of the project in cfg and its key, or
// the runner-wide ones and "".
func hostList(cfg *config.Config, project string) (hosts *[]string, key string, err error) {
	if project == "" {
		return &cfg.AllowHosts, "", nil
	}
	p, i, err := findProject(cfg, project)
	if err != nil {
		return nil, "", err
	}
	return &cfg.Projects[i].AllowHosts, p.Key, nil
}
