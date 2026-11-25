package workspace

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// An agent never sees the forge's URL (CL decision
// 2026-11-25-forge-off-limits-for-agents): git writes the URL a fetch came
// from into FETCH_HEAD, so neither the repo volume nor an extra repository
// may keep one. These run the scripts with the machine's sh and git.

// sh runs script with args in dir, git's own config only.
func sh(t *testing.T, dir, script string, args ...string) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"-c", script, "sh"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
}

// forge makes a repository with one commit at a path that stands for the
// forge's URL, and returns its file:// URL.
func forge(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	src := filepath.Join(t.TempDir(), "forge.example.com", "o", "r")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	sh(t, src, `git init --quiet && echo hi > a && git add a && git commit --quiet -m init`)
	return "file://" + src
}

// noURL fails when a file under dir, other than its objects, names url's
// forge host.
func noURL(t *testing.T, dir, url string) {
	t.Helper()
	host := "forge.example.com"
	if !strings.Contains(url, host) {
		t.Fatalf("url %s lacks %s", url, host)
	}
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			if e != nil && e.Name() == "objects" {
				return filepath.SkipDir
			}
			return err
		}
		b, err := os.ReadFile(p)
		if err == nil && strings.Contains(string(b), host) {
			t.Errorf("%s names the forge: %q", p, b)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRepoVolumeKeepsNoForgeURL(t *testing.T) {
	url := forge(t)
	repo := filepath.Join(t.TempDir(), "repo")
	// A FETCH_HEAD left by an older runner, which wrote one on every fetch.
	sh(t, t.TempDir(), `git init --quiet --bare "$1" && git -C "$1" fetch --quiet --no-tags "$2" "+refs/heads/*:refs/heads/*"`, repo, url)
	sh(t, t.TempDir(), initRepoScript, repo)
	sh(t, t.TempDir(), `git -C "$1" fetch --prune --quiet --no-tags --no-write-fetch-head "$2" "+refs/heads/*:refs/heads/*"`, repo, url)
	noURL(t, repo, url)
}

func TestExtraKeepsNoForgeURL(t *testing.T) {
	url := forge(t)
	mount := t.TempDir()
	for range 2 { // a new folder, then one fetched before
		sh(t, mount, fetchExtraScript, "lib", url, "HEAD")
	}
	if b, err := os.ReadFile(filepath.Join(mount, "lib", "a")); err != nil || string(b) != "hi\n" {
		t.Fatalf("lib/a = %q, %v", b, err)
	}
	noURL(t, filepath.Join(mount, "lib"), url)
}
