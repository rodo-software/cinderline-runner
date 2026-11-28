package manage

import (
	"time"

	"github.com/rodo-software/cinderline-runner/internal/ca"
	"github.com/rodo-software/cinderline-runner/internal/config"
)

// RotateCA replaces the runner's CA (CLR-247) and returns the new one. A
// running runner needs no reload: each container takes the CA as it is when
// it starts, so those running now keep the old one.
func RotateCA(paths config.Paths) (*ca.CA, error) {
	return ca.Rotate(ca.Dir(paths.State), time.Now())
}
