package daemon

import (
	"context"
	"os"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/loopback"
)

// TestMain keeps the package's tests off the network (CLR-202), and gives
// the memory budget a Docker of 16 GiB, as without one no slot is offered.
func TestMain(m *testing.M) {
	dockerMemory = func(context.Context) (int64, error) { return 16 << 30, nil }
	os.Exit(loopback.Run(m))
}
