package client

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
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

// prepares counts the state volume preparations among the docker commands
// of an Exec, safely across goroutines; fail makes the next one fail.
type prepares struct {
	mu   sync.Mutex
	n    int
	fail bool
}

func (p *prepares) exec(_ context.Context, _ []string, argv ...string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.Contains(argv[len(argv)-1], "chown") {
		if p.fail {
			p.fail = false
			return "", errors.New("docker broke")
		}
		p.n++
	}
	return "", nil
}

func (p *prepares) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

// The state volume is prepared once for the turns of a CLI, not before each.
func TestStatePreparedOnce(t *testing.T) {
	x := &prepares{}
	c := &CLI{Name: "claude", Project: "CSR", Env: &environment.Fake{Runs: make([]environment.RunReply, 2)}, Exec: x.exec}
	for range 2 {
		if _, err := c.Run(context.Background(), Turn{Dir: "/runs/CSR-7"}); err != nil {
			t.Fatal(err)
		}
	}
	if x.count() != 1 {
		t.Errorf("prepared %d times, want 1", x.count())
	}
}

// A failed preparation is not remembered: the next turn prepares again.
func TestStatePrepareRetried(t *testing.T) {
	x := &prepares{fail: true}
	env := &environment.Fake{Runs: make([]environment.RunReply, 1)}
	c := &CLI{Name: "claude", Project: "CSR", Env: env, Exec: x.exec}
	if _, err := c.Run(context.Background(), Turn{Dir: "/runs/CSR-7"}); err == nil {
		t.Fatal("the turn ran on an unprepared state volume")
	}
	if _, err := c.Run(context.Background(), Turn{Dir: "/runs/CSR-7"}); err != nil {
		t.Fatal(err)
	}
	if x.count() != 1 || len(env.Calls()) != 1 {
		t.Errorf("prepared %d times, %d turns ran; want 1 and 1", x.count(), len(env.Calls()))
	}
}

// A failed turn may have failed on its state volume (gone, or not the agent
// user's): the next turn prepares it again, and the one after does not.
func TestStatePreparedAgainAfterFailedTurn(t *testing.T) {
	x := &prepares{}
	env := &environment.Fake{Runs: []environment.RunReply{{}, {Err: errors.New("exit status 1")}, {}, {}}}
	c := &CLI{Name: "claude", Project: "CSR", Env: env, Exec: x.exec}
	var errs []error
	for range 4 {
		_, err := c.Run(context.Background(), Turn{Dir: "/runs/CSR-7"})
		errs = append(errs, err)
	}
	if errs[0] != nil || errs[1] == nil || errs[2] != nil || errs[3] != nil {
		t.Fatalf("turn errors = %v", errs)
	}
	if x.count() != 2 {
		t.Errorf("prepared %d times, want 2", x.count())
	}
}

// Lanes run turns on one CLI in parallel (CLR-228): no data race, and they
// prepare the state volume once.
func TestStatePreparedOnceInParallel(t *testing.T) {
	const turns = 20
	x := &prepares{}
	c := &CLI{Name: "claude", Project: "CSR", Env: &environment.Fake{Runs: make([]environment.RunReply, turns)}, Exec: x.exec}
	var wg sync.WaitGroup
	for range turns {
		wg.Go(func() {
			if _, err := c.Run(context.Background(), Turn{Dir: "/runs/CSR-7"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if x.count() != 1 {
		t.Errorf("prepared %d times, want 1", x.count())
	}
}
