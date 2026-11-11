package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// dockerRetry is how often run asks a silent daemon again (decision 2026-11-08-docker-prereq).
var dockerRetry = 10 * time.Second

var errNoDocker = errors.New("docker is not installed or not on PATH: install Docker from https://docs.docker.com/get-docker/ and try again")

// minFreeKiB is the free disk Docker needs: 5 GiB, as df -k reports it.
const minFreeKiB = 5 << 20

func dockerHint(goos string) string {
	switch goos {
	case "darwin", "windows":
		return `start Docker Desktop and wait until it says it is running`
	default:
		return `start it with "sudo systemctl start docker" (or "systemctl --user start docker" for rootless Docker)`
	}
}

// keepsDNSInside reports whether Docker Engine version v has the fix for
// CVE-2024-29018 (23.0.11, 25.0.5, 26.0.0): older engines answer DNS lookups
// from an --internal network through the host's resolver, a way out past the
// egress proxy. A version it cannot read does not have it.
func keepsDNSInside(v string) bool {
	var major, minor, patch int
	if n, _ := fmt.Sscanf(v, "%d.%d.%d", &major, &minor, &patch); n != 3 {
		return false
	}
	return major >= 26 || major == 25 && (minor > 0 || patch >= 5) || major == 23 && (minor > 0 || patch >= 11)
}

// checkDocker reports a missing docker, a silent daemon, an engine too old or low disk; with wait it waits for the daemon.
func (c *CLI) checkDocker(ctx context.Context, wait bool) error {
	x := c.exec
	if x == nil {
		x = workspace.RunCommand
	}

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
	if !keepsDNSInside(engine) {
		return fmt.Errorf("the Docker Engine, %s, is too old: it forwards DNS lookups from the agents' internal network, which lets an agent send data out past the allowlist (CVE-2024-29018); update to 25.0.5 or later (or 23.0.11 or later of 23.0)", engine)
	}

	out, err := x(ctx, nil, "docker", "run", "--rm", "--network", "none", "--entrypoint", "df", workspace.GitImage, "-Pk", "/")
	if err != nil {
		return fmt.Errorf("measuring Docker's free disk: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return fmt.Errorf("cannot read free disk from df output: %q", out)
	}
	freeKiB, err := strconv.Atoi(fields[3])
	if err != nil {
		return fmt.Errorf("cannot read free disk from df output: %q", out)
	}
	if freeKiB < minFreeKiB {
		return fmt.Errorf("only %.1f GB free for Docker's data, %s needs at least 5 GB: free space with \"docker system prune\" or give Docker more disk", float64(freeKiB)/(1<<20), name.Name)
	}
	return nil
}
