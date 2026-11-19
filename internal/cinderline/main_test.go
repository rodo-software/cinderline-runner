package cinderline

import (
	"os"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/loopback"
)

// TestMain keeps the package's tests off the network (CLR-202).
func TestMain(m *testing.M) { os.Exit(loopback.Run(m)) }
