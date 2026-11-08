package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

var (
	versionArgv = "docker version --format {{.Server.Version}}"
	dfArgv      = "docker run --rm --network none --entrypoint df " + workspace.GitImage + " -Pk /"
)

// dfOut is df -Pk output with avail KiB available.
func dfOut(avail int) string {
	return fmt.Sprintf("Filesystem     1024-blocks      Used Available Capacity Mounted on\noverlay          61202244  20414012  %d      36%% /\n", avail)
}

var errNotFound = fmt.Errorf("docker version: %w", &exec.Error{Name: "docker", Err: exec.ErrNotFound})
var errNoDaemon = errors.New("docker version: exit status 1\nCannot connect to the Docker daemon at unix:///var/run/docker.sock")

// scriptedDocker answers versionArgv with versionErrs in turn (nil once they run out)
// and dfArgv with df; it records every call.
type scriptedDocker struct {
	calls       []string
	versionErrs []error
	df          string
}

func (s *scriptedDocker) exec(_ context.Context, _ []string, argv ...string) (string, error) {
	a := strings.Join(argv, " ")
	s.calls = append(s.calls, a)
	switch a {
	case versionArgv:
		if len(s.versionErrs) > 0 {
			err := s.versionErrs[0]
			s.versionErrs = s.versionErrs[1:]
			if err != nil {
				return "", err
			}
		}
		return "27.3.1\n", nil
	case dfArgv:
		return s.df, nil
	}
	return "", fmt.Errorf("unexpected docker call: %s", a)
}

// healthyDocker is a docker with its daemon up and 40 GB free.
func healthyDocker(ctx context.Context, env []string, argv ...string) (string, error) {
	return (&scriptedDocker{df: dfOut(40 << 20)}).exec(ctx, env, argv...)
}

func TestPrereqEnoughDiskPasses(t *testing.T) {
	c, _, _ := newCLI(t, "")
	d := &scriptedDocker{df: dfOut(6 << 20)}
	c.exec = d.exec
	if err := c.checkDocker(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if want := []string{versionArgv, dfArgv}; strings.Join(d.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q, want %q", d.calls, want)
	}
}

func TestPrereqDockerNotOnPath(t *testing.T) {
	c, _, _ := newCLI(t, "")
	d := &scriptedDocker{versionErrs: []error{errNotFound}}
	c.exec = d.exec
	err := c.checkDocker(context.Background(), true) // even when waiting: a missing docker never appears
	if err == nil || !strings.Contains(err.Error(), "not on PATH") || !strings.Contains(err.Error(), "https://docs.docker.com/get-docker/") {
		t.Fatalf("err = %v, want docker not on PATH with the install link", err)
	}
	if len(d.calls) != 1 {
		t.Fatalf("calls = %q, want only docker version", d.calls)
	}
}

func TestPrereqDaemonNotAnswering(t *testing.T) {
	c, _, _ := newCLI(t, "")
	d := &scriptedDocker{versionErrs: []error{errNoDaemon}}
	c.exec = d.exec
	err := c.checkDocker(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "daemon") || !strings.Contains(err.Error(), dockerHint(runtime.GOOS)) {
		t.Fatalf("err = %v, want daemon not answering with hint %q", err, dockerHint(runtime.GOOS))
	}
	if len(d.calls) != 1 {
		t.Fatalf("calls = %q, want only docker version", d.calls)
	}
}

func TestDockerHintPerOS(t *testing.T) {
	for goos, want := range map[string]string{"darwin": "Docker Desktop", "windows": "Docker Desktop", "linux": "systemctl"} {
		if h := dockerHint(goos); !strings.Contains(h, want) {
			t.Errorf("dockerHint(%q) = %q, want it to mention %q", goos, h, want)
		}
	}
}

func TestPrereqLowDisk(t *testing.T) {
	c, _, _ := newCLI(t, "")
	c.exec = (&scriptedDocker{df: dfOut(1 << 20)}).exec
	err := c.checkDocker(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "1.0 GB") || !strings.Contains(err.Error(), "5 GB") {
		t.Fatalf("err = %v, want 1.0 GB free against the 5 GB needed", err)
	}
	if minFreeKiB != 5<<20 {
		t.Fatalf("minFreeKiB = %d, want 5 GiB in KiB", minFreeKiB)
	}
}

func TestPrereqUnreadableDf(t *testing.T) {
	c, _, _ := newCLI(t, "")
	c.exec = (&scriptedDocker{df: "garbage\n"}).exec
	if err := c.checkDocker(context.Background(), false); err == nil {
		t.Fatal("want an error for unreadable df output")
	}
}

// Decision 2026-11-08-docker-prereq: run waits for the daemon, saying so once.
func TestPrereqRunWaitsForDaemon(t *testing.T) {
	old := dockerRetry
	dockerRetry = 10 * time.Millisecond
	t.Cleanup(func() { dockerRetry = old })
	c, _, errb := newCLI(t, "")
	d := &scriptedDocker{versionErrs: []error{errNoDaemon, errNoDaemon}, df: dfOut(40 << 20)}
	c.exec = d.exec
	if err := c.checkDocker(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(d.calls, "|"), versionArgv); n != 3 {
		t.Fatalf("docker version called %d times, want 3", n)
	}
	if n := strings.Count(errb.String(), dockerHint(runtime.GOOS)); n != 1 {
		t.Fatalf("hint printed %d times on stderr, want once: %q", n, errb.String())
	}
}

func TestPrereqWaitStopsOnCancel(t *testing.T) {
	old := dockerRetry
	dockerRetry = 10 * time.Millisecond
	t.Cleanup(func() { dockerRetry = old })
	c, _, _ := newCLI(t, "")
	c.exec = func(context.Context, []string, ...string) (string, error) { return "", errNoDaemon }
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.checkDocker(ctx, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's error", err)
	}
}

func TestAddChecksDockerFirst(t *testing.T) {
	c, out, errb := newCLI(t, "")
	c.exec = (&scriptedDocker{versionErrs: []error{errNotFound}}).exec
	if code := c.Run([]string{"add"}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "not on PATH") || strings.Contains(out.String(), "URL of the") {
		t.Fatalf("stdout %q stderr %q: want the docker message before any question", out.String(), errb.String())
	}
}

func TestAddFailsOnDaemonWithoutWaiting(t *testing.T) {
	c, _, errb := newCLI(t, "")
	c.exec = func(context.Context, []string, ...string) (string, error) { return "", errNoDaemon }
	if code := c.Run([]string{"add"}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), dockerHint(runtime.GOOS)) {
		t.Fatalf("stderr %q, want the daemon hint", errb.String())
	}
}

func TestRunChecksDocker(t *testing.T) {
	c, _, errb := daemonCLI(t, "")
	c.exec = (&scriptedDocker{df: dfOut(1 << 20)}).exec
	if code := c.Run([]string{"run"}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "5 GB") {
		t.Fatalf("stderr %q, want the low disk message", errb.String())
	}
}

func TestPrereqFullDisk(t *testing.T) {
	c, _, _ := newCLI(t, "")
	c.exec = (&scriptedDocker{df: dfOut(0)}).exec
	if err := c.checkDocker(context.Background(), false); err == nil || !strings.Contains(err.Error(), "0.0 GB") {
		t.Fatalf("err = %v, want 0.0 GB free", err)
	}
}
