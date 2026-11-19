package run

import (
	"context"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// runCheck runs check in dir, with the agent's clone mounts but without its
// client state, tools or credentials, limited to maxMinutes; it reaches the
// project's hosts and allow. failed is a check that ran and failed, with
// the tail of its output; err is a stop or a check that could not run.
func (p *Project) runCheck(ctx context.Context, dir, check string, maxMinutes int, allow []string) (failed bool, tail string, err error) {
	mounts, err := workspace.CloneMounts(p.Config.ID(), dir)
	if err != nil {
		return false, "", fmt.Errorf("the check could not run: %w", err)
	}
	out, err := p.Env.Run(ctx, environment.Spec{
		Issue:    path.Base(dir),
		Dir:      dir,
		Mounts:   mounts,
		Cmd:      []string{"sh", "-c", check},
		Timeout:  time.Duration(maxMinutes) * time.Minute,
		AppAllow: allow,
	})
	var exit interface{ ExitCode() int }
	switch {
	case err == nil:
		return false, "", nil
	case ctx.Err() != nil:
		return false, "", ctx.Err()
	case !errors.As(err, &exit) && !errors.Is(err, environment.ErrTimeout):
		return false, "", fmt.Errorf("the check could not run: %w", err)
	}
	return true, environment.Tail(out), nil
}
