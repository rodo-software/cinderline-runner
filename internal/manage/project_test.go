package manage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

const gitToken = "git-token-of-the-new-project"

// addSetup is a runner with the projects ps in config.json, none for no
// file, and its secrets.
func addSetup(t *testing.T, ps ...config.Project) (config.Paths, *secrets.Secrets) {
	t.Helper()
	keyring.MockInit()
	p := paths(t)
	p.State = p.Dir
	if len(ps) > 0 {
		if err := (&config.Config{Projects: ps}).Save(p.File); err != nil {
			t.Fatal(err)
		}
	}
	return p, secrets.New(p.State)
}

// loadProjects is config.json's projects, none without the file.
func loadProjects(t *testing.T, p config.Paths) []config.Project {
	t.Helper()
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Projects
}

func addProject(p config.Paths, sec *secrets.Secrets, in NewProject) error {
	_, _, err := AddProject(context.Background(), p, sec, "test", in)
	return err
}

// noTokens fails the test when an error text holds a token or either is stored for w.
func noTokens(t *testing.T, what string, err error, sec *secrets.Secrets, w config.Project, tokens ...string) {
	t.Helper()
	for _, tok := range tokens {
		if err != nil && tok != "" && strings.Contains(err.Error(), tok) {
			t.Errorf("%s: the error holds a token: %v", what, err)
		}
	}
	if stored(t, sec, w, secrets.RunnerToken) != "" || stored(t, sec, w, secrets.GitToken) != "" {
		t.Errorf("%s: a token was stored", what)
	}
}

// CLR-301: AddProject adds the instance's project, with the repository it
// names for an empty one, and stores both tokens.
func TestAddProject(t *testing.T) {
	u := instance(t, "default")
	p, sec := addSetup(t)
	got, stores, err := AddProject(context.Background(), p, sec, "test", NewProject{URL: u + "/", RunnerToken: goodToken, GitToken: gitToken})
	if err != nil {
		t.Fatal(err)
	}
	want := config.Project{LocalID: "WEB", URL: u, Key: "WEB", Workspace: "default", Repository: "https://git.example.com/o/web"}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(load(t, p).Projects, []config.Project{want}) {
		t.Errorf("added %+v, config %+v; want %+v", got, load(t, p).Projects, want)
	}
	if stores != [2]secrets.Store{secrets.Keychain, secrets.Keychain} {
		t.Errorf("stores %v", stores)
	}
	if stored(t, sec, want, secrets.RunnerToken) != goodToken || stored(t, sec, want, secrets.GitToken) != gitToken {
		t.Error("tokens not stored")
	}

	// The same repository typed another way, next to a project at another instance.
	u2 := instance(t, "beta")
	if err := addProject(p, sec, NewProject{URL: u2, RunnerToken: goodToken, Repository: "https://GIT.example.com/o/web.git", GitToken: gitToken}); err != nil {
		t.Fatal(err)
	}
	if ps := load(t, p).Projects; len(ps) != 2 || ps[1].Repository != "https://GIT.example.com/o/web.git" || ps[1].Workspace != "beta" {
		t.Errorf("projects %+v", ps)
	}
}

// CLR-301: each refused answer is Refused with add's text, in add's order,
// and stores nothing; no error holds a token.
func TestAddProjectRefuses(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { polls++ }))
	t.Cleanup(srv.Close)
	u := instance(t, "default")
	existing := web
	existing.URL = u
	w := config.Project{URL: u, Key: "WEB", Workspace: "default"}
	for _, tt := range []struct {
		what string
		in   NewProject
		ps   []config.Project
		want string
	}{
		// Refused by the URL's own check, not by "could not be reached": the token is never sent (decision 2026-11-19-https-instance).
		{"plain http", NewProject{URL: "http://cinderline.example.com", RunnerToken: goodToken, GitToken: gitToken}, nil, `"http://cinderline.example.com" must be an absolute https URL; plain http only on localhost`},
		{"not a URL", NewProject{URL: "not a url", RunnerToken: goodToken, GitToken: gitToken}, nil, `"not a url" must be an absolute https URL`},
		// srv counts the request an empty token must not make.
		{"no runner token", NewProject{URL: srv.URL, GitToken: gitToken}, nil, "a runner token is needed"},
		{"wrong token", NewProject{URL: u, RunnerToken: "wrong-token-of-sixteen", GitToken: gitToken}, nil, "the instance rejected this runner token"},
		{"duplicate", NewProject{URL: u, RunnerToken: goodToken, GitToken: gitToken}, []config.Project{existing}, "WEB at " + u + " is already added, in workspace default; run `"},
		{"same ID elsewhere", NewProject{URL: u, RunnerToken: goodToken, GitToken: gitToken}, []config.Project{web}, "WEB at https://a.example.com already names its Docker volumes and secrets WEB"},
		{"other repository", NewProject{URL: u, RunnerToken: goodToken, Repository: "https://git.example.com/o/other", GitToken: gitToken}, nil, "https://git.example.com/o/other is not the project's repository, https://git.example.com/o/web"},
		{"repository not https", NewProject{URL: u, RunnerToken: goodToken, Repository: "ssh://git@git.example.com/o/web", GitToken: gitToken}, nil, "must be an https URL"},
		{"no git token", NewProject{URL: u, RunnerToken: goodToken}, nil, "a git token is needed"},
	} {
		p, sec := addSetup(t, tt.ps...)
		err := addProject(p, sec, tt.in)
		refused(t, tt.what, err, tt.want)
		noTokens(t, tt.what, err, sec, w, goodToken, gitToken, tt.in.RunnerToken)
		if n := len(loadProjects(t, p)); n != len(tt.ps) {
			t.Errorf("%s: %d projects in the config, want %d", tt.what, n, len(tt.ps))
		}
	}
	if polls != 0 {
		t.Errorf("%d requests to an instance before its URL and token were checked", polls)
	}
}

// CLR-301: a refused token is ErrTokenRejected, so add asks it again; an
// instance that answers anything else is refused too, so add asks the URL
// again, and its error does not hold the token it echoes.
func TestAddProjectInstanceErrors(t *testing.T) {
	p, sec := addSetup(t)
	if err := addProject(p, sec, NewProject{URL: instance(t, "default"), RunnerToken: "wrong-token-of-sixteen", GitToken: gitToken}); !errors.Is(err, ErrTokenRejected) {
		t.Errorf("wrong token: %v, want ErrTokenRejected", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"broken for ` + r.Header.Get("Authorization") + `"}`))
	}))
	t.Cleanup(srv.Close)
	err := addProject(p, sec, NewProject{URL: srv.URL, RunnerToken: goodToken, GitToken: gitToken})
	refused(t, "not an instance", err, srv.URL+" could not be reached as a cinderline instance: ")
	if errors.Is(err, ErrTokenRejected) || strings.Contains(err.Error(), goodToken) {
		t.Errorf("not an instance: %v", err)
	}
}

// CLR-301: a token that cannot be stored undoes the add: no project, no token.
func TestAddProjectUndoesAFailedStore(t *testing.T) {
	u := instance(t, "default")
	api := config.Project{Workspace: "default", URL: "https://b.example.com", Key: "API", Repository: "https://git.example.com/o/api"}
	p, sec := addSetup(t, api)
	err := addProject(p, sec, NewProject{URL: u, RunnerToken: goodToken, GitToken: strings.Repeat("g", secrets.MaxSecretBytes+1)})
	if err == nil || !strings.Contains(err.Error(), "the limit is") {
		t.Fatalf("err %v", err)
	}
	if ps := load(t, p).Projects; len(ps) != 1 || ps[0].Key != "API" {
		t.Errorf("projects %+v, want only the one before", ps)
	}
	noTokens(t, "undo", nil, sec, config.Project{URL: u, Key: "WEB", Workspace: "default"})
}
