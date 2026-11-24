package client

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// DeleteSession searches the state volumes the clients write to
// (StateVolume), and only those that exist: mounting a missing one would
// create it.
func TestDeleteSession(t *testing.T) {
	volumes := "cinderline-CSR-state-opencode\ncinderline-CSR-state-claude\ncinderline-CSR-state-claude-old\n"
	var got [][]string
	x := func(_ context.Context, _ []string, argv ...string) (string, error) {
		got = append(got, argv)
		if argv[1] == "volume" {
			return volumes, nil
		}
		return "", nil
	}
	if err := DeleteSession(context.Background(), x, "CSR", "CSR-7", "abcd1234-uuid"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSession(context.Background(), x, "CSR", "CSR-7", ""); err != nil {
		t.Fatal(err)
	}
	ls := []string{"docker", "volume", "ls", "--quiet", "--filter", "name=cinderline-CSR-state-"}
	prefix := []string{"docker", "run", "--rm", "--network", "none",
		"-v", StateVolume("CSR", "claude") + ":/s/claude", "-v", StateVolume("CSR", "opencode") + ":/s/opencode",
		"--entrypoint", "sh", workspace.GitImage, "-c"}
	want := [][]string{
		ls,
		slices.Concat(prefix, []string{`find /s -mindepth 2 \( -name "-runs-$1" -o -name "*$2*" \) -prune -exec rm -rf -- {} +`, "sh", "CSR-7", "abcd1234-uuid"}),
		ls,
		slices.Concat(prefix, []string{`find /s -mindepth 2 -name "-runs-$1" -prune -exec rm -rf -- {} +`, "sh", "CSR-7", ""}),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv =\n%q\nwant\n%q", got, want)
	}
	// Nothing that could widen the pattern reaches find.
	for _, bad := range [][2]string{{"CSR-7", "*"}, {"CSR-7", "short"}, {"../x", "abcd1234"}, {"CSR-7", "abcd/1234"}, {"", "abcd1234"}} {
		if err := DeleteSession(context.Background(), x, "CSR", bad[0], bad[1]); err == nil {
			t.Errorf("DeleteSession(%q, %q) succeeded", bad[0], bad[1])
		}
	}
	if len(got) != 4 {
		t.Fatalf("%d docker commands, want 4", len(got))
	}
	// No state volume yet: nothing to search, and no container that would make one.
	got, volumes = nil, ""
	if err := DeleteSession(context.Background(), x, "CSR", "CSR-7", "abcd1234-uuid"); err != nil || len(got) != 1 {
		t.Errorf("without volumes: err %v, commands %q, want only the volume ls", err, got)
	}
}

// A client the runner does not know runs nothing.
func TestUnknownClient(t *testing.T) {
	c := &CLI{Name: "nope"}
	if _, err := c.Run(context.Background(), Turn{}); err == nil {
		t.Error("Run succeeded")
	}
}

// SetTools while another lane's turn runs on the same CLI (CLR-228): no data
// race, and a turn uses one whole mount, the old one or the new one.
func TestSetToolsDuringRun(t *testing.T) {
	old := tools.Mount{Args: []string{"-v", "old:/opt/cinderline:ro"}, Env: []string{"OLD=1"}}
	fresh := tools.Mount{Args: []string{"-v", "new:/opt/cinderline:ro"}, Env: []string{"NEW=1"}}
	const turns = 50
	env := &environment.Fake{Runs: make([]environment.RunReply, turns)}
	c := &CLI{
		Name: "claude", Project: "CSR", Env: env, Tools: old,
		Exec: func(context.Context, []string, ...string) (string, error) { return "", nil },
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range turns {
			if i%2 == 0 {
				c.SetTools(fresh)
			} else {
				c.SetTools(old)
			}
		}
	})
	wg.Go(func() {
		for range turns {
			_, _ = c.Run(context.Background(), Turn{Dir: "/runs/CSR-7"})
		}
	})
	wg.Wait()
	for _, call := range env.Calls() {
		s := call.Args[0].(environment.Spec)
		oldOK := slices.Contains(s.Mounts, "old:/opt/cinderline:ro") && slices.Contains(s.Env, "OLD=1")
		newOK := slices.Contains(s.Mounts, "new:/opt/cinderline:ro") && slices.Contains(s.Env, "NEW=1")
		if oldOK == newOK {
			t.Fatalf("a turn mixed the mounts: %q %q", s.Mounts, s.Env)
		}
	}
}
