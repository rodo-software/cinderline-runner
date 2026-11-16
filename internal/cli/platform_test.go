package cli

import (
	"strconv"
	"strings"
	"testing"
)

func TestCheckWindowsBuild(t *testing.T) {
	// 0 means not Windows; 19045 is Windows 10 22H2, 22631 Windows 11 23H2, 26100 Windows 11 24H2.
	for _, build := range []uint32{0, 19045, 22631, 26100} {
		if err := checkWindowsBuild(build); err != nil {
			t.Errorf("checkWindowsBuild(%d) = %v, want nil", build, err)
		}
	}
	for _, build := range []uint32{14393, 17763, 19044} {
		err := checkWindowsBuild(build)
		if err == nil {
			t.Fatalf("checkWindowsBuild(%d) = nil, want an error", build)
		}
		for _, want := range []string{"19045", "Windows 10 22H2", "Windows 11 23H2", strconv.Itoa(int(build))} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("checkWindowsBuild(%d) = %q, lacks %q", build, err, want)
			}
		}
	}
}

func TestRunRefusesOldWindowsBuild(t *testing.T) {
	c, _, errb := newCLI(t, "")
	c.WindowsBuild = 17763
	if code := c.Run([]string{"status"}); code != 1 {
		t.Errorf("status on build 17763: code %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "19045") {
		t.Errorf("stderr %q does not name build 19045", errb.String())
	}
}

func TestRunVersionOnOldWindowsBuild(t *testing.T) {
	c, out, _ := newCLI(t, "")
	c.WindowsBuild = 17763
	if code := c.Run([]string{"version"}); code != 0 || !strings.Contains(out.String(), "1.2.3") {
		t.Errorf("version on build 17763: code %d, stdout %q", code, out.String())
	}
}
