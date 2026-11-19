package cli

// Acceptance tests for CSR-58: `login <client> --project <key>`. The docker
// argv per client is tested in internal/client (LoginArgv); these cover the
// command's arguments and its refusals, which happen before any docker call.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
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
	saveConfig(t, c, config.Project{Workspace: "default", URL: "https://cinderline.test", Key: "WEB", Repository: "https://git.test/o/r.git"})
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
		saveConfig(t, c, config.Project{Workspace: "default", URL: "https://cinderline.test", Key: "WEB", Repository: "https://git.test/o/r.git"})
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
	const imageLine, toolsLine = "Fetching the agent image", "Installing the agent tools"
	for _, tc := range []struct {
		name                       string
		imageMissing, toolsMissing bool
	}{{"present", false, false}, {"image missing", true, false}, {"tools missing", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			c, out, _ := newCLI(t, "")
			saveConfig(t, c, config.Project{Workspace: "default", URL: "https://cinderline.test", Key: "WEB", Repository: "https://git.test/o/r.git"})
			var fetched, downloaded bool
			c.exec = func(_ context.Context, _ []string, argv ...string) (string, error) {
				cmd := strings.Join(argv, " ")
				switch {
				case strings.HasPrefix(cmd, "docker image inspect"):
					if tc.imageMissing {
						return "", errors.New("no such image")
					}
				case strings.HasPrefix(cmd, "docker pull"), strings.HasPrefix(cmd, "docker build"):
					fetched = true
					if !strings.Contains(out.String(), imageLine) {
						t.Errorf("%s before the image line; stdout %q", cmd, out.String())
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
			if fetched != tc.imageMissing || downloaded != tc.toolsMissing {
				t.Errorf("fetched %v, downloaded %v", fetched, downloaded)
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

// TestLoginChecksCredentials covers CLR-199: login reports success only when
// the CLI's sign-in left its credentials file on the state volume the
// project's ID names, the one its runs mount; a project outside the default
// workspace has an ID other than its key. CLR-210: only that success
// deletes the client's stored model key, which would otherwise win over the
// login, and says so; every case runs with and without a stored key.
func TestLoginChecksCredentials(t *testing.T) {
	const volume = "cinderline-acme-WEB-state-codex"
	for _, tc := range []struct {
		name     string
		ttyErr   error // the sign-in itself
		checkErr error // test -f on the volume
		code     int
		stdout   string
		stderr   string
	}{
		{"saved", nil, nil, 0, "codex is signed in for project WEB", ""},
		{"not saved", nil, exitStatus(1), 1, "", "its credentials are not on " + volume},
		{"not checked", nil, errors.New("docker gone"), 1, "", "docker gone"},
		{"sign-in failed", errors.New("exit status 1"), nil, 1, "", "codex login: exit status 1"},
	} {
		for _, stored := range []bool{false, true} {
			t.Run(tc.name+"/stored key "+strconv.FormatBool(stored), func(t *testing.T) {
				c, out, errb := newCLI(t, "")
				p := config.Project{Workspace: "acme", URL: "https://cinderline.test", Key: "WEB", Repository: "https://git.test/o/r.git"}
				saveConfig(t, c, p)
				if stored {
					if _, err := c.Secrets.Set(p, secrets.OpenAIKey, "sk-old"); err != nil {
						t.Fatal(err)
					}
				}
				var checked []string
				c.exec = func(_ context.Context, _ []string, argv ...string) (string, error) {
					cmd := strings.Join(argv, " ")
					switch {
					case strings.HasPrefix(cmd, "docker version"):
						return "amd64", nil
					case strings.Contains(cmd, "ld-musl"):
						return "glibc", nil
					case strings.Contains(cmd, ":/state --entrypoint test"):
						checked = argv
						return "", tc.checkErr
					}
					return "", nil
				}
				c.http = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
					t.Errorf("downloaded %s; the tools volume is complete", r.URL)
					return nil, errors.New("offline")
				})}
				var signIn []string
				c.tty = func(_ context.Context, argv []string) error { signIn = argv; return tc.ttyErr }

				if code := c.Run([]string{"login", "codex", "--project", "WEB"}); code != tc.code {
					t.Fatalf("code %d, want %d; stderr %q", code, tc.code, errb.String())
				}
				if !strings.Contains(strings.Join(signIn, " "), volume+":/state") {
					t.Errorf("sign-in argv %q does not mount %s", signIn, volume)
				}
				if tc.ttyErr != nil {
					if checked != nil {
						t.Errorf("checked %q after a failed sign-in", checked)
					}
				} else if !strings.Contains(strings.Join(checked, " "), volume+":/state") {
					t.Errorf("check argv %q does not mount %s", checked, volume)
				}
				if tc.stdout != "" && !strings.Contains(out.String(), tc.stdout) {
					t.Errorf("stdout %q lacks %q", out.String(), tc.stdout)
				}
				if tc.code != 0 && strings.Contains(out.String(), "signed in") {
					t.Errorf("stdout %q reports a sign-in that failed", out.String())
				}
				if !strings.Contains(errb.String(), tc.stderr) {
					t.Errorf("stderr %q lacks %q", errb.String(), tc.stderr)
				}
				replaced := stored && tc.code == 0
				want := ""
				if stored && !replaced {
					want = "sk-old"
				}
				if got := secret(t, c, p, secrets.OpenAIKey); got != want {
					t.Errorf("stored key %q after the login, want %q", got, want)
				}
				if got := strings.Contains(out.String(), "replace"); got != replaced {
					t.Errorf("stdout %q mentions a replaced key: %v, want %v", out.String(), got, replaced)
				}
			})
		}
	}
}

type exitStatus int

func (e exitStatus) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitStatus) ExitCode() int { return int(e) }
