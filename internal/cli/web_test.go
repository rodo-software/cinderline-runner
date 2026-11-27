package cli

import (
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
)

// CLR-269: web turns the page on or off and sets its port, runner-wide,
// keeps the projects, and without an argument says whether it is on and where.
func TestWeb(t *testing.T) {
	p := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"}
	c, out, errb := newCLI(t, "")
	saveConfig(t, c, p)
	load := func() *config.Config {
		t.Helper()
		cfg, err := config.Load(c.Paths.File)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	for _, bad := range [][]string{{"yes"}, {"on", "off"}, {"port"}, {"port", "1", "2"}, {"--project", "WEB", "on"}} {
		if code := c.Run(append([]string{"web"}, bad...)); code != 2 {
			t.Errorf("web %q: code %d, want 2", bad, code)
		}
	}
	for _, bad := range []string{"x", "80", "65536"} {
		if code := c.Run([]string{"web", "port", bad}); code != 1 || load().WebPort != 0 {
			t.Errorf("web port %s: code %d, web_port %d; want 1 and unchanged", bad, code, load().WebPort)
		}
	}
	out.Reset()
	if code := c.Run([]string{"web"}); code != 0 || !strings.Contains(out.String(), "off") || !strings.Contains(out.String(), "127.0.0.1:7420") {
		t.Errorf("web = %d, %q; want it off at 127.0.0.1:7420", code, out.String())
	}
	if code := c.Run([]string{"web", "on"}); code != 0 || load().Web != "on" {
		t.Fatalf("on: code %d, web %q, stderr %q", code, load().Web, errb.String())
	}
	if code := c.Run([]string{"web", "port", "8080"}); code != 0 || load().WebPort != 8080 || load().Web != "on" {
		t.Fatalf("port: code %d, config %+v, stderr %q", code, load(), errb.String())
	}
	if len(projects(t, c)) != 1 {
		t.Error("the project was lost")
	}
	out.Reset()
	if code := c.Run([]string{"web"}); code != 0 || !strings.Contains(out.String(), "on") || !strings.Contains(out.String(), "127.0.0.1:8080") {
		t.Errorf("web = %d, %q; want it on at 127.0.0.1:8080", code, out.String())
	}
	if code := c.Run([]string{"web", "off"}); code != 0 || load().Web != "off" {
		t.Errorf("off: code %d, web %q", code, load().Web)
	}
}

// CLR-269: a change by web reloads a running runner.
func TestWebReloads(t *testing.T) {
	c, _, _ := reloadCLI(t, "")
	f := serveFakeRunner(t, c, "", false)
	if code := c.Run([]string{"web", "port", "8080"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, c.Stderr)
	}
	if _, _, reloaded := f.state(); !reloaded {
		t.Error("the runner was not reloaded")
	}
}
