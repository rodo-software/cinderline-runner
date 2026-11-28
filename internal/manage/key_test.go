package manage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// instance is a Cinderline instance whose poll accepts only goodToken, as
// the runner token of project WEB in workspace ws; its refusal echoes the
// token it got, as a careless app could.
func instance(t *testing.T, ws string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); tok != goodToken {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"bad token ` + tok + `"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"poll_after":30,"project":{"key":"WEB","name":"Web","repository_url":"https://git.example.com/o/web","forge":"forgejo","base_branch":"main","environment":"default","workspace":"` + ws + `"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// keySetup is a config.json with web, at instance u, with an extra
// repository, and its secrets.
func keySetup(t *testing.T, u string) (config.Paths, *secrets.Secrets, config.Project) {
	t.Helper()
	keyring.MockInit()
	p := paths(t)
	p.State = p.Dir
	w := web
	w.URL, w.ExtraRepositories = u, []config.ExtraRepository{lib}
	if err := (&config.Config{Projects: []config.Project{w}, Providers: []config.Provider{lan}}).Save(p.File); err != nil {
		t.Fatal(err)
	}
	return p, secrets.New(p.State), w
}

// setKey is SetKey on the project with ID project, the zero one for "".
func setKey(p config.Paths, sec *secrets.Secrets, project, kind, value string) error {
	var pr config.Project
	if project != "" {
		var err error
		if pr, err = FindProject(p, project); err != nil {
			return err
		}
	}
	_, err := SetKey(context.Background(), p, sec, "test", pr, kind, value)
	return err
}

func stored(t *testing.T, sec *secrets.Secrets, p config.Project, k secrets.Kind) string {
	t.Helper()
	v, _, err := sec.Get(p, k)
	if err != nil && !errors.Is(err, secrets.ErrNotFound) {
		t.Fatal(err)
	}
	return v
}

// CLR-300: SetKey stores each kind a project has, and a provider's key
// without a project; ListKeys says which are set; DeleteKey deletes them.
func TestSetListDeleteKey(t *testing.T) {
	p, sec, w := keySetup(t, instance(t, "default"))
	kinds := map[string]secrets.Kind{
		"runner-token": secrets.RunnerToken, "git-token": secrets.GitToken,
		"claude": secrets.AnthropicCredential, "opencode": secrets.OpenRouterKey, "codex": secrets.OpenAIKey, "gemini": secrets.GeminiKey,
	}
	list := func() map[string]bool {
		t.Helper()
		ks, err := ListKeys(sec, w)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]bool{}
		for _, k := range ks {
			m[k.Kind] = k.Set
		}
		return m
	}
	want := map[string]bool{"git-token:" + lib.URL: false}
	for n := range kinds {
		want[n] = false
	}
	if got := list(); len(got) != len(want) {
		t.Fatalf("list %v, want %v", got, want)
	}
	for n, k := range kinds {
		v := "value-of-" + n
		if n == "runner-token" {
			v = goodToken
		}
		if err := setKey(p, sec, w.ID(), n, v); err != nil {
			t.Fatalf("set %s: %v", n, err)
		}
		if got := stored(t, sec, w, k); got != v {
			t.Errorf("%s: %s = %q, want %q", n, k, got, v)
		}
		want[n] = true
		if got := list(); got[n] != true || len(got) != len(want) {
			t.Errorf("after set %s: list %v", n, got)
		}
	}
	if _, err := sec.Set(w, secrets.ExtraGitToken(lib.URL), "lib-token"); err != nil {
		t.Fatal(err)
	}
	if got := list(); !got["git-token:"+lib.URL] {
		t.Errorf("list %v, want the extra repository's token set", got)
	}
	for n, k := range kinds {
		if err := DeleteKey(p, sec, w, n); err != nil {
			t.Fatalf("delete %s: %v", n, err)
		}
		if got := stored(t, sec, w, k); got != "" {
			t.Errorf("%s left after delete: %q", n, got)
		}
		if got := list(); got[n] {
			t.Errorf("after delete %s: list %v", n, got)
		}
	}

	if err := setKey(p, sec, "", "provider:lan", providerKey); err != nil || keyOf(t, sec, "lan") != providerKey {
		t.Fatalf("set provider:lan: %v, key %q", err, keyOf(t, sec, "lan"))
	}
	if err := DeleteKey(p, sec, config.Project{}, "provider:lan"); err != nil || keyOf(t, sec, "lan") != "" {
		t.Fatalf("delete provider:lan: %v, key %q", err, keyOf(t, sec, "lan"))
	}
}

// Each refusal is Refused, stores nothing and holds no value.
func TestSetKeyRefuses(t *testing.T) {
	const v = "the-secret-value"
	for what, c := range map[string]struct{ ws, project, kind, value, want string }{
		"unknown kind":           {"default", "WEB", "aider", v, `unknown key "aider"`},
		"an extra repository's":  {"default", "WEB", "git-token:" + lib.URL, v, "unknown key"},
		"unknown project":        {"default", "NOPE", "claude", v, "no project NOPE"},
		"empty value":            {"default", "WEB", "claude", "", "empty value"},
		"rejected runner token":  {"default", "WEB", "runner-token", v, "rejected this runner token"},
		"another workspace's":    {"beta", "WEB", "runner-token", goodToken, "workspace beta"},
		"provider with project":  {"default", "WEB", "provider:lan", v, "belongs to no project"},
		"unknown provider":       {"default", "", "provider:nope", v, "no provider nope"},
		"empty provider key":     {"default", "", "provider:lan", "", "empty value"},
		"no project for a model": {"default", "", "claude", v, "needs a project"},
	} {
		p, sec, w := keySetup(t, instance(t, c.ws))
		err := setKey(p, sec, c.project, c.kind, c.value)
		if !errors.As(err, new(Refused)) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want Refused containing %q", what, err, c.want)
			continue
		}
		if strings.Contains(err.Error(), c.value) && c.value != "" {
			t.Errorf("%s: the error holds the value: %v", what, err)
		}
		for _, k := range secrets.Kinds(w) {
			if got := stored(t, sec, w, k); got != "" {
				t.Errorf("%s: stored %s %q", what, k, got)
			}
		}
		if keyOf(t, sec, "lan") != "" {
			t.Errorf("%s: stored a provider key", what)
		}
	}
	for what, c := range map[string]struct {
		p          config.Project
		kind, want string
	}{
		"unknown kind":          {web, "aider", "unknown key"},
		"no project":            {config.Project{}, "claude", "needs a project"},
		"unknown provider":      {config.Project{}, "provider:nope", "no provider nope"},
		"provider with project": {web, "provider:lan", "belongs to no project"},
	} {
		p, sec, _ := keySetup(t, "https://a.example.com")
		if err := DeleteKey(p, sec, c.p, c.kind); !errors.As(err, new(Refused)) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("delete, %s: %v, want Refused containing %q", what, err, c.want)
		}
	}
	p, _, _ := keySetup(t, "https://a.example.com")
	if _, err := FindProject(p, "NOPE"); !errors.As(err, new(Refused)) {
		t.Errorf("an unknown project: %v, want Refused", err)
	}
}

// An instance that cannot be reached is the runner's failure, not a
// refusal, and its error does not hold the token either.
func TestSetKeyUnreachableInstance(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	p, sec, w := keySetup(t, u)
	err := setKey(p, sec, w.ID(), "runner-token", goodToken)
	if err == nil || errors.As(err, new(Refused)) || !strings.Contains(err.Error(), "could not be reached") || strings.Contains(err.Error(), goodToken) {
		t.Errorf("unreachable: %v", err)
	}
	if stored(t, sec, w, secrets.RunnerToken) != "" {
		t.Error("stored a token the instance did not check")
	}
}
