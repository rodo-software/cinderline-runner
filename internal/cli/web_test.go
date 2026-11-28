package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/web"
)

// CLR-269: web turns the page on or off and sets its port, runner-wide,
// keeps the projects, and without an argument says whether it is on and where.
func TestWeb(t *testing.T) {
	p := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"}
	c, _, errb := newCLI(t, "")
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
	if code := c.Run([]string{"web", "on"}); code != 0 || load().Web != "on" {
		t.Fatalf("on: code %d, web %q, stderr %q", code, load().Web, errb.String())
	}
	if code := c.Run([]string{"web", "port", "8080"}); code != 0 || load().WebPort != 8080 || load().Web != "on" {
		t.Fatalf("port: code %d, config %+v, stderr %q", code, load(), errb.String())
	}
	if len(projects(t, c)) != 1 {
		t.Error("the project was lost")
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

// webCLI is a CLI whose opener records the URL it was given instead of
// starting a browser.
func webCLI(t *testing.T, on bool) (c *CLI, out, errb *bytes.Buffer, opened *[]string) {
	t.Helper()
	c, out, errb = daemonCLI(t, "")
	cfg := &config.Config{WebPort: 8080}
	if !on {
		cfg.Web = "off"
	}
	if err := cfg.Save(c.Paths.File); err != nil {
		t.Fatal(err)
	}
	opened = new([]string)
	c.open = func(url string) error { *opened = append(*opened, url); return nil }
	return c, out, errb, opened
}

// CLR-272: web says how to fix a page that is off or a daemon that does not
// run, and exits non-zero without opening anything.
func TestWebOpenRefused(t *testing.T) {
	for _, tc := range []struct {
		name, fix string
		on, serve bool
	}{{"page off", "web on", false, true}, {"not running", " run", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			c, out, errb, opened := webCLI(t, tc.on)
			if tc.serve {
				serveFakeRunner(t, c, "", false)
			}
			if code := c.Run([]string{"web"}); code != 1 || !strings.Contains(errb.String(), tc.fix) || out.Len() != 0 || len(*opened) != 0 {
				t.Errorf("code %d, stdout %q, stderr %q, opened %q; want 1, a hint with %q, nothing opened", code, out, errb, *opened, tc.fix)
			}
		})
	}
}

// CLR-272: web prints the URL with the token and opens it; --no-open only
// prints it; a failing opener still exits 0.
func TestWebOpen(t *testing.T) {
	c, out, errb, opened := webCLI(t, true)
	serveFakeRunner(t, c, "", false)
	tok, err := web.Token(c.Paths.State)
	if err != nil {
		t.Fatal(err)
	}
	url := "http://127.0.0.1:8080/?token=" + tok
	if code := c.Run([]string{"web"}); code != 0 || !strings.Contains(out.String(), url) || len(*opened) != 1 || (*opened)[0] != url {
		t.Fatalf("code %d, stdout %q, stderr %q, opened %q; want %s printed and opened", code, out, errb, *opened, url)
	}
	out.Reset()
	if code := c.Run([]string{"web", "--no-open"}); code != 0 || !strings.Contains(out.String(), url) || len(*opened) != 1 {
		t.Errorf("--no-open: code %d, stdout %q, opened %q; want the URL printed, nothing opened", code, out, *opened)
	}
	c.open = func(string) error { return errors.New("no browser") }
	out.Reset()
	if code := c.Run([]string{"web"}); code != 0 || !strings.Contains(out.String(), url) {
		t.Errorf("failing opener: code %d, stdout %q; want 0 and the URL", code, out)
	}
	for _, bad := range [][]string{{"--no-open", "on"}, {"--new-token", "off"}, {"--new-token", "--no-open"}} {
		if code := c.Run(append([]string{"web"}, bad...)); code != 2 {
			t.Errorf("web %q: code %d, want 2", bad, code)
		}
	}
}

// CLR-272: --new-token replaces the token file, mode 0600, and says open
// pages must be opened again; it needs neither the page on nor the daemon.
func TestWebNewToken(t *testing.T) {
	c, out, errb, opened := webCLI(t, false)
	old, err := web.Token(c.Paths.State)
	if err != nil {
		t.Fatal(err)
	}
	if code := c.Run([]string{"web", "--new-token"}); code != 0 || !strings.Contains(out.String(), "again") || len(*opened) != 0 {
		t.Fatalf("code %d, stdout %q, stderr %q, opened %q", code, out, errb, *opened)
	}
	tok, err := web.Token(c.Paths.State)
	if err != nil || tok == old {
		t.Errorf("token %q (%v), old %q; want a new valid one", tok, err, old)
	}
	if fi, err := os.Stat(filepath.Join(c.Paths.State, "web-token")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("token file %v, %v; want mode 0600", fi, err)
	}
}
