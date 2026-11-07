package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/rodo-software/cinderstack-runner/internal/forge"
)

// Acceptance tests of CSR-47: add checks the repository's CI secrets with the
// project's forge and git token, warns, and goes on.

func addWithCISecrets(t *testing.T, reply forge.CISecretsReply) (code int, out string, got []string) {
	t.Helper()
	u := instance(t, "WEB", repoURL)
	c, stdout, _ := newCLI(t, lines(u, goodToken, "", "git-tok"))
	c.Forge = func(kind, repository, token string) (forge.Forge, error) {
		got = []string{kind, repository, token}
		return &forge.Fake{CISecretsReplies: []forge.CISecretsReply{reply}}, nil
	}
	return c.Run([]string{"add"}), stdout.String(), got
}

func TestAddCISecretsSafe(t *testing.T) {
	code, out, got := addWithCISecrets(t, forge.CISecretsReply{})
	if code != 0 || !strings.Contains(out, "Added WEB at ") {
		t.Fatalf("code %d:\n%s", code, out)
	}
	if want := []string{"forgejo", repoURL, "git-tok"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("forge built with %q, want %q", got, want)
	}
	if strings.Contains(out, "Warning") {
		t.Errorf("warned with no exposed secrets:\n%s", out)
	}
}

func TestAddCISecretsExposedWarnsAndGoesOn(t *testing.T) {
	code, out, _ := addWithCISecrets(t, forge.CISecretsReply{Exposed: []string{"repository secret DEPLOY_KEY", "group variable NPM_TOKEN"}})
	if code != 0 || !strings.Contains(out, "Added WEB at ") {
		t.Fatalf("code %d:\n%s", code, out)
	}
	for _, s := range []string{"Warning", "repository secret DEPLOY_KEY", "group variable NPM_TOKEN", "README"} {
		if !strings.Contains(out, s) {
			t.Errorf("stdout lacks %q:\n%s", s, out)
		}
	}
}

func TestAddCISecretsUnknownWarnsAndGoesOn(t *testing.T) {
	code, out, _ := addWithCISecrets(t, forge.CISecretsReply{Err: errors.New("GET secrets: 403 forbidden")})
	if code != 0 || !strings.Contains(out, "Added WEB at ") {
		t.Fatalf("code %d:\n%s", code, out)
	}
	for _, s := range []string{"Warning", "could not", "403 forbidden", "README"} {
		if !strings.Contains(out, s) {
			t.Errorf("stdout lacks %q:\n%s", s, out)
		}
	}
}
