package cli

// Acceptance tests for CSR-58: `login <client> --project <key>`. The docker
// argv per client is tested in internal/client (LoginArgv); these cover the
// command's arguments and its refusals, which happen before any docker call.

import (
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
