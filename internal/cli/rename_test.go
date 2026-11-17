package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// CLR-191: after CSR became CLR in the app, rename takes the new key from the
// instance and every local name stays, also for an entry an older runner
// wrote with names_by_key; key set runner-token then takes a CLR token.
func TestRename(t *testing.T) {
	u := instanceIn(t, "CLR", repoURL, "acme")
	c, out, errb := newCLI(t, goodToken+"\n")
	old := `{"version": 1, "projects": [{"url": "` + u + `", "key": "CSR", "workspace": "acme", "names_by_key": true, "repository": "` + repoURL + `"}]}`
	if err := os.WriteFile(c.Paths.File, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	stored := config.Project{LocalID: "CSR", URL: u, Workspace: "acme"} // its names were the key alone
	if _, err := c.Secrets.Set(stored, secrets.RunnerToken, goodToken); err != nil {
		t.Fatal(err)
	}
	if code := c.Run([]string{"rename", "--project", "CSR"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "Renamed CSR to CLR") || !strings.Contains(errb.String(), "names_by_key") {
		t.Errorf("stdout %q, stderr %q; want the rename and a warning of names_by_key", out.String(), errb.String())
	}
	got := projects(t, c)
	if len(got) != 1 || got[0].Key != "CLR" || got[0].ID() != "CSR" || secret(t, c, got[0], secrets.RunnerToken) != goodToken {
		t.Fatalf("projects %+v: want key CLR, ID CSR and its runner token", got)
	}
	if code := c.Run([]string{"key", "set", "runner-token", "--project", "CLR"}); code != 0 {
		t.Errorf("key set after rename: code %d, stderr %q", code, errb.String())
	}

	out.Reset()
	if code := c.Run([]string{"rename", "--project", "CLR"}); code != 0 || !strings.Contains(out.String(), "already") {
		t.Errorf("rename again: code %d, stdout %q", code, out.String())
	}

	if _, err := c.Secrets.Set(got[0], secrets.RunnerToken, "revoked"); err != nil {
		t.Fatal(err)
	}
	if code := c.Run([]string{"rename", "--project", "CLR"}); code != 1 || !strings.Contains(errb.String(), "rejected") {
		t.Errorf("rejected token: code %d, stderr %q", code, errb.String())
	}
	if err := c.Secrets.Delete(got[0], secrets.RunnerToken); err != nil {
		t.Fatal(err)
	}
	if code := c.Run([]string{"rename", "--project", "CLR"}); code != 1 || !strings.Contains(errb.String(), "no runner token") {
		t.Errorf("no token: code %d, stderr %q", code, errb.String())
	}
}
