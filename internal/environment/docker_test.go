package environment

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	defaultimage "github.com/rodo-software/cinderline-runner/images/default"
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
	if !strings.HasPrefix(tag, "cinderline-default:") || len(tag) != len("cinderline-default:")+12 {
		t.Errorf("tag = %q, want cinderline-default:<12 hex of the Dockerfile>", tag)
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
	for _, want := range []string{"php:8.4", "composer", "setup_22.x", " git ", "pdo_mysql", "golang:1.27.3", "staticcheck@", "govulncheck@", "GOTOOLCHAIN=local", "--uid 1000", "USER runner"} {
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

var issueLabels = []string{"--label", "cinderline.project=CSR", "--label", "cinderline.issue=CSR-31"}

func TestUp(t *testing.T) {
	d, calls := scripted(nil, nil)
	if err := d.up(context.Background(), "CSR-31"); err != nil {
		t.Fatal(err)
	}
	want := []execCall{
		{nil, append(append([]string{"docker", "network", "create"}, issueLabels...), "cinderline-CSR-CSR-31")},
		{nil, append(append([]string{"docker", "run", "--detach"}, issueLabels...),
			"--name", "cinderline-CSR-CSR-31-mysql", "--network", "cinderline-CSR-CSR-31", "--network-alias", "mysql",
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
	if got := (*calls)[len(*calls)-1].Argv; !reflect.DeepEqual(got, []string{"docker", "network", "rm", "cinderline-CSR-CSR-31"}) {
		t.Errorf("last call = %q, want the network removed", got)
	}
}

func TestDownRemovesAllEvenAfterAFailure(t *testing.T) {
	boom := errors.New("boom")
	d, calls := scripted(nil, map[int]error{0: boom})
	if err := d.down(context.Background(), "CSR-31"); !errors.Is(err, boom) {
		t.Errorf("down = %v, want boom", err)
	}
	want := []execCall{
		// -v removes MySQL's anonymous data volume with it.
		{nil, []string{"docker", "rm", "--force", "--volumes", "cinderline-CSR-CSR-31-agent", "cinderline-CSR-CSR-31-mysql"}},
		{nil, []string{"docker", "network", "rm", "cinderline-CSR-CSR-31"}},
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
		{nil, []string{"docker", "ps", "--all", "--quiet", "--filter", "label=cinderline.project=CSR"}},
		{nil, []string{"docker", "rm", "--force", "--volumes", "c1", "c2"}},
		{nil, []string{"docker", "network", "ls", "--quiet", "--filter", "label=cinderline.project=CSR"}},
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

// runner returns a Docker whose Exec and Stream record every call. stream
// plays the agent container: it gets the output writer.
func runner(fail map[string]error, stream func(ctx context.Context, out io.Writer) error) (*Docker, func() []execCall, *bytes.Buffer) {
	var (
		mu    sync.Mutex
		calls []execCall
	)
	record := func(env []string, argv []string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, execCall{env, argv})
	}
	var logs bytes.Buffer
	d := &Docker{
		Project: "CSR",
		Log:     slog.New(slog.NewTextHandler(&logs, nil)),
		Exec: func(ctx context.Context, env []string, argv ...string) (string, error) {
			record(env, argv)
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", fail[strings.Join(argv[:3], " ")]
		},
		Stream: func(ctx context.Context, env []string, stdin string, out io.Writer, argv ...string) error {
			record(env, append(argv, "<stdin:"+stdin+">"))
			return stream(ctx, out)
		},
	}
	return d, func() []execCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]execCall(nil), calls...)
	}, &logs
}

const secret = "sk-ant-secret"

var turn = Spec{
	Issue:  "CSR-31",
	Dir:    "/runs/CSR-31",
	Mounts: []string{"-v", "cinderline-CSR-runs:/runs"},
	Env:    []string{"ANTHROPIC_API_KEY=" + secret, "CINDERLINE_TOKEN=tok=en"},
	Cmd:    []string{"claude", "-p"},
	Stdin:  "the prompt",
}

func TestRun(t *testing.T) {
	d, calls, logs := runner(nil, func(_ context.Context, out io.Writer) error {
		io.WriteString(out, "first line\nsecond ")
		io.WriteString(out, "line\nno newline")
		return nil
	})
	out, err := d.Run(context.Background(), turn)
	if err != nil {
		t.Fatal(err)
	}
	if out != "first line\nsecond line\nno newline" {
		t.Errorf("output = %q", out)
	}
	image := imageTag()
	agent := append(append([]string{"docker", "run", "--rm", "--interactive"}, issueLabels...),
		"--name", "cinderline-CSR-CSR-31-agent", "--network", "cinderline-CSR-CSR-31",
		"--add-host", "host.docker.internal:host-gateway", "--user", "1000:1000", "--workdir", "/runs/CSR-31",
		"-e", "DB_HOST=mysql", "-e", "DB_PORT=3306", "-e", "DB_DATABASE=laravel_testing", "-e", "DB_USERNAME=laravel", "-e", "DB_PASSWORD=laravel",
		// Secrets by name only: their values travel in the docker command's environment.
		"-e", "ANTHROPIC_API_KEY", "-e", "CINDERLINE_TOKEN",
		"-v", "cinderline-CSR-runs:/runs", image, "claude", "-p", "<stdin:the prompt>")
	got := calls()
	if len(got) != 6 {
		t.Fatalf("calls =\n%q", got)
	}
	if want := (execCall{turn.Env, agent}); !reflect.DeepEqual(got[3], want) {
		t.Errorf("agent call =\n%q\nwant\n%q", got[3], want)
	}
	wantAfter := []execCall{
		{nil, []string{"docker", "rm", "--force", "--volumes", "cinderline-CSR-CSR-31-agent", "cinderline-CSR-CSR-31-mysql"}},
		{nil, []string{"docker", "network", "rm", "cinderline-CSR-CSR-31"}},
	}
	if !reflect.DeepEqual(got[0].Argv[:3], []string{"docker", "image", "inspect"}) || got[1].Argv[1] != "network" || got[2].Argv[1] != "run" {
		t.Errorf("before the agent: %q, want image, network, MySQL", got[:3])
	}
	if !reflect.DeepEqual(got[4:6], wantAfter) {
		t.Errorf("after the agent:\n%q\nwant\n%q", got[4:6], wantAfter)
	}
	for _, c := range got {
		if strings.Contains(strings.Join(c.Argv, " "), secret) {
			t.Errorf("secret in argv %q", c.Argv)
		}
	}
	for _, line := range []string{`line="first line"`, `line="second line"`, `line="no newline"`} {
		if !strings.Contains(logs.String(), line) || !strings.Contains(logs.String(), "issue=CSR-31") {
			t.Errorf("log lacks %s with the issue:\n%s", line, logs)
		}
	}
}

func imageTag() string {
	d, _ := scripted(nil, nil)
	tag, _ := d.Image(context.Background())
	return tag
}

func TestRunStopsAtTheTimeout(t *testing.T) {
	stopped := make(chan struct{})
	d, calls, _ := runner(nil, func(_ context.Context, out io.Writer) error {
		io.WriteString(out, "working\n")
		<-stopped
		return exitErr(137)
	})
	record := d.Exec
	d.Exec = func(ctx context.Context, env []string, argv ...string) (string, error) {
		if argv[1] == "stop" {
			close(stopped)
		}
		return record(ctx, env, argv...)
	}
	s := turn
	s.Timeout = 10 * time.Millisecond
	out, err := d.Run(context.Background(), s)
	if !errors.Is(err, ErrTimeout) || out != "working\n" {
		t.Errorf("Run = %q, %v; want the output and ErrTimeout", out, err)
	}
	var stop []string
	for _, c := range calls() {
		if c.Argv[1] == "stop" {
			stop = c.Argv
		}
	}
	if !reflect.DeepEqual(stop, []string{"docker", "stop", "cinderline-CSR-CSR-31-agent"}) {
		t.Errorf("stop = %q", stop)
	}
}

// exitErr is a command that ran and exited with that code, as *exec.ExitError.
type exitErr int

func (e exitErr) Error() string { return "exit status" }
func (e exitErr) ExitCode() int { return int(e) }

func TestRunFailedCommandStillTearsDown(t *testing.T) {
	d, calls, _ := runner(nil, func(context.Context, io.Writer) error { return exitErr(2) })
	_, err := d.Run(context.Background(), turn)
	var exit interface{ ExitCode() int }
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || errors.Is(err, ErrTimeout) {
		t.Errorf("Run = %v, want exit code 2", err)
	}
	if got := calls(); len(got) != 6 || got[5].Argv[1] != "network" {
		t.Errorf("calls = %q, want the teardown", got)
	}
}

// A stop of the runner kills the docker command but not its container, so
// the teardown removes it, with a context that is not cancelled.
func TestRunCancelledStillTearsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d, calls, _ := runner(nil, func(ctx context.Context, _ io.Writer) error {
		cancel()
		return ctx.Err()
	})
	if _, err := d.Run(ctx, turn); !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want Canceled", err)
	}
	got := calls()
	if len(got) != 6 || !reflect.DeepEqual(got[4].Argv[:2], []string{"docker", "rm"}) {
		t.Errorf("calls = %q, want the teardown", got)
	}
}

func TestRunWithoutServicesRunsNoAgent(t *testing.T) {
	boom := errors.New("boom")
	d, calls, _ := runner(map[string]error{"docker network create": boom}, func(context.Context, io.Writer) error {
		t.Error("agent started without its network")
		return nil
	})
	if _, err := d.Run(context.Background(), turn); !errors.Is(err, boom) {
		t.Errorf("Run = %v, want boom", err)
	}
	if got := calls(); len(got) != 2 {
		t.Errorf("calls = %q, want inspect and network create only", got)
	}
}

func TestRunRefusesBadSpecs(t *testing.T) {
	for _, s := range []Spec{
		{Issue: "../x", Cmd: []string{"claude"}},
		{Issue: "CSR-31"},
		{Issue: "CSR-31", Cmd: []string{"claude"}, Env: []string{"NOVALUE"}},
	} {
		d, calls, _ := runner(nil, nil)
		if _, err := d.Run(context.Background(), s); err == nil {
			t.Errorf("Run(%+v) = nil, want an error", s)
		}
		if len(calls()) != 0 {
			t.Errorf("Run(%+v) ran %q", s, calls())
		}
	}
}

func TestStreamCommand(t *testing.T) {
	var out bytes.Buffer
	if err := streamCommand(context.Background(), []string{"GOFLAGS=-count=1"}, "", &out, "go", "env", "GOFLAGS"); err != nil || strings.TrimSpace(out.String()) != "-count=1" {
		t.Errorf("streamCommand = %q, %v; want the extra environment passed", out.String(), err)
	}
	out.Reset()
	err := streamCommand(context.Background(), nil, "", &out, "go", "no-such-command")
	var exit interface{ ExitCode() int }
	if !errors.As(err, &exit) || !strings.Contains(out.String(), "unknown command") {
		t.Errorf("streamCommand = %v, output %q; want the exit and standard error in the output", err, out.String())
	}
	if err := streamCommand(context.Background(), nil, "", &out, "no-such-command-here"); err == nil {
		t.Error("a command that does not exist ran")
	}
}
