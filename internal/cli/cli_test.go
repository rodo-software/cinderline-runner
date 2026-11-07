package cli

import (
	"bytes"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderstack-runner/internal/config"
	"github.com/rodo-software/cinderstack-runner/internal/forge"
	"github.com/rodo-software/cinderstack-runner/internal/name"
	"github.com/rodo-software/cinderstack-runner/internal/secrets"
)

// No test here may use t.Parallel: keyring.MockInit replaces a global provider.

const (
	goodToken = "runner-good"
	repoURL   = "https://git.example.com/owner/Repo"
)

// instance serves /api/runner/v1/project for goodToken and answers 401 to any other token.
func instance(t *testing.T, key, repo string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/runner/v1/project" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+goodToken {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"Unauthenticated."}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"key":"` + key + `","name":"Web shop","repository_url":"` + repo + `","forge":"forgejo","base_branch":"main","environment":"laravel"}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// closedURL is an http URL on a port nothing listens on.
func closedURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return "http://" + addr
}

func newCLI(t *testing.T, stdin string) (*CLI, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	keyring.MockInit()
	dir := t.TempDir()
	paths := config.Paths{Dir: dir, File: filepath.Join(dir, "config.json"), State: filepath.Join(dir, "state")}
	var out, errb bytes.Buffer
	return &CLI{
		Version: "1.2.3",
		Paths:   paths,
		Secrets: secrets.New(paths.State),
		Stdin:   strings.NewReader(stdin),
		Stdout:  &out,
		Stderr:  &errb,
		Forge: func(string, string, string) (forge.Forge, error) {
			return &forge.Fake{CISecretsReplies: []forge.CISecretsReply{{}}}, nil
		},
	}, &out, &errb
}

func lines(s ...string) string { return strings.Join(s, "\n") + "\n" }

func saveConfig(t *testing.T, c *CLI, ps ...config.Project) {
	t.Helper()
	cfg := &config.Config{Projects: ps}
	if err := cfg.Save(c.Paths.File); err != nil {
		t.Fatal(err)
	}
}

func projects(t *testing.T, c *CLI) []config.Project {
	t.Helper()
	cfg, err := config.Load(c.Paths.File)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Projects
}

func secret(t *testing.T, c *CLI, p config.Project, k secrets.Kind) string {
	t.Helper()
	v, _, err := c.Secrets.Get(p, k)
	if errors.Is(err, secrets.ErrNotFound) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDispatch(t *testing.T) {
	tests := []struct {
		args    []string
		code    int
		stdout  string
		usageOn bool
	}{
		{[]string{"version"}, 0, name.Name + " 1.2.3\n", false},
		{[]string{}, 2, "", true},
		{[]string{"nope"}, 2, "", true},
		{[]string{"version", "x"}, 2, "", true},
		{[]string{"add", "x"}, 2, "", true},
		{[]string{"remove"}, 2, "", true},
		{[]string{"remove", "-bogus", "WEB"}, 2, "", true},
		{[]string{"run", "x"}, 2, "", true},
		{[]string{"status", "x"}, 2, "", true},
		{[]string{"logs", "x"}, 2, "", true},
		{[]string{"logs", "-n", "x"}, 2, "", true},
		{[]string{"stop", "x"}, 2, "", true},
	}
	for _, tt := range tests {
		c, out, errb := newCLI(t, "")
		if code := c.Run(tt.args); code != tt.code {
			t.Errorf("%v: code %d, want %d", tt.args, code, tt.code)
		}
		if out.String() != tt.stdout {
			t.Errorf("%v: stdout %q, want %q", tt.args, out.String(), tt.stdout)
		}
		if tt.usageOn && !strings.Contains(errb.String(), "usage:") {
			t.Errorf("%v: stderr %q lacks usage:", tt.args, errb.String())
		}
	}
	c, _, errb := newCLI(t, "")
	c.Run(nil)
	for _, cmd := range []string{"add", "remove", "run", "status", "logs", "stop", "version"} {
		if !strings.Contains(errb.String(), "  "+cmd) {
			t.Errorf("usage %q does not list %s", errb.String(), cmd)
		}
	}
}

func TestMainVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main("9.9.9", []string{"version"}, strings.NewReader(""), &out, &errb); code != 0 || out.String() != name.Name+" 9.9.9\n" {
		t.Fatalf("Main = %d, %q, %q", code, out.String(), errb.String())
	}
}

func TestAddHappyPath(t *testing.T) {
	u := instance(t, "WEB", repoURL)
	c, out, errb := newCLI(t, lines(u, goodToken, "", "git-tok"))
	if code := c.Run([]string{"add"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	want := config.Project{URL: u, Key: "WEB", Repository: repoURL}
	if got := projects(t, c); !reflect.DeepEqual(got, []config.Project{want}) {
		t.Fatalf("config projects %+v, want %+v", got, want)
	}
	if v := secret(t, c, want, secrets.RunnerToken); v != goodToken {
		t.Errorf("runner token %q", v)
	}
	if v := secret(t, c, want, secrets.GitToken); v != "git-tok" {
		t.Errorf("git token %q", v)
	}
	for _, s := range []string{"WEB", "Web shop", "Stored runner token in the keychain", "Stored git token in the keychain", "Added WEB at " + u + "\n"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("stdout lacks %q:\n%s", s, out.String())
		}
	}
	if strings.Contains(out.String(), "Copy model credentials") {
		t.Error("offered to copy model credentials with no other project")
	}
}

func TestAddReasksBadURL(t *testing.T) {
	u := instance(t, "WEB", repoURL)
	c, out, errb := newCLI(t, lines("ftp://x.example.com", "not a url", u, goodToken, "", "git-tok"))
	if code := c.Run([]string{"add"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if n := strings.Count(out.String(), "URL of the "); n != 3 {
		t.Errorf("asked the URL %d times, want 3:\n%s", n, out.String())
	}
}

func TestAddReasksRejectedToken(t *testing.T) {
	u := instance(t, "WEB", repoURL)
	c, out, errb := newCLI(t, lines(u, "", "bad-token", goodToken, "", "git-tok"))
	if code := c.Run([]string{"add"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "rejected") {
		t.Errorf("stdout does not say the token was rejected:\n%s", out.String())
	}
	if n := strings.Count(out.String(), "Runner token"); n != 3 {
		t.Errorf("asked the token %d times, want 3", n)
	}
	p := config.Project{URL: u, Key: "WEB"}
	if v := secret(t, c, p, secrets.RunnerToken); v != goodToken {
		t.Errorf("runner token %q, want the good one", v)
	}
}

func TestAddUnreachableURLGoesBackToURL(t *testing.T) {
	u := instance(t, "WEB", repoURL)
	// The token typed with the unreachable URL is kept and retried with the next URL.
	c, out, errb := newCLI(t, lines(closedURL(t), goodToken, u, "", "git-tok"))
	if code := c.Run([]string{"add"}); code != 0 {
		t.Fatalf("code %d, stderr %q\n%s", code, errb.String(), out.String())
	}
	if !strings.Contains(out.String(), "could not be reached") {
		t.Errorf("stdout does not say unreachable:\n%s", out.String())
	}
	if n := strings.Count(out.String(), "Runner token"); n != 1 {
		t.Errorf("asked the token %d times, want 1", n)
	}
	if got := projects(t, c); len(got) != 1 || got[0].URL != u {
		t.Errorf("projects %+v", got)
	}
}

func TestAddRepository(t *testing.T) {
	u := instance(t, "WEB", repoURL)
	c, out, errb := newCLI(t, lines(u, goodToken,
		"https://git.example.com/owner/other", "not a url", "ssh://git@GIT.example.com:2222/owner/repo.git/", "git-tok"))
	if code := c.Run([]string{"add"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if n := strings.Count(out.String(), "not the project's repository"); n != 1 {
		t.Errorf("rejected %d times as another repository, want 1:\n%s", n, out.String())
	}
	if got := projects(t, c); len(got) != 1 || got[0].Repository != "ssh://git@GIT.example.com:2222/owner/repo.git/" {
		t.Errorf("projects %+v", got)
	}
}

func TestAddRepositoryWithoutProjectRepository(t *testing.T) {
	u := instance(t, "WEB", "")
	c, _, errb := newCLI(t, lines(u, goodToken, "", "https://any.example.com/x/y", "git-tok"))
	if code := c.Run([]string{"add"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if got := projects(t, c); len(got) != 1 || got[0].Repository != "https://any.example.com/x/y" {
		t.Errorf("projects %+v", got)
	}
}

func TestAddEOFStoresNothing(t *testing.T) {
	u := instance(t, "WEB", repoURL)
	for _, in := range []string{"", lines(u), lines(u, goodToken), lines(u, goodToken, "")} {
		c, _, errb := newCLI(t, in)
		if code := c.Run([]string{"add"}); code != 1 {
			t.Errorf("%q: code %d, want 1", in, code)
		}
		if errb.Len() == 0 {
			t.Errorf("%q: no error on stderr", in)
		}
		if got := projects(t, c); got != nil {
			t.Errorf("%q: config written: %+v", in, got)
		}
		if v := secret(t, c, config.Project{URL: u, Key: "WEB"}, secrets.RunnerToken); v != "" {
			t.Errorf("%q: runner token stored", in)
		}
	}
}

func TestAddRefusesExistingProject(t *testing.T) {
	u := instance(t, "WEB", repoURL)
	c, _, errb := newCLI(t, lines(u, goodToken, "", "git-tok"))
	existing := config.Project{URL: u, Key: "WEB", Repository: repoURL}
	saveConfig(t, c, existing)
	if code := c.Run([]string{"add"}); code != 1 {
		t.Fatalf("code %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "already added") || !strings.Contains(errb.String(), name.Name+" remove") {
		t.Errorf("stderr %q", errb.String())
	}
	if v := secret(t, c, existing, secrets.RunnerToken); v != "" {
		t.Error("runner token stored")
	}
}

func TestAddCopiesModelCredentials(t *testing.T) {
	u := instance(t, "WEB", repoURL)
	other := config.Project{URL: "https://other.example.com", Key: "API", Repository: "https://git.example.com/o/api"}
	empty := config.Project{URL: "https://other.example.com", Key: "DOCS", Repository: "https://git.example.com/o/docs"}

	c, out, errb := newCLI(t, lines(u, goodToken, "", "git-tok", "7", "x", "1"))
	saveConfig(t, c, other, empty)
	if _, err := c.Secrets.Set(other, secrets.AnthropicCredential, "sk-ant"); err != nil {
		t.Fatal(err)
	}
	if code := c.Run([]string{"add"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	for _, s := range []string{"1. API at https://other.example.com", "2. DOCS at https://other.example.com", "Copied model credentials from API"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("stdout lacks %q:\n%s", s, out.String())
		}
	}
	if n := strings.Count(out.String(), "Copy model credentials"); n != 3 {
		t.Errorf("asked %d times, want 3", n)
	}
	if v := secret(t, c, config.Project{URL: u, Key: "WEB"}, secrets.AnthropicCredential); v != "sk-ant" {
		t.Errorf("anthropic credential %q", v)
	}

	c, out, errb = newCLI(t, lines(u, goodToken, "", "git-tok", "2"))
	saveConfig(t, c, other, empty)
	if code := c.Run([]string{"add"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "DOCS at https://other.example.com holds no model credentials") {
		t.Errorf("stdout:\n%s", out.String())
	}
	if got := projects(t, c); len(got) != 3 {
		t.Errorf("projects %+v", got)
	}

	// EOF at the copy question means none; the project stays added.
	c, _, errb = newCLI(t, lines(u, goodToken, "", "git-tok"))
	saveConfig(t, c, other)
	if code := c.Run([]string{"add"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
}

func TestRemove(t *testing.T) {
	a := config.Project{URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/a"}
	b := config.Project{URL: "https://b.example.com", Key: "WEB", Repository: "https://git.example.com/o/b"}
	api := config.Project{URL: "https://a.example.com", Key: "API", Repository: "https://git.example.com/o/api"}
	kinds := []secrets.Kind{secrets.RunnerToken, secrets.GitToken, secrets.AnthropicCredential, secrets.OpenRouterKey}
	setup := func() (*CLI, *bytes.Buffer, *bytes.Buffer) {
		c, out, errb := newCLI(t, "")
		saveConfig(t, c, a, b, api)
		for _, p := range []config.Project{a, b, api} {
			for _, k := range kinds {
				if _, err := c.Secrets.Set(p, k, "v"); err != nil {
					t.Fatal(err)
				}
			}
		}
		return c, out, errb
	}

	c, out, errb := setup()
	if code := c.Run([]string{"remove", "API"}); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if out.String() != "Removed API at https://a.example.com\n" {
		t.Errorf("stdout %q", out.String())
	}
	if got := projects(t, c); !reflect.DeepEqual(got, []config.Project{a, b}) {
		t.Errorf("projects %+v", got)
	}
	for _, k := range kinds {
		if secret(t, c, api, k) != "" {
			t.Errorf("%s of API not deleted", k)
		}
		if secret(t, c, a, k) == "" {
			t.Errorf("%s of WEB at a deleted", k)
		}
	}

	c, _, errb = setup()
	if code := c.Run([]string{"remove", "NOPE"}); code != 1 || !strings.Contains(errb.String(), "NOPE") {
		t.Errorf("unknown key: code %d, stderr %q", code, errb.String())
	}

	c, _, errb = setup()
	if code := c.Run([]string{"remove", "WEB"}); code != 1 || !strings.Contains(errb.String(), "-url") {
		t.Errorf("ambiguous key: code %d, stderr %q", code, errb.String())
	}
	if len(projects(t, c)) != 3 {
		t.Error("ambiguous remove changed the config")
	}

	c, _, errb = setup()
	if code := c.Run([]string{"remove", "-url", "https://b.example.com/", "WEB"}); code != 0 {
		t.Fatalf("with -url: code %d, stderr %q", code, errb.String())
	}
	if got := projects(t, c); !reflect.DeepEqual(got, []config.Project{a, api}) {
		t.Errorf("projects %+v", got)
	}
	if secret(t, c, b, secrets.RunnerToken) != "" || secret(t, c, a, secrets.RunnerToken) == "" {
		t.Error("removed the wrong project's secrets")
	}

	c, _, errb = setup()
	if code := c.Run([]string{"remove", "-url", "https://c.example.com", "WEB"}); code != 1 {
		t.Errorf("unknown url: code %d, stderr %q", code, errb.String())
	}
}
