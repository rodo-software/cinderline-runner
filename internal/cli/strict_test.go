package cli

import (
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
)

// CLR-193: strict-hosts turns strict host blocking on and off, runner-wide,
// keeps the projects, and without an argument says which it is.
func TestStrictHosts(t *testing.T) {
	web := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"}
	c, out, errb := newCLI(t, "")
	saveConfig(t, c, web)
	strict := func() bool {
		t.Helper()
		cfg, err := config.Load(c.Paths.File)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.StrictHosts
	}
	for _, bad := range [][]string{{"yes"}, {"on", "off"}, {"--project", "WEB", "on"}} {
		if code := c.Run(append([]string{"strict-hosts"}, bad...)); code != 2 {
			t.Errorf("strict-hosts %q: code %d, want 2", bad, code)
		}
	}
	if code := c.Run([]string{"strict-hosts"}); code != 0 || !strings.Contains(out.String(), "off") {
		t.Errorf("strict-hosts = %d, %q; want it off", code, out.String())
	}
	if code := c.Run([]string{"strict-hosts", "on"}); code != 0 || !strict() {
		t.Fatalf("on: code %d, strict %v, stderr %q", code, strict(), errb.String())
	}
	if len(projects(t, c)) != 1 {
		t.Error("the project was lost")
	}
	out.Reset()
	if code := c.Run([]string{"strict-hosts"}); code != 0 || !strings.Contains(out.String(), "on") {
		t.Errorf("strict-hosts = %d, %q; want it on", code, out.String())
	}
	if code := c.Run([]string{"strict-hosts", "off"}); code != 0 || strict() {
		t.Errorf("off: code %d, strict %v", code, strict())
	}
}
