package cli

import (
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/tools"
)

// CLR-317: pin holds a client at a version runner-wide, latest lets it go,
// and without arguments it shows each client's version and pin.
func TestPin(t *testing.T) {
	web := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"}
	c, out, _ := newCLI(t, "")
	saveConfig(t, c, web)
	pinned := func() map[string]string {
		t.Helper()
		cfg, err := config.Load(c.Paths.File)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Pinned
	}
	for _, bad := range [][]string{{"claude"}, {"cursor", "1.0.0"}, {"claude", "1.0.0", "x"}} {
		if code := c.Run(append([]string{"pin"}, bad...)); code != 2 {
			t.Errorf("pin %q: code %d, want 2", bad, code)
		}
	}
	if code := c.Run([]string{"pin", "claude", "2.1.316/../x"}); code != 1 || pinned() != nil {
		t.Errorf("bad version: code %d, pins %v", code, pinned())
	}
	if code := c.Run([]string{"pin", "claude", "2.1.316"}); code != 0 || pinned()["claude"] != "2.1.316" {
		t.Fatalf("pin: code %d, pins %v", code, pinned())
	}
	out.Reset()
	if code := c.Run([]string{"pin"}); code != 0 || !strings.Contains(out.String(), "claude "+tools.ClaudeVersion+", pinned at 2.1.316") ||
		!strings.Contains(out.String(), "codex "+tools.CodexVersion+", follows its vendor's latest release") {
		t.Errorf("pin = %d, %q", code, out.String())
	}
	if code := c.Run([]string{"pin", "claude", "latest"}); code != 0 || pinned() != nil {
		t.Errorf("latest: code %d, pins %v", code, pinned())
	}
	if len(projects(t, c)) != 1 {
		t.Error("the project was lost")
	}
}
