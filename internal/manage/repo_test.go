package manage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// The keyring mock replaces a global provider: no test here may use t.Parallel.

const goodToken = "good-token"

var (
	web = config.Project{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"}
	lib = config.ExtraRepository{URL: "https://git.example.com/o/lib", Ref: "v1.0"}
)

// lsRemote is a git check that accepts only goodToken, counting its calls;
// its refusal echoes the token it got, as a careless git could.
func lsRemote(calls *int) func(context.Context, []string, ...string) (string, error) {
	return func(_ context.Context, env []string, _ ...string) (string, error) {
		*calls++
		if !slices.Equal(env, []string{"GIT_TOKEN=" + goodToken}) {
			return "", errors.New("authentication failed for " + strings.Join(env, " "))
		}
		return "", nil
	}
}

// repoSetup is a config.json with web, its secrets and a git check.
func repoSetup(t *testing.T, extra ...config.ExtraRepository) (config.Paths, *secrets.Secrets, *int) {
	t.Helper()
	keyring.MockInit()
	p := paths(t)
	p.State = p.Dir
	w := web
	w.ExtraRepositories = extra
	if err := (&config.Config{Projects: []config.Project{w}}).Save(p.File); err != nil {
		t.Fatal(err)
	}
	return p, secrets.New(p.State), new(int)
}

func addRepo(p config.Paths, sec *secrets.Secrets, calls *int, project string, r config.ExtraRepository, token string) error {
	_, err := AddRepo(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), p, sec, lsRemote(calls), project, r, token)
	return err
}

func extras(t *testing.T, p config.Paths) []config.ExtraRepository {
	t.Helper()
	cfg, err := config.Load(p.File)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Projects[0].ExtraRepositories
}

// CLR-298: AddRepo stores the repository in config.json and its token in
// the secrets; RemoveRepo, by name or URL, removes both.
func TestAddAndRemoveRepo(t *testing.T) {
	for _, by := range []string{"lib", lib.URL} {
		p, sec, calls := repoSetup(t)
		if err := addRepo(p, sec, calls, "WEB", lib, goodToken); err != nil {
			t.Fatal(err)
		}
		if got := extras(t, p); !slices.Equal(got, []config.ExtraRepository{lib}) || *calls != 1 {
			t.Fatalf("extra repositories %v after %d git checks", got, *calls)
		}
		if v, _, err := sec.Get(web, secrets.ExtraGitToken(lib.URL)); v != goodToken {
			t.Fatalf("token %q, %v", v, err)
		}
		if r, err := RemoveRepo(p, sec, "WEB", by); err != nil || r != lib {
			t.Fatalf("RemoveRepo = %v, %v", r, err)
		}
		if got := extras(t, p); len(got) != 0 {
			t.Errorf("remove by %s: %v left", by, got)
		}
		if _, _, err := sec.Get(web, secrets.ExtraGitToken(lib.URL)); !errors.Is(err, secrets.ErrNotFound) {
			t.Errorf("remove by %s: token left (%v)", by, err)
		}
	}
}

// Each refusal is Refused, stores nothing, and comes before the git check
// unless it is the git check's; no error holds the token.
func TestAddRepoRefuses(t *testing.T) {
	for what, c := range map[string]struct {
		project string
		repo    config.ExtraRepository
		token   string
		want    string
		checked int
	}{
		"unknown project":   {"NOPE", lib, goodToken, "no project NOPE", 0},
		"credentials":       {"WEB", config.ExtraRepository{URL: "https://tok@git.example.com/o/lib"}, goodToken, "without a user name or password", 0},
		"not https":         {"WEB", config.ExtraRepository{URL: "http://git.example.com/o/lib"}, goodToken, "must be an https URL", 0},
		"bad folder name":   {"WEB", config.ExtraRepository{URL: "https://git.example.com/o/-lib"}, goodToken, "cannot be a folder name", 0},
		"own repository":    {"WEB", config.ExtraRepository{URL: "https://git.example.com/o/web.git"}, goodToken, "the project's own repository", 0},
		"already added":     {"WEB", config.ExtraRepository{URL: "https://other.example.com/x/dep"}, goodToken, "already added as dep", 0},
		"bad ref":           {"WEB", config.ExtraRepository{URL: lib.URL, Ref: "a..b"}, goodToken, "not a plain branch or tag name", 0},
		"no token":          {"WEB", lib, "", "a git token is needed", 0},
		"token cannot read": {"WEB", lib, "wrong-token-of-some-length", "this token cannot read https://git.example.com/o/lib at v1.0", 1},
	} {
		dep := config.ExtraRepository{URL: "https://git.example.com/o/dep"}
		p, sec, calls := repoSetup(t, dep)
		err := addRepo(p, sec, calls, c.project, c.repo, c.token)
		if r := (Refused{}); !errors.As(err, &r) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want Refused containing %q", what, err, c.want)
		}
		if c.token != "" && strings.Contains(err.Error(), c.token) {
			t.Errorf("%s: the error holds the token: %v", what, err)
		}
		if *calls != c.checked {
			t.Errorf("%s: %d git checks, want %d", what, *calls, c.checked)
		}
		if got := extras(t, p); !slices.Equal(got, []config.ExtraRepository{dep}) {
			t.Errorf("%s: extra repositories %v", what, got)
		}
		if _, _, err := sec.Get(web, secrets.ExtraGitToken(c.repo.URL)); !errors.Is(err, secrets.ErrNotFound) {
			t.Errorf("%s: a token was stored (%v)", what, err)
		}
	}
}

// RemoveRepo refuses a repository or project it does not know, and leaves
// the rest.
func TestRemoveRepoRefuses(t *testing.T) {
	p, sec, _ := repoSetup(t, lib)
	if _, err := sec.Set(web, secrets.ExtraGitToken(lib.URL), goodToken); err != nil {
		t.Fatal(err)
	}
	for project, repo := range map[string]string{"WEB": "nope", "NOPE": "lib"} {
		_, err := RemoveRepo(p, sec, project, repo)
		if r := (Refused{}); !errors.As(err, &r) {
			t.Errorf("%s %s: %v, want Refused", project, repo, err)
		}
	}
	if got := extras(t, p); !slices.Equal(got, []config.ExtraRepository{lib}) {
		t.Errorf("extra repositories %v", got)
	}
	if v, _, _ := sec.Get(web, secrets.ExtraGitToken(lib.URL)); v != goodToken {
		t.Errorf("token %q", v)
	}
}
