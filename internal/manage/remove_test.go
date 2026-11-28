package manage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/run"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// withSecrets stores a runner and a git token for each of ps.
func withSecrets(t *testing.T, sec *secrets.Secrets, ps ...config.Project) {
	t.Helper()
	for _, p := range ps {
		for _, k := range []secrets.Kind{secrets.RunnerToken, secrets.GitToken} {
			if _, err := sec.Set(p, k, goodToken); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// noWarning fails the test on a warning.
func noWarning(t *testing.T) func(error) {
	return func(err error) { t.Errorf("warning: %v", err) }
}

// CLR-302: RemoveProject removes the one project its key, URL and
// workspace name from config.json, then its secrets; a namesake keeps its own.
func TestRemoveProject(t *testing.T) {
	a, b := web, web
	b.URL = "https://b.example.com"
	p, sec := addSetup(t, a, b)
	withSecrets(t, sec, a, b)

	if _, err := RemoveProject(p, sec, Project{Key: "WEB"}, noWarning(t)); !errors.As(err, new(Refused)) || !strings.Contains(err.Error(), "-url") {
		t.Errorf("a key two projects have: %v, want Refused naming -url", err)
	}
	for _, which := range []Project{{Key: "NOPE"}, {Key: "WEB", URL: "https://c.example.com"}} {
		if _, err := RemoveProject(p, sec, which, noWarning(t)); !errors.As(err, new(NotFound)) {
			t.Errorf("%+v: %v, want NotFound", which, err)
		}
	}
	if ps := loadProjects(t, p); len(ps) != 2 {
		t.Fatalf("a refused remove changed the config: %+v", ps)
	}

	got, err := RemoveProject(p, sec, Project{Key: "WEB", URL: b.URL + "/"}, noWarning(t))
	if err != nil {
		t.Fatal(err)
	}
	if ps := loadProjects(t, p); got.URL != b.URL || len(ps) != 1 || ps[0].URL != a.URL {
		t.Errorf("removed %+v, config %+v; want %s gone, the other kept", got, loadProjects(t, p), b.URL)
	}
	if stored(t, sec, b, secrets.RunnerToken) != "" || stored(t, sec, b, secrets.GitToken) != "" {
		t.Error("the removed project's secrets are still there")
	}
	if stored(t, sec, a, secrets.RunnerToken) == "" || stored(t, sec, a, secrets.GitToken) == "" {
		t.Error("the namesake's secrets are gone")
	}
}

// docker is an Exec that records its calls and answers list with its output.
type docker struct {
	calls [][]string
	list  map[string]string
}

func (d *docker) exec(_ context.Context, _ []string, argv ...string) (string, error) {
	d.calls = append(d.calls, argv)
	return d.list[strings.Join(argv, " ")], nil
}

// CLR-302: DropLocal removes the Docker containers, networks and volumes,
// the session notes and the outbox of p's ID, the Docker ones only when no
// project in config.json still has the ID.
func TestDropLocal(t *testing.T) {
	gone := web
	gone.URL = "https://b.example.com"
	for _, shared := range []bool{false, true} {
		var ps []config.Project
		if shared {
			ps = append(ps, web) // the same ID WEB at another instance
		}
		p, _ := addSetup(t, append(ps, config.Project{Workspace: "default", URL: "https://c.example.com", Key: "API", Repository: "https://git.example.com/o/api"})...)
		notes, outbox := run.PausedDir(p.State, gone), run.OutboxFile(p.State, gone)
		for _, f := range []string{filepath.Join(notes, "WEB-1.json"), outbox} {
			if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f, []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		d := &docker{list: map[string]string{"docker volume ls --quiet --filter label=cinderline.project=WEB": "cinderline-WEB-repo\n"}}
		DropLocal(context.Background(), p, d.exec, gone, noWarning(t))
		want := [][]string{
			{"docker", "ps", "--all", "--quiet", "--filter", "label=cinderline.project=WEB"},
			{"docker", "network", "ls", "--quiet", "--filter", "label=cinderline.project=WEB"},
			{"docker", "volume", "ls", "--quiet", "--filter", "label=cinderline.project=WEB"},
			{"docker", "volume", "rm", "--force", "cinderline-WEB-repo"},
		}
		if shared {
			want = nil
		}
		if !reflect.DeepEqual(d.calls, want) {
			t.Errorf("shared %v: docker calls %q, want %q", shared, d.calls, want)
		}
		for _, f := range []string{notes, outbox} {
			if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("shared %v: %s is still there: %v", shared, f, err)
			}
		}
	}
}

// CLR-302: RenameProject gives the project the key its instance gives it
// now and the ID with it, and moves its secrets; it refuses another
// new_key than the instance's and a project with a run in flight, and says
// with one ID that nothing changed.
func TestRenameProject(t *testing.T) {
	u := instance(t, "default") // says the key is WEB
	old := config.Project{LocalID: "OLD", Workspace: "default", URL: u, Key: "OLD", Repository: "https://git.example.com/o/web"}
	p, sec := addSetup(t, old)
	withSecrets(t, sec, old)
	idle := func() (localapi.Status, error) { return localapi.Status{}, nil }
	rename := func(newKey string, status func() (localapi.Status, error)) (config.Project, config.Project, error) {
		return RenameProject(context.Background(), p, sec, (&docker{}).exec, "test", status, Project{Key: "OLD"}, newKey, noWarning(t))
	}

	busy := func() (localapi.Status, error) {
		return localapi.Status{Projects: []localapi.ProjectStatus{{Project: "OLD", Issues: []string{"OLD-5"}}}}, nil
	}
	for what, err := range map[string]error{
		"other new_key": second(rename("API", idle)),
		"busy":          second(rename("", busy)),
	} {
		if !errors.As(err, new(Refused)) {
			t.Errorf("%s: %v, want Refused", what, err)
		}
		if ps := loadProjects(t, p); !reflect.DeepEqual(ps, []config.Project{old}) || stored(t, sec, old, secrets.GitToken) == "" {
			t.Errorf("%s changed the config or the secrets: %+v", what, ps)
		}
	}
	if _, _, err := RenameProject(context.Background(), p, sec, (&docker{}).exec, "test", idle, Project{Key: "NOPE"}, "", noWarning(t)); !errors.As(err, new(NotFound)) {
		t.Errorf("unknown project: %v, want NotFound", err)
	}

	before, after, err := rename("WEB", idle)
	if err != nil {
		t.Fatal(err)
	}
	want := old
	want.Key, want.LocalID = "WEB", "WEB"
	if !reflect.DeepEqual(before, old) || !reflect.DeepEqual(after, want) || !reflect.DeepEqual(loadProjects(t, p), []config.Project{want}) {
		t.Errorf("renamed %+v to %+v, config %+v; want %+v", before, after, loadProjects(t, p), want)
	}
	for _, k := range []secrets.Kind{secrets.RunnerToken, secrets.GitToken} {
		if stored(t, sec, want, k) != goodToken || stored(t, sec, old, k) != "" {
			t.Errorf("%s did not move to the new ID", k)
		}
	}

	if before, after, err := RenameProject(context.Background(), p, sec, (&docker{}).exec, "test", idle, Project{Key: "WEB"}, "", noWarning(t)); err != nil || before.ID() != after.ID() {
		t.Errorf("rename again: %+v, %+v, %v; want one ID", before, after, err)
	}
}

func second[A, B any](_ A, _ B, err error) error { return err }

// Busy refuses what may hold work the forge has not: a run in flight, a
// Project Manager run, an outbox, a paused note and a clone without a kept note.
func TestBusy(t *testing.T) {
	p, _ := addSetup(t, web)
	none := &docker{}
	for what, st := range map[string]localapi.Status{
		"WEB-1": {Projects: []localapi.ProjectStatus{{Project: "WEB", Issues: []string{"WEB-1"}}}},
		"WEB-2": {Projects: []localapi.ProjectStatus{{Project: "WEB", Manager: "WEB-2"}}},
	} {
		if err := Busy(context.Background(), p, none.exec, st, web); !errors.As(err, new(Refused)) || !strings.Contains(err.Error(), what) {
			t.Errorf("%s: %v", what, err)
		}
	}
	if err := Busy(context.Background(), p, none.exec, localapi.Status{}, web); err != nil {
		t.Errorf("idle: %v", err)
	}
	clones := func(_ context.Context, _ []string, argv ...string) (string, error) {
		if slices.Contains(argv, "run") {
			return "/runs/WEB-7\n", nil
		}
		return "", nil
	}
	if err := Busy(context.Background(), p, clones, localapi.Status{}, web); !errors.As(err, new(Refused)) || !strings.Contains(err.Error(), "WEB-7") {
		t.Errorf("a clone without a note: %v", err)
	}
}
