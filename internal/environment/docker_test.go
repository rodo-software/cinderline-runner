package environment

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	defaultimage "github.com/rodo-software/cinderstack-runner/images/default"
)

// execCall is one recorded call through the Exec seam.
type execCall struct {
	Env  []string
	Argv []string
}

// scripted returns a Docker whose Exec records every call and answers the
// i-th with outs[i] and errs[i].
func scripted(outs []string, errs map[int]error) (*Docker, *[]execCall) {
	var calls []execCall
	d := &Docker{
		Project: "CSR",
		Exec: func(_ context.Context, env []string, argv ...string) (string, error) {
			calls = append(calls, execCall{env, argv})
			i := len(calls) - 1
			out := ""
			if i < len(outs) {
				out = outs[i]
			}
			return out, errs[i]
		},
	}
	return d, &calls
}

func TestImageExists(t *testing.T) {
	d, calls := scripted(nil, nil)
	tag, err := d.Image(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tag, "cinderstack-default:") || len(tag) != len("cinderstack-default:")+12 {
		t.Errorf("tag = %q, want cinderstack-default:<12 hex of the Dockerfile>", tag)
	}
	want := []execCall{{nil, []string{"docker", "image", "inspect", "--format", "{{.Id}}", tag}}}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls =\n%q\nwant\n%q", *calls, want)
	}
}

func TestImageBuiltWhenMissing(t *testing.T) {
	var built []byte
	d, calls := scripted(nil, map[int]error{0: errors.New("no such image")})
	record := d.Exec
	d.Exec = func(ctx context.Context, env []string, argv ...string) (string, error) {
		if argv[1] == "build" {
			// The context is a temporary folder that holds only the Dockerfile.
			b, err := os.ReadFile(filepath.Join(argv[len(argv)-1], "Dockerfile"))
			if err != nil {
				t.Error(err)
			}
			built = b
		}
		return record(ctx, env, argv...)
	}
	tag, err := d.Image(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 {
		t.Fatalf("calls = %q, want inspect and build", *calls)
	}
	build := (*calls)[1].Argv
	if want := []string{"docker", "build", "--quiet", "--tag", tag}; !reflect.DeepEqual(build[:len(want)], want) || len(build) != len(want)+1 {
		t.Errorf("build argv = %q, want %q <dir>", build, want)
	}
	if !bytes.Equal(built, defaultimage.Dockerfile) {
		t.Errorf("built %q, want the embedded Dockerfile", built)
	}
	if _, err := os.Stat(build[len(build)-1]); !os.IsNotExist(err) {
		t.Errorf("build folder left behind: %v", err)
	}
}

func TestDefaultDockerfile(t *testing.T) {
	s := string(defaultimage.Dockerfile)
	for _, want := range []string{"php:8.4", "composer", "setup_22.x", " git ", "pdo_mysql", "--uid 1000", "USER runner"} {
		if !strings.Contains(s, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
	for _, cli := range []string{"claude-code", "opencode"} {
		if strings.Contains(s, cli) {
			t.Errorf("Dockerfile installs %s; agent CLIs come from the tools volume", cli)
		}
	}
}

var issueLabels = []string{"--label", "cinderstack.project=CSR", "--label", "cinderstack.issue=CSR-31"}

func TestUp(t *testing.T) {
	d, calls := scripted(nil, nil)
	if err := d.up(context.Background(), "CSR-31"); err != nil {
		t.Fatal(err)
	}
	want := []execCall{
		{nil, append(append([]string{"docker", "network", "create"}, issueLabels...), "cinderstack-CSR-CSR-31")},
		{nil, append(append([]string{"docker", "run", "--detach", "--rm"}, issueLabels...),
			"--name", "cinderstack-CSR-CSR-31-mysql", "--network", "cinderstack-CSR-CSR-31", "--network-alias", "mysql",
			"-e", "MYSQL_DATABASE=laravel_testing", "-e", "MYSQL_USER=laravel", "-e", "MYSQL_PASSWORD=laravel", "-e", "MYSQL_ROOT_PASSWORD=root",
			MySQLImage)},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls =\n%q\nwant\n%q", *calls, want)
	}
	if !strings.Contains(MySQLImage, "mysql:8.0") || !strings.Contains(MySQLImage, "@sha256:") {
		t.Errorf("MySQLImage %q is not MySQL 8 pinned by digest", MySQLImage)
	}
}

// The variables today's compose file sets, so Laravel's tests reach the run's MySQL.
func TestDBEnv(t *testing.T) {
	want := []string{"DB_HOST=mysql", "DB_PORT=3306", "DB_DATABASE=laravel_testing", "DB_USERNAME=laravel", "DB_PASSWORD=laravel"}
	if !reflect.DeepEqual(DBEnv, want) {
		t.Errorf("DBEnv = %q, want %q", DBEnv, want)
	}
}

func TestUpFailureLeavesNothing(t *testing.T) {
	boom := errors.New("boom")
	d, calls := scripted(nil, map[int]error{1: boom})
	if err := d.up(context.Background(), "CSR-31"); !errors.Is(err, boom) {
		t.Errorf("up = %v, want boom", err)
	}
	if got := (*calls)[len(*calls)-1].Argv; !reflect.DeepEqual(got, []string{"docker", "network", "rm", "cinderstack-CSR-CSR-31"}) {
		t.Errorf("last call = %q, want the network removed", got)
	}
}

func TestDownRemovesBothEvenAfterAFailure(t *testing.T) {
	boom := errors.New("boom")
	d, calls := scripted(nil, map[int]error{0: boom})
	if err := d.down(context.Background(), "CSR-31"); !errors.Is(err, boom) {
		t.Errorf("down = %v, want boom", err)
	}
	want := []execCall{
		// -v removes MySQL's anonymous data volume with it.
		{nil, []string{"docker", "rm", "--force", "--volumes", "cinderstack-CSR-CSR-31-mysql"}},
		{nil, []string{"docker", "network", "rm", "cinderstack-CSR-CSR-31"}},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls =\n%q\nwant\n%q", *calls, want)
	}
}

func TestCleanupRemovesStaleContainersAndNetworks(t *testing.T) {
	d, calls := scripted([]string{"c1\nc2\n", "", "n1\n", ""}, nil)
	if err := d.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []execCall{
		{nil, []string{"docker", "ps", "--all", "--quiet", "--filter", "label=cinderstack.project=CSR"}},
		{nil, []string{"docker", "rm", "--force", "--volumes", "c1", "c2"}},
		{nil, []string{"docker", "network", "ls", "--quiet", "--filter", "label=cinderstack.project=CSR"}},
		{nil, []string{"docker", "network", "rm", "n1"}},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls =\n%q\nwant\n%q", *calls, want)
	}
}

// Volumes are left alone: they hold the clones of paused runs and sessions.
func TestCleanupWithNothingStale(t *testing.T) {
	d, calls := scripted(nil, nil)
	if err := d.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 {
		t.Errorf("calls = %q, want only the two lists", *calls)
	}
}

func TestCleanupReportsAFailedList(t *testing.T) {
	boom := errors.New("boom")
	d, calls := scripted(nil, map[int]error{0: boom})
	if err := d.Cleanup(context.Background()); !errors.Is(err, boom) {
		t.Errorf("Cleanup = %v, want boom", err)
	}
	if len(*calls) != 2 {
		t.Errorf("calls = %q, want the networks still listed", *calls)
	}
}

// Ported from lib.test.mjs "tail keeps the last lines and caps the length".
func TestTail(t *testing.T) {
	if got := tail("a\nb\nc\n", 2, 4000); got != "b\nc" {
		t.Errorf("tail = %q", got)
	}
	if got := tail(strings.Repeat("x", 50), 1, 10); got != "…"+strings.Repeat("x", 10) {
		t.Errorf("tail = %q", got)
	}
	if got := tail("ééé", 1, 2); got != "…éé" {
		t.Errorf("tail = %q, want whole characters", got)
	}
}
