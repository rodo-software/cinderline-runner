package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/run"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// After CSR became CLR in the app, rename takes the new key from the
// instance and gives the project the ID of a newly added CLR: the secrets
// move, the old Docker volumes and session notes go (decision
// 2026-11-17-rename-moves-local-names). Also for an entry an older runner
// wrote with names_by_key, and for one 0.4.1 renamed, which kept its old ID.
func TestRename(t *testing.T) {
	u := instanceIn(t, "CLR", repoURL, "acme")
	c, out, errb := newCLI(t, goodToken+"\n")
	d := &fakeDocker{lists: map[string]string{"docker volume ls --quiet --filter label=cinderline.project=CSR": "cinderline-CSR-repo\n"}}
	c.exec = d.exec
	old := `{"version": 1, "projects": [{"url": "` + u + `", "key": "CSR", "workspace": "acme", "names_by_key": true, "repository": "` + repoURL + `"}]}`
	if err := os.WriteFile(c.Paths.File, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	stored := config.Project{LocalID: "CSR", URL: u, Workspace: "acme"} // its names were the key alone
	for k, v := range map[secrets.Kind]string{secrets.RunnerToken: goodToken, secrets.GitToken: "git"} {
		if _, err := c.Secrets.Set(stored, k, v); err != nil {
			t.Fatal(err)
		}
	}
	notes := run.PausedDir(c.Paths.State, stored)
	note := func(ref, origin string) {
		t.Helper()
		if err := os.MkdirAll(notes, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(notes, ref+".json"), []byte(`{"origin":"`+origin+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(why string) {
		t.Helper()
		errb.Reset()
		if code := c.Run([]string{"rename", "--project", "CSR"}); code != 1 || !strings.Contains(errb.String(), why) {
			t.Errorf("code %d, stderr %q; want a refusal with %q", code, errb.String(), why)
		}
		if got := projects(t, c); got[0].Key != "CSR" || secret(t, c, stored, secrets.GitToken) != "git" || len(d.calls) != 0 {
			t.Errorf("a refused rename changed something: %+v, docker %q", got, d.calls)
		}
	}

	// Work that may not be on the forge is in the volumes rename removes.
	note("CSR-3", "paused")
	refused("CSR-3")
	os.Remove(filepath.Join(notes, "CSR-3.json"))
	outbox := run.OutboxFile(c.Paths.State, stored)
	if err := os.MkdirAll(filepath.Dir(outbox), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outbox, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	refused("not received")
	os.Remove(outbox)

	note("CSR-4", "kept")
	errb.Reset()
	if code := c.Run([]string{"rename", "--project", "CSR"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "Renamed CSR to acme-CLR") || !strings.Contains(errb.String(), "names_by_key") {
		t.Errorf("stdout %q, stderr %q; want the rename and a warning of names_by_key", out.String(), errb.String())
	}
	got := projects(t, c)
	if len(got) != 1 || got[0].Key != "CLR" || got[0].ID() != "acme-CLR" || got[0].Repository != repoURL {
		t.Fatalf("projects %+v: want key CLR, ID acme-CLR and its repository", got)
	}
	if secret(t, c, got[0], secrets.RunnerToken) != goodToken || secret(t, c, got[0], secrets.GitToken) != "git" ||
		secret(t, c, stored, secrets.RunnerToken) != "" || secret(t, c, stored, secrets.GitToken) != "" {
		t.Error("want both secrets under the new ID and none under the old")
	}
	if want := []string{"docker", "volume", "rm", "--force", "cinderline-CSR-repo"}; len(d.calls) != 4 || !reflect.DeepEqual(d.calls[3], want) {
		t.Errorf("docker calls %q, want three lists and %q", d.calls, want)
	}
	if _, err := os.Stat(notes); !os.IsNotExist(err) {
		t.Errorf("session notes of the old ID are still there: %v", err)
	}
	if code := c.Run([]string{"key", "set", "runner-token", "--project", "CLR"}); code != 0 {
		t.Errorf("key set after rename: code %d, stderr %q", code, errb.String())
	}

	out.Reset()
	if code := c.Run([]string{"rename", "--project", "CLR"}); code != 0 || !strings.Contains(out.String(), "already") {
		t.Errorf("rename again: code %d, stdout %q", code, out.String())
	}

	// What 0.4.1's rename left: the new key with the old ID.
	half := got[0]
	half.LocalID = "CSR"
	saveConfig(t, c, half)
	if _, err := c.Secrets.Set(half, secrets.RunnerToken, goodToken); err != nil {
		t.Fatal(err)
	}
	if code := c.Run([]string{"rename", "--project", "CLR"}); code != 0 || projects(t, c)[0].ID() != "acme-CLR" {
		t.Errorf("new key with the old ID: code %d, stderr %q, projects %+v", code, errb.String(), projects(t, c))
	}

	// Another project that has the new name already.
	saveConfig(t, c, half, config.Project{URL: "https://b.example.com", Key: "CLR", Workspace: "acme", Repository: repoURL})
	if _, err := c.Secrets.Set(half, secrets.RunnerToken, goodToken); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	if code := c.Run([]string{"rename", "--project", "CLR", "-url", u}); code != 1 || !strings.Contains(errb.String(), "already names") {
		t.Errorf("a taken name: code %d, stderr %q", code, errb.String())
	}
	saveConfig(t, c, got[0])

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
