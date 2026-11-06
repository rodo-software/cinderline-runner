package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/rodo-software/cinderstack-runner/internal/workspace"
)

// detectMusl says whether image is musl-based, by the dynamic loader
// Claude Code's own installer looks for.
func detectMusl(ctx context.Context, x workspace.Exec, image string) (bool, error) {
	out, err := x(ctx, nil, "docker", "run", "--rm", "--network", "none", "--entrypoint", "sh", image,
		"-c", "ls /lib/ld-musl-* >/dev/null 2>&1 && echo musl || echo glibc")
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(out) {
	case "musl":
		return true, nil
	case "glibc":
		return false, nil
	}
	return false, fmt.Errorf("libc of %s: unexpected answer %q", image, out)
}
