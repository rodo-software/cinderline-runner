package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// dockerRetry is how often run asks a silent daemon again (decision 2026-11-08-docker-prereq).
var dockerRetry = 10 * time.Second

var errNoDocker = errors.New("docker is not installed or not on PATH: install Docker from https://docs.docker.com/get-docker/ and try again")

// minFree is the free disk Docker needs: 5 GiB.
const minFree = 5 << 30

func dockerHint(goos string) string {
	switch goos {
	case "darwin", "windows":
		return `start Docker Desktop and wait until it says it is running`
	default:
		return `start it with "sudo systemctl start docker" (or "systemctl --user start docker" for rootless Docker)`
	}
}

// engineOK reports whether Docker Engine version v is 26.0 or later:
// volume-subpath, which mounts only an issue's own clone in its agent's
// container (CLR-207, decision 2026-11-19-docker-26), needs 26.0, which also
// has the fix for CVE-2024-29018 (older engines answer DNS lookups from an
// --internal network through the host's resolver, a way out past the egress
// proxy). A version it cannot read is too old.
func engineOK(v string) bool {
	var major int
	n, _ := fmt.Sscanf(v, "%d.", &major)
	return n == 1 && major >= 26
}

// checkDocker reports a missing docker, a silent daemon, an engine too old or low disk; with wait it waits for the daemon.
func (c *CLI) checkDocker(ctx context.Context, wait bool) error {
	x := c.exec

	var engine string
	version := func() error {
		out, err := x(ctx, nil, "docker", "version", "--format", "{{.Server.Version}}")
		if errors.Is(err, exec.ErrNotFound) {
			return errNoDocker
		}
		engine = strings.TrimSpace(out)
		return err
	}
	err := version()
	if err != nil && err != errNoDocker {
		msg := "the Docker daemon does not answer \"docker version\": " + dockerHint(runtime.GOOS)
		if !wait {
			return errors.New(msg)
		}
		fmt.Fprintln(c.Stderr, msg+"; waiting for it")
		for err != nil && err != errNoDocker {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(dockerRetry):
			}
			err = version()
		}
	}
	if err != nil {
		return err
	}
	if !engineOK(engine) {
		return fmt.Errorf("the Docker Engine, %s, is too old: %s needs 26.0 or later, to mount only an issue's own clone in its agent's container and to keep the agents' DNS lookups inside their network (CVE-2024-29018); update Docker Engine, or Docker Desktop to 4.29 or later", engine, name.Name)
	}

	out, err := x(ctx, nil, "docker", "run", "--rm", "--network", "none", "--entrypoint", "df", workspace.GitImage, "-Pk", "/")
	if err != nil {
		return fmt.Errorf("measuring Docker's free disk: %w", err)
	}
	free, err := workspace.ParseDF(out)
	if err != nil {
		return err
	}
	if free < minFree {
		return fmt.Errorf("only %.1f GB free for Docker's data, %s needs at least 5 GB: free space with \"docker system prune\" or give Docker more disk", float64(free)/(1<<30), name.Name)
	}
	return nil
}
