package run

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// A repository that fails to fetch, or has no token, is left out and
// reported in a progress line; the others reach the prompt (CSR-160).
func TestExtraRepositories(t *testing.T) {
	api := &fakeAPI{handle: func(key, body string) (reply, bool) {
		return reply{204, ""}, key == "POST /issues/CSR-1/progress"
	}}
	c := api.client(t)
	p := project(t)
	lib := config.ExtraRepository{URL: "https://git.example/o/lib", Ref: "v1"}
	bad := config.ExtraRepository{URL: "https://git.example/o/bad"}
	untokened := config.ExtraRepository{URL: "https://git.example/o/none"}
	p.Config.ExtraRepositories = []config.ExtraRepository{lib, bad, untokened}
	for _, r := range []config.ExtraRepository{lib, bad} {
		if _, err := p.Secrets.Set(p.Config, secrets.ExtraGitToken(r.URL), "tok-"+r.Name()); err != nil {
			t.Fatal(err)
		}
	}
	var got []workspace.Extra
	p.fetchExtras = func(_ context.Context, repos []workspace.Extra) map[string]error {
		got = repos
		return map[string]error{"bad": errors.New("fetch failed")}
	}

	ok := p.extraRepositories(context.Background(), c, "CSR-1")
	if len(ok) != 1 || ok[0] != lib {
		t.Errorf("readable = %v, want only lib", ok)
	}
	if len(got) != 2 || got[0] != (workspace.Extra{Name: "lib", URL: lib.URL, Ref: "v1", Token: "tok-lib"}) || got[1].Token != "tok-bad" {
		t.Errorf("fetched %+v", got)
	}
	w := strings.Join(api.writes(), "\n")
	for _, u := range []string{bad.URL, untokened.URL} {
		if !strings.Contains(w, u+" not fetched") {
			t.Errorf("no progress for %s:\n%s", u, w)
		}
	}
	if strings.Contains(w, "lib not fetched") {
		t.Errorf("progress for lib:\n%s", w)
	}

	prompt := CodingPrompt(soloTeam, "CSR-1", "T", "Body.", "b", "CSR", ok)
	if !strings.Contains(prompt, "read-only, for reference:\n\n- `/extra/lib`: https://git.example/o/lib\n") ||
		strings.Index(prompt, "/extra/lib") > strings.Index(prompt, "## Issue") {
		t.Errorf("prompt:\n%s", prompt)
	}
}
