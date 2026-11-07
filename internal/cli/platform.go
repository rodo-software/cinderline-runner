package cli

import (
	"fmt"

	"github.com/rodo-software/cinderstack-runner/internal/name"
)

// Docker Desktop's minimum, plan M2 "Local socket on Windows"
const minWindowsBuild = 19045

func checkWindowsBuild(build uint32) error {
	if build == 0 || build >= minWindowsBuild {
		return nil
	}
	return fmt.Errorf("%s needs Windows 10 22H2 (build 19045) or Windows 11 23H2 or later, for Docker Desktop with WSL2; this is Windows build %d", name.Name, build)
}
