package cli

// Acceptance tests for CSR-58: `login <client> --project <key>`. The docker
// argv per client is tested in internal/client (LoginArgv); these cover the
// command's arguments and its refusals, which happen before any docker call.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
)

func TestLoginUsage(t *testing.T) {
	for _, args := range [][]string{
		{"login"},
		{"login", "claude"},
		{"login", "claude", "--project"},
		{"login", "claude", "--project", "WEB", "extra"},
	} {
		c, _, errb := newCLI(t, "")
		if code := c.Run(args); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
		if !strings.Contains(errb.String(), "usage:") {
			t.Errorf("%v: stderr %q lacks usage:", args, errb.String())
		}
	}
	c, _, errb := newCLI(t, "")
	c.Run(nil)
	if !strings.Contains(errb.String(), "  login") {
		t.Errorf("usage %q does not list login", errb.String())
	}
}

func TestLoginUnknownProject(t *testing.T) {
	c, _, errb := newCLI(t, "")
	saveConfig(t, c, config.Project{URL: "https://cinderline.test", Key: "WEB", Repository: "https://git.test/o/r.git"})
	if code := c.Run([]string{"login", "claude", "--project", "NOPE"}); code != 1 {
		t.Errorf("code %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "NOPE") {
		t.Errorf("stderr %q does not name the project", errb.String())
	}
}

func TestLoginClientWithoutLogin(t *testing.T) {
	for client, want := range map[string]string{"opencode": "OpenRouter", "cursor": "cursor"} {
		c, _, errb := newCLI(t, "")
		saveConfig(t, c, config.Project{URL: "https://cinderline.test", Key: "WEB", Repository: "https://git.test/o/r.git"})
		if code := c.Run([]string{"login", client, "--project", "WEB"}); code != 1 {
			t.Errorf("%s: code %d, want 1", client, code)
		}
		if !strings.Contains(errb.String(), want) {
			t.Errorf("%s: stderr %q lacks %q", client, errb.String(), want)
		}
	}
}

// TestLoginFirstTimeLines covers CSR-149: one stdout line before each slow
// first-time step, printed before the step starts, and none when the image
// and tools volume are already there. The fake docker stops login at the
// state volume, before the real interactive docker run.
func TestLoginFirstTimeLines(t *testing.T) {
	const imageLine, toolsLine = "Building the agent image", "Installing the agent tools"
	for _, tc := range []struct {
		name                       string
		imageMissing, toolsMissing bool
	}{{"present", false, false}, {"image missing", true, false}, {"tools missing", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			c, out, _ := newCLI(t, "")
			saveConfig(t, c, config.Project{URL: "https://cinderline.test", Key: "WEB", Repository: "https://git.test/o/r.git"})
			var built, downloaded bool
			c.exec = func(_ context.Context, _ []string, argv ...string) (string, error) {
				cmd := strings.Join(argv, " ")
				switch {
				case strings.HasPrefix(cmd, "docker image inspect"):
					if tc.imageMissing {
						return "", errors.New("no such image")
					}
				case strings.HasPrefix(cmd, "docker build"):
					built = true
					if !strings.Contains(out.String(), imageLine) {
						t.Errorf("docker build before the image line; stdout %q", out.String())
					}
				case strings.HasPrefix(cmd, "docker version"):
					return "amd64", nil
				case strings.Contains(cmd, "ld-musl"):
					return "glibc", nil
				case strings.Contains(cmd, "/t/complete"):
					if tc.toolsMissing {
						return "", errors.New("not complete")
					}
				case strings.HasPrefix(cmd, "docker volume create"):
					return "", errors.New("stop here")
				}
				return "", nil
			}
			c.http = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
				downloaded = true
				if !strings.Contains(out.String(), toolsLine) {
					t.Errorf("download before the tools line; stdout %q", out.String())
				}
				return nil, errors.New("offline")
			})}
			if code := c.Run([]string{"login", "claude", "--project", "WEB"}); code != 1 {
				t.Errorf("code %d, want 1 (the fake stops login)", code)
			}
			if built != tc.imageMissing || downloaded != tc.toolsMissing {
				t.Errorf("built %v, downloaded %v", built, downloaded)
			}
			if got := strings.Contains(out.String(), imageLine); got != tc.imageMissing {
				t.Errorf("image line printed %v, want %v; stdout %q", got, tc.imageMissing, out.String())
			}
			if got := strings.Contains(out.String(), toolsLine); got != tc.toolsMissing {
				t.Errorf("tools line printed %v, want %v; stdout %q", got, tc.toolsMissing, out.String())
			}
		})
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
