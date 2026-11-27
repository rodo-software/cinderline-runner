package run

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/tools"
)

// Every turn of a run mounts the tools of the run's own prepare, so new
// agent CLIs that go live meanwhile wait for the next run (CLR-317).
func TestRunKeepsItsToolsMount(t *testing.T) {
	f := newFixture(t)
	mount := tools.Mount{Args: []string{"-v", "cinderline-tools-0123456789ab:/opt/cinderline:ro"}}
	f.p.prepare = func(context.Context, cinderline.Project, string) (int64, tools.Mount, error) { return 0, mount, nil }
	f.cl.Runs = []client.RunReply{ok("s1", "Planned."), ok("w1", "Part done.")}
	app := &fakeApp{onNext: stepsOf(runNew, `{"kind":"run","issue":{"ref":"CSR-1.2","title":"Part"},"agent":"Coder","session":"new","token":null,"prompt":"Part."}`, `{"kind":"done","session":"kept"}`)}
	if started, err := f.p.run(context.Background(), app.client(t), proj, soloJob(), 0); !started || err != nil {
		t.Fatalf("run = %v, %v", started, err)
	}
	ts := turns(f.cl)
	if len(ts) != 2 {
		t.Fatalf("%d turns", len(ts))
	}
	for _, tr := range ts {
		if tr.Tools == nil || !slices.Equal(tr.Tools.Args, mount.Args) {
			t.Fatalf("turn mounts %+v, want the run's %v", tr.Tools, mount.Args)
		}
	}
}

// A new CLI fails its check when --version prints no version, or when it
// lists no models while the live one did; the listing asks only those.
func TestCheckTools(t *testing.T) {
	s := newScriptedExec()
	for _, n := range []string{"claude", "gemini", "opencode"} {
		s.add(binaryArgv(testMount, "img", n), n+" 1.2.3\n", nil)
	}
	s.add(binaryArgv(testMount, "img", "codex"), "codex-cli\n", nil) // starts, prints no version
	var asked []string
	got := checkTools(context.Background(), s.exec, "img", testMount, []string{"claude", "gemini"}, func(names []string) map[string][]string {
		asked = names
		return map[string][]string{"claude": {"claude-fable-5-2"}}
	})
	if !slices.Equal(got, []string{"codex", "gemini"}) || !slices.Equal(asked, []string{"claude", "gemini"}) {
		t.Fatalf("failed %v, listed %v; want codex and gemini, listing claude and gemini", got, asked)
	}
}

// The listing is tried for the ready clients whose live CLI listed well.
func TestProjectCheckToolsListsTheListed(t *testing.T) {
	p := &Project{
		clients: []cinderline.ReportClient{{Name: "claude", State: "ready"}, {Name: "codex", State: "ready"}, {Name: "gemini", State: "missing_credential"}},
		models:  map[string]modelList{"claude": {"claude", []string{"m"}, time.Now()}, "gemini": {"gemini", []string{"g"}, time.Now()}},
	}
	var listed []string
	p.checkTools = func(_ context.Context, _ tools.Versions, l []string) ([]string, error) { listed = l; return nil, nil }
	if _, err := p.CheckTools(context.Background(), tools.Pinned()); err != nil || !slices.Equal(listed, []string{"claude"}) {
		t.Fatalf("listed %v, %v; want claude", listed, err)
	}
}
