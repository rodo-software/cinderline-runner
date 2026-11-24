//go:build integration

package run

// CLR-217 on real Docker: a turn killed at its memory limit is an
// OutOfMemoryError with that limit; exit code 137 without the kernel's kill
// is not. The integration CI job runs it with -run Integration ./internal/run/.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

func TestIntegrationOutOfMemory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	var logs lockedBuffer
	project := "ITM" + strconv.Itoa(os.Getpid())
	env := &environment.Docker{
		Project: project,
		Exec:    workspace.RunCommand,
		Log:     slog.New(slog.NewTextHandler(&logs, nil)),
		Memory:  64 << 20,
	}
	t.Cleanup(func() {
		if err := env.Cleanup(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		t.Log(logs.String())
	})
	run := func(issue, script string) (string, error) {
		return env.Run(ctx, environment.Spec{Issue: project + "-" + issue, Dir: "/tmp", Cmd: []string{"sh", "-c", script}, Timeout: 5 * time.Minute})
	}

	// tail keeps a newline-free stream whole, so it grows until the kernel
	// kills it; the shell then exits 137 itself.
	out, err := run("1", "head -c 1073741824 /dev/zero | tail")
	var oom *environment.OutOfMemoryError
	if !errors.As(err, &oom) || oom.Limit != 64<<20 {
		t.Errorf("Run = %v, want OutOfMemoryError at 64 MiB\n%s", err, out)
	}

	// A kill that is not the kernel's at the limit: exit 137 alone. Not of
	// the shell itself, which as the container's PID 1 ignores SIGKILL.
	out, err = run("2", "sleep 60 & kill -9 $!; wait $!")
	var exit interface{ ExitCode() int }
	if !errors.As(err, &exit) || exit.ExitCode() != 137 || errors.As(err, &oom) {
		t.Errorf("Run = %v, want exit code 137 and no OutOfMemoryError\n%s", err, out)
	}

	// The agent containers are gone, though they no longer run with --rm.
	left, err := workspace.RunCommand(ctx, nil, "docker", "ps", "--all", "--quiet", "--filter", "label="+workspace.ProjectLabel(project))
	if err != nil || strings.TrimSpace(left) != "" {
		t.Errorf("containers left = %q, %v", left, err)
	}
}
