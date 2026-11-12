package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// The menus need a terminal; tests drive the functions under them.

func TestManageNeedsTerminal(t *testing.T) {
	c, _, errb := newCLI(t, "")
	if code := c.Run([]string{"manage"}); code != 1 || !strings.Contains(errb.String(), "interactive terminal") {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if code := c.Run([]string{"manage", "x"}); code != 2 {
		t.Fatalf("manage x: code %d", code)
	}
}

func tokenByKind(t *testing.T, c *CLI, p config.Project, k secrets.Kind) token {
	t.Helper()
	for _, tk := range c.tokens(p) {
		if tk.kind == k {
			return tk
		}
	}
	t.Fatalf("no token of kind %s", k)
	return token{}
}

func TestTokensListAllKindsWithStatus(t *testing.T) {
	c, _, _ := newCLI(t, "")
	p := config.Project{URL: "https://a.example.com", Key: "WEB", ExtraRepositories: []config.ExtraRepository{{URL: "https://git.example.com/o/lib"}}}
	if _, err := c.Secrets.Set(p, secrets.RunnerToken, "secret-runner"); err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, tk := range c.tokens(p) {
		labels = append(labels, tk.label)
	}
	want := "runner token,git token,claude key,opencode key,codex key,gemini key,git token for lib"
	if got := strings.Join(labels, ","); got != want {
		t.Errorf("labels %q, want %q", got, want)
	}
	if got := len(c.tokens(p)); got != len(secrets.Kinds(p)) {
		t.Errorf("%d tokens, secrets.Kinds has %d", got, len(secrets.Kinds(p)))
	}
	if s := c.tokenStatus(p, secrets.RunnerToken); s != "set, in the keychain" {
		t.Errorf("runner token status %q", s)
	}
	if s := c.tokenStatus(p, secrets.GitToken); s != "not set" {
		t.Errorf("git token status %q", s)
	}
	keyring.MockInitWithError(errors.New("no keychain"))
	if _, err := c.Secrets.Set(p, secrets.GitToken, "secret-git"); err != nil {
		t.Fatal(err)
	}
	if s := c.tokenStatus(p, secrets.GitToken); s != "set, in the fallback file" {
		t.Errorf("git token status %q", s)
	}
}

func TestReplaceRunnerTokenChecksInstanceAndKey(t *testing.T) {
	c, out, _ := newCLI(t, "")
	p := config.Project{URL: instance(t, "WEB", repoURL), Key: "WEB"}
	tk := tokenByKind(t, c, p, secrets.RunnerToken)
	if err := c.replaceToken(p, tk, "wrong"); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("wrong token: %v", err)
	}
	if v := secret(t, c, p, secrets.RunnerToken); v != "" {
		t.Fatalf("rejected token stored: %q", v)
	}
	other := config.Project{URL: instance(t, "SHOP", repoURL), Key: "WEB"}
	if err := c.replaceToken(other, tk, goodToken); err == nil || !strings.Contains(err.Error(), "for project SHOP") {
		t.Fatalf("another project's token: %v", err)
	}
	if err := c.replaceToken(p, tk, goodToken); err != nil {
		t.Fatal(err)
	}
	if v := secret(t, c, p, secrets.RunnerToken); v != goodToken {
		t.Fatalf("runner token %q", v)
	}
	if strings.Contains(out.String(), goodToken) {
		t.Errorf("stdout shows the token: %q", out.String())
	}
}

func TestReplaceExtraTokenChecksLsRemote(t *testing.T) {
	c, _, _ := newCLI(t, "")
	var seen []string
	c.exec = lsRemoteDocker("good-token", &seen)
	r := config.ExtraRepository{URL: "https://git.example.com/o/lib", Ref: "v1"}
	p := config.Project{URL: "https://a.example.com", Key: "WEB", ExtraRepositories: []config.ExtraRepository{r}}
	tk := tokenByKind(t, c, p, secrets.ExtraGitToken(r.URL))
	if err := c.replaceToken(p, tk, "bad-token"); err == nil || !strings.Contains(err.Error(), "cannot read") {
		t.Fatalf("bad token: %v", err)
	}
	if err := c.replaceToken(p, tk, "good-token"); err != nil {
		t.Fatal(err)
	}
	if v := secret(t, c, p, tk.kind); v != "good-token" {
		t.Fatalf("token %q", v)
	}
}

func TestReplaceAndDeleteModelKey(t *testing.T) {
	c, _, _ := newCLI(t, "")
	p := config.Project{URL: "https://a.example.com", Key: "WEB"}
	tk := tokenByKind(t, c, p, secrets.OpenAIKey)
	if err := c.replaceToken(p, tk, strings.Repeat("x", secrets.MaxSecretBytes+1)); err == nil {
		t.Fatal("stored a key over the size limit")
	}
	if err := c.replaceToken(p, tk, "sk-new"); err != nil {
		t.Fatal(err)
	}
	if v := secret(t, c, p, secrets.OpenAIKey); v != "sk-new" {
		t.Fatalf("key %q", v)
	}
	if err := c.deleteToken(p, tk); err != nil {
		t.Fatal(err)
	}
	if s := c.tokenStatus(p, secrets.OpenAIKey); s != "not set" {
		t.Fatalf("after delete: %q", s)
	}
}
