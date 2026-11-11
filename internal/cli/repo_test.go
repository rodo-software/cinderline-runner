package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// lsRemoteDocker is a healthy docker whose git ls-remote accepts only good,
// recording the token and argv of each.
func lsRemoteDocker(good string, seen *[]string) func(context.Context, []string, ...string) (string, error) {
	return func(ctx context.Context, env []string, argv ...string) (string, error) {
		if !slices.Contains(argv, "--entrypoint") || !strings.Contains(strings.Join(argv, " "), "ls-remote") {
			return healthyDocker(ctx, env, argv...)
		}
		*seen = append(*seen, strings.Join(env, " ")+" | "+strings.Join(argv, " "))
		if !slices.Equal(env, []string{"GIT_TOKEN=" + good}) {
			return "", errors.New("authentication failed")
		}
		return "", nil
	}
}

// add-repo re-asks a URL outside the allowlist, a bad ref, and everything
// after a token ls-remote rejects; list and remove-repo follow (CSR-160).
func TestAddListRemoveRepo(t *testing.T) {
	web := config.Project{URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"}
	c, out, errb := newCLI(t, lines(
		"https://tok@git.example.com/o/lib", // credentials in the URL
		"https://git.example.com/o/web",     // the project's own
		"https://git.example.com/o/lib",
		"a..b",
		"v1.0",
		"wrong-token",
		"https://git.example.com/o/lib",
		"v1.0",
		"good-token",
	))
	var seen []string
	c.exec = lsRemoteDocker("good-token", &seen)
	saveConfig(t, c, web)

	if code := c.Run([]string{"add-repo", "--project", "WEB"}); code != 0 {
		t.Fatalf("code %d, stderr %q, stdout %q", code, errb.String(), out.String())
	}
	got := projects(t, c)[0].ExtraRepositories
	want := []config.ExtraRepository{{URL: "https://git.example.com/o/lib", Ref: "v1.0"}}
	if !slices.Equal(got, want) {
		t.Fatalf("extra repositories %v, want %v", got, want)
	}
	if v := secret(t, c, web, secrets.ExtraGitToken(want[0].URL)); v != "good-token" {
		t.Errorf("token %q", v)
	}
	if v := secret(t, c, web, secrets.GitToken); v != "" {
		t.Errorf("project git token touched: %q", v)
	}
	if len(seen) != 2 || !strings.HasSuffix(seen[1], "https://git.example.com/o/lib refs/heads/v1.0 refs/tags/v1.0") || strings.Contains(strings.SplitN(seen[1], " | ", 2)[1], "good-token") {
		t.Errorf("ls-remote calls %q", seen)
	}
	for _, s := range []string{"without credentials", "the project's own repository", "not a plain branch or tag name", "This token cannot read", "runs read it at /extra/lib"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("stdout lacks %q:\n%s", s, out.String())
		}
	}

	out.Reset()
	if code := c.Run([]string{"list-repos", "--project", "WEB"}); code != 0 || out.String() != "lib\thttps://git.example.com/o/lib\tv1.0\n" {
		t.Errorf("list-repos: code %d, %q", code, out.String())
	}

	if code := c.Run([]string{"remove-repo", "--project", "WEB", "nope"}); code != 1 {
		t.Errorf("remove-repo nope: code %d", code)
	}
	if code := c.Run([]string{"remove-repo", "--project", "WEB", "lib"}); code != 0 {
		t.Fatalf("remove-repo: code %d, stderr %q", code, errb.String())
	}
	if got := projects(t, c)[0].ExtraRepositories; len(got) != 0 {
		t.Errorf("after remove: %v", got)
	}
	if v := secret(t, c, web, secrets.ExtraGitToken(want[0].URL)); v != "" {
		t.Errorf("token left after remove: %q", v)
	}
}

// Removing the project removes its extra repositories' tokens too.
func TestRemoveProjectDeletesExtraTokens(t *testing.T) {
	r := config.ExtraRepository{URL: "https://git.example.com/o/lib"}
	web := config.Project{URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web", ExtraRepositories: []config.ExtraRepository{r}}
	c, _, errb := newCLI(t, "")
	c.exec = func(context.Context, []string, ...string) (string, error) { return "", nil }
	saveConfig(t, c, web)
	if _, err := c.Secrets.Set(web, secrets.ExtraGitToken(r.URL), "tok"); err != nil {
		t.Fatal(err)
	}
	if code := c.Run([]string{"remove", "WEB"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if v := secret(t, c, web, secrets.ExtraGitToken(r.URL)); v != "" {
		t.Errorf("extra token left: %q", v)
	}
}
