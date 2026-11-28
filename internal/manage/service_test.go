package manage

import (
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/name"
)

func TestServiceStartPerOS(t *testing.T) {
	for goos, want := range map[string]string{
		"linux":   "systemctl --user start " + name.Name,
		"darwin":  "launchctl start " + name.Name,
		"windows": "schtasks /run /tn " + name.Name,
	} {
		if got := ServiceStart(goos); got != want {
			t.Errorf("ServiceStart(%q) = %q, want %q", goos, got, want)
		}
	}
}
