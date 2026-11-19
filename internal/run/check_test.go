package run

import (
	"slices"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/environment"
)

// CLR-207: the check container mounts only the issue's own folder of the
// runs volume, at the clone's usual path, and the repo read-only: never the
// whole runs volume with the other issues' clones.
func TestTheCheckMountsOnlyTheIssuesClone(t *testing.T) {
	f := newFixture(t)
	f.cl.Runs = []client.RunReply{ok("s", "Done.")}
	f.env.Runs = []environment.RunReply{{}}
	app := &fakeApp{onNext: stepsOf(runNew, checkStep, `{"kind":"stop"}`)}
	if _, err := f.run(t, app); err != nil {
		t.Fatal(err)
	}
	calls := f.env.Calls()
	if len(calls) != 1 {
		t.Fatalf("environment calls = %+v, want the check's one", calls)
	}
	want := []string{
		"-v", "cinderline-CSR-repo:/repo:ro",
		"--mount", "type=volume,src=cinderline-CSR-runs,dst=/runs/CSR-1,volume-subpath=CSR-1",
	}
	if got := calls[0].Args[0].(environment.Spec).Mounts; !slices.Equal(got, want) {
		t.Errorf("check mounts %q, want %q", got, want)
	}
}
