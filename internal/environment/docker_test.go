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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	defaultimage "github.com/rodo-software/cinderline-runner/images/default"
	proxyimage "github.com/rodo-software/cinderline-runner/images/proxy"
	"github.com/rodo-software/cinderline-runner/internal/redact"
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
	pin(t, "", "")
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
	pin(t, "", "")
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

var issueLabels = []string{"--label", "cinderline.project=CSR", "--label", "cinderline.issue=CSR-31"}

// CSR-63: the run's network is internal (no route out, no upstream DNS); the
// egress proxy sits on it and on the default bridge, hardened, with the
// run's allowlist as its arguments.
func TestUp(t *testing.T) {
	d, calls := scripted(nil, nil)
	if err := d.up(context.Background(), "CSR-31", "cinderline-proxy:abc", []string{"cs.example:443", "api.anthropic.com:443"}, false); err != nil {
		t.Fatal(err)
	}
	want := []execCall{
		{nil, append(append([]string{"docker", "network", "create", "--internal"}, issueLabels...), "cinderline-CSR-CSR-31")},
		{nil, append(append([]string{"docker", "run", "--detach"}, issueLabels...),
			"--name", "cinderline-CSR-CSR-31-proxy", "--network", "cinderline-CSR-CSR-31", "--network-alias", "proxy",
			"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only",
			"--memory", "64m", "--memory-swap", "64m",
			"cinderline-proxy:abc", "cs.example:443", "api.anthropic.com:443")},
		{nil, []string{"docker", "network", "connect", "bridge", "cinderline-CSR-CSR-31-proxy"}},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls =\n%q\nwant\n%q", *calls, want)
	}
}

func TestUpFailureLeavesNothing(t *testing.T) {
	boom := errors.New("boom")
	d, calls := scripted(nil, map[int]error{1: boom})
	if err := d.up(context.Background(), "CSR-31", "cinderline-proxy:abc", nil, false); !errors.Is(err, boom) {
		t.Errorf("up = %v, want boom", err)
	}
	if got := (*calls)[len(*calls)-1].Argv; !reflect.DeepEqual(got, []string{"docker", "network", "rm", "cinderline-CSR-CSR-31"}) {
		t.Errorf("last call = %q, want the network removed", got)
	}
}

// A network a turn's failed removal left is removed and made again, so the
// issue's later turns still run (decision 2026-11-11-audit-fixes).
func TestUpRemovesALeftoverNetwork(t *testing.T) {
	d, calls := scripted(nil, map[int]error{0: errors.New("network already exists")})
	if err := d.up(context.Background(), "CSR-31", "cinderline-proxy:abc", nil, false); err != nil {
		t.Fatalf("up = %v", err)
	}
	var got []string
	for _, c := range *calls {
		got = append(got, strings.Join(c.Argv[1:3], " "))
	}
	if want := []string{"network create", "rm --force", "network rm", "network create", "run --detach", "network connect"}; !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

func TestDownRemovesAllEvenAfterAFailure(t *testing.T) {
	boom := errors.New("boom")
	d, calls := scripted(nil, map[int]error{0: boom})
	if err := d.down(context.Background(), "CSR-31"); !errors.Is(err, boom) {
		t.Errorf("down = %v, want boom", err)
	}
	want := []execCall{
		{nil, []string{"docker", "rm", "--force", "--volumes", "cinderline-CSR-CSR-31-agent", "cinderline-CSR-CSR-31-proxy"}},
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
		{nil, []string{"docker", "volume", "ls", "--quiet", "--filter", "label=cinderline.compose.CSR"}},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls =\n%q\nwant\n%q", *calls, want)
	}
}

// Volumes are left alone, they hold the clones of paused runs and
// sessions, except those of compose projects.
func TestCleanupWithNothingStale(t *testing.T) {
	d, calls := scripted(nil, nil)
	if err := d.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 3 {
		t.Errorf("calls = %q, want only the three lists", *calls)
	}
}

func TestCleanupReportsAFailedList(t *testing.T) {
	boom := errors.New("boom")
	d, calls := scripted(nil, map[int]error{0: boom})
	if err := d.Cleanup(context.Background()); !errors.Is(err, boom) {
		t.Errorf("Cleanup = %v, want boom", err)
	}
	if len(*calls) != 3 {
		t.Errorf("calls = %q, want the networks and volumes still listed", *calls)
	}
}

// Tail keeps the last lines and caps the length.
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
		Memory:  4 << 30,
		Log:     slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
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
	Allow:  []string{"api.anthropic.com:443"},
}

func TestRun(t *testing.T) {
	d, calls, logs := runner(nil, func(_ context.Context, out io.Writer) error {
		io.WriteString(out, "first line\nsecond ")
		io.WriteString(out, "line\nno newline")
		return nil
	})
	d.Allow = []string{"cs.example:443"}
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
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		// No swap: going over the limit kills the turn (decision 2026-11-24-agent-memory-limit).
		"--memory", "4294967296", "--memory-swap", "4294967296",
		"--user", "1000:1000", "--workdir", "/runs/CSR-31",
		"-e", "HTTPS_PROXY=http://proxy:3128", "-e", "HTTP_PROXY=http://proxy:3128",
		"-e", "https_proxy=http://proxy:3128", "-e", "http_proxy=http://proxy:3128",
		"-e", "NO_PROXY=localhost,127.0.0.1", "-e", "no_proxy=localhost,127.0.0.1",
		// Secrets by name only: their values travel in the docker command's environment.
		"-e", "ANTHROPIC_API_KEY", "-e", "CINDERLINE_TOKEN",
		"-v", "cinderline-CSR-runs:/runs", image, "claude", "-p", "<stdin:the prompt>")
	got := calls()
	if len(got) != 9 {
		t.Fatalf("calls =\n%q", got)
	}
	if want := (execCall{turn.Env, agent}); !reflect.DeepEqual(got[5], want) {
		t.Errorf("agent call =\n%q\nwant\n%q", got[5], want)
	}
	// The project's hosts first, then the client's.
	if want := []string{proxyTag(), "cs.example:443", "api.anthropic.com:443"}; !reflect.DeepEqual(got[3].Argv[len(got[3].Argv)-3:], want) {
		t.Errorf("proxy call = %q, want it to end in %q", got[3].Argv, want)
	}
	wantAfter := []execCall{
		{nil, []string{"docker", "logs", "cinderline-CSR-CSR-31-proxy"}},
		{nil, []string{"docker", "rm", "--force", "--volumes", "cinderline-CSR-CSR-31-agent", "cinderline-CSR-CSR-31-proxy"}},
		{nil, []string{"docker", "network", "rm", "cinderline-CSR-CSR-31"}},
	}
	if !reflect.DeepEqual(got[0].Argv[:3], []string{"docker", "image", "inspect"}) || !reflect.DeepEqual(got[1].Argv[:3], []string{"docker", "image", "inspect"}) ||
		got[2].Argv[1] != "network" || got[3].Argv[1] != "run" || got[4].Argv[2] != "connect" {
		t.Errorf("before the agent: %q, want image, proxy image, network, proxy, its egress", got[:5])
	}
	if !reflect.DeepEqual(got[6:9], wantAfter) {
		t.Errorf("after the agent:\n%q\nwant\n%q", got[6:9], wantAfter)
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

func TestRunMarksOnlyAppApprovedHosts(t *testing.T) {
	d, calls, _ := runner(nil, func(context.Context, io.Writer) error { return nil })
	d.Allow = []string{"192.168.1.5:443", "mirror.example:443"}
	s := turn
	s.AppAllow = []string{"app.example:443", "mirror.example:443"}
	if _, err := d.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	want := []string{proxyTag(), "192.168.1.5:443", "mirror.example:443", "api.anthropic.com:443", "public:app.example:443", "public:mirror.example:443"}
	got := calls()[3].Argv
	if !slices.Equal(got[len(got)-len(want):], want) {
		t.Fatalf("proxy argv = %q, want suffix %q", got, want)
	}
}

func imageTag() string {
	d, _ := scripted(nil, nil)
	tag, _ := d.Image(context.Background())
	return tag
}

func proxyTag() string {
	d, _ := scripted(nil, nil)
	tag, _ := d.ProxyImage(context.Background())
	return tag
}

// agentArgv runs turn and returns the agent container's docker argv.
func agentArgv(t *testing.T) []string {
	t.Helper()
	d, calls, _ := runner(nil, func(context.Context, io.Writer) error { return nil })
	if _, err := d.Run(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	for _, c := range calls() {
		if strings.Contains(strings.Join(c.Argv, " "), "<stdin:") {
			return c.Argv
		}
	}
	t.Fatalf("no agent container in %q", calls())
	return nil
}

func hasPair(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}

// CSR-63: the agent container drops every capability, cannot gain any
// (setuid binaries included), keeps Docker's default seccomp profile, which
// refuses unshare and clone with new namespaces, and has no way around the
// internal network: no --privileged, no --add-host, no host network.
func TestAgentContainerIsHardened(t *testing.T) {
	argv := agentArgv(t)
	if !hasPair(argv, "--cap-drop", "ALL") {
		t.Errorf("agent argv lacks --cap-drop ALL: %q", argv)
	}
	if !hasPair(argv, "--security-opt", "no-new-privileges") {
		t.Errorf("agent argv lacks --security-opt no-new-privileges: %q", argv)
	}
	if !hasPair(argv, "--network", "cinderline-CSR-CSR-31") {
		t.Errorf("agent argv is not on the run's network: %q", argv)
	}
	for i, a := range argv {
		switch {
		case a == "--privileged", a == "--add-host", a == "--cap-add", strings.HasPrefix(a, "--add-host="), strings.HasPrefix(a, "--cap-add="):
			t.Errorf("agent argv has %s: %q", a, argv)
		case a == "--network" && i+1 < len(argv) && argv[i+1] == "host", a == "--network=host":
			t.Errorf("agent argv uses the host network: %q", argv)
		case a == "--security-opt" && i+1 < len(argv) && strings.HasPrefix(argv[i+1], "seccomp"):
			t.Errorf("agent argv changes the seccomp profile: %q", argv)
		case strings.Contains(a, "host.docker.internal"):
			// Willem's decision on CSR-63 (option C): no local backend through the host.
			t.Errorf("agent argv reaches host.docker.internal: %q", argv)
		}
	}
}

// The proxy variables the agent CLIs, git, Composer, npm, Go and curl honour;
// curl reads only the lowercase http_proxy, so both spellings are set.
func TestProxyEnv(t *testing.T) {
	want := []string{
		"HTTPS_PROXY=http://proxy:3128", "HTTP_PROXY=http://proxy:3128",
		"https_proxy=http://proxy:3128", "http_proxy=http://proxy:3128",
		"NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1",
	}
	if !reflect.DeepEqual(ProxyEnv, want) {
		t.Errorf("ProxyEnv = %q, want %q", ProxyEnv, want)
	}
}

// CSR-63: every CONNECT the proxy refused shows up in the run's log by host
// and is handed to OnRefused, which counts it for the runner report. Lines
// that are not refusals are ignored.
func TestRunLogsAndCountsRefusedHosts(t *testing.T) {
	d, _, logs := runner(nil, func(context.Context, io.Writer) error { return nil })
	record := d.Exec
	d.Exec = func(ctx context.Context, env []string, argv ...string) (string, error) {
		if len(argv) > 1 && argv[1] == "logs" {
			_, _ = record(ctx, env, argv...)
			return `{"time":"2026-11-09T12:00:00Z","level":"WARN","msg":"refused","host":"evil.example","port":"443"}` + "\n" +
				`{"time":"2026-11-09T12:00:01Z","level":"INFO","msg":"listening","addr":":3128"}` + "\n" +
				"not json\n" +
				`{"time":"2026-11-09T12:00:02Z","level":"WARN","msg":"refused","host":"probe.example","port":"80"}` + "\n", nil
		}
		return record(ctx, env, argv...)
	}
	var refused []string
	d.OnRefused = func(issue, host string) { refused = append(refused, issue+" "+host) }
	if _, err := d.Run(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	if want := []string{"CSR-31 evil.example:443", "CSR-31 probe.example:80"}; !reflect.DeepEqual(refused, want) {
		t.Errorf("OnRefused got %q, want %q", refused, want)
	}
	for _, want := range []string{"host=evil.example:443", "host=probe.example:80"} {
		if !strings.Contains(logs.String(), `msg="host refused"`) || !strings.Contains(logs.String(), want) {
			t.Errorf("run log lacks a host refused line with %s:\n%s", want, logs)
		}
	}
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `msg="host refused"`) && (!strings.Contains(line, "issue=CSR-31") || !strings.Contains(line, "project=CSR")) {
			t.Errorf("refused line without its project and issue: %s", line)
		}
	}
}

// The proxy image is built locally from the embedded Dockerfile and source,
// tagged with their hash, like the default image.
func TestProxyImageBuiltWhenMissing(t *testing.T) {
	files := map[string][]byte{}
	d, calls := scripted(nil, map[int]error{0: errors.New("no such image")})
	record := d.Exec
	d.Exec = func(ctx context.Context, env []string, argv ...string) (string, error) {
		if argv[1] == "build" {
			for _, f := range []string{"Dockerfile", "main.go"} {
				b, err := os.ReadFile(filepath.Join(argv[len(argv)-1], f))
				if err != nil {
					t.Error(err)
				}
				files[f] = b
			}
		}
		return record(ctx, env, argv...)
	}
	tag, err := d.ProxyImage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tag, "cinderline-proxy:") || len(tag) != len("cinderline-proxy:")+12 {
		t.Errorf("tag = %q, want cinderline-proxy:<12 hex>", tag)
	}
	if len(*calls) != 2 || !reflect.DeepEqual((*calls)[0].Argv, []string{"docker", "image", "inspect", "--format", "{{.Id}}", tag}) ||
		!reflect.DeepEqual((*calls)[1].Argv[:5], []string{"docker", "build", "--quiet", "--tag", tag}) {
		t.Errorf("calls = %q, want inspect then build of %s", *calls, tag)
	}
	if !bytes.Equal(files["Dockerfile"], proxyimage.Dockerfile) || !bytes.Equal(files["main.go"], proxyimage.Main) {
		t.Error("build folder does not hold the embedded Dockerfile and main.go")
	}
}

// Without a proxy image no agent starts: the run fails closed.
func TestRunWithoutProxyImageRunsNoAgent(t *testing.T) {
	boom := errors.New("build failed")
	d, calls, _ := runner(nil, func(context.Context, io.Writer) error {
		t.Error("agent started without its proxy")
		return nil
	})
	record := d.Exec
	d.Exec = func(ctx context.Context, env []string, argv ...string) (string, error) {
		out, _ := record(ctx, env, argv...)
		if strings.Contains(strings.Join(argv, " "), "cinderline-proxy:") {
			return out, boom
		}
		return out, nil
	}
	if _, err := d.Run(context.Background(), turn); !errors.Is(err, boom) {
		t.Errorf("Run = %v, want the proxy image error", err)
	}
	for _, c := range calls() {
		if c.Argv[1] == "network" || c.Argv[1] == "run" {
			t.Errorf("ran %q without a proxy image", c.Argv)
		}
	}
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
	if got := calls(); len(got) != 9 || got[8].Argv[1] != "network" {
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
	if len(got) != 9 || !reflect.DeepEqual(got[7].Argv[:2], []string{"docker", "rm"}) {
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
	if got := calls(); len(got) != 6 || got[5].Argv[2] != "create" {
		t.Errorf("calls = %q, want the two image inspects, network create, the leftovers removed and one more create", got)
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

// CLR-215: the project's memory setting limits the agent container, swap
// included; without one Run refuses rather than run the agent unlimited.
func TestRunMemoryLimit(t *testing.T) {
	d, calls, _ := runner(nil, func(context.Context, io.Writer) error { return nil })
	d.Memory = 8 << 30
	if _, err := d.Run(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	agent := calls()[5].Argv
	if !hasPair(agent, "--memory", "8589934592") || !hasPair(agent, "--memory-swap", "8589934592") {
		t.Errorf("agent argv = %q, want --memory and --memory-swap 8589934592", agent)
	}

	d, calls, _ = runner(nil, func(context.Context, io.Writer) error {
		t.Error("agent started without a memory limit")
		return nil
	})
	d.Memory = 0
	if _, err := d.Run(context.Background(), turn); err == nil {
		t.Error("Run without a memory limit = nil, want an error")
	}
	if len(calls()) != 0 {
		t.Errorf("Run without a memory limit ran %q", calls())
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

// TestRunRedactsSplitValue: a secret split over two writes of the agent's
// output is still redacted in the log, which gets complete lines (CSR-157).
func TestRunRedactsSplitValue(t *testing.T) {
	const key = "sk-ant-api03-splitvalue-0123456789"
	redact.Add("anthropic key", key)
	d, _, logs := runner(nil, func(_ context.Context, out io.Writer) error {
		io.WriteString(out, "env: ANTHROPIC_API_KEY="+key[:12])
		io.WriteString(out, key[12:]+"\ndone\n")
		return nil
	})
	d.Log = slog.New(redact.Handler(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if _, err := d.Run(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), key[:12]) || !strings.Contains(logs.String(), "[redacted anthropic key]") {
		t.Errorf("log:\n%s", logs.String())
	}
}

// A turn whose output stays silent for its Idle limit is stopped with
// ErrIdle, which names the last line it printed; output in time keeps it
// going (CSR-159).
func TestRunStopsWhenIdle(t *testing.T) {
	stopped := make(chan struct{})
	d, calls, _ := runner(nil, func(_ context.Context, out io.Writer) error {
		for range 5 {
			io.WriteString(out, "step\n")
			time.Sleep(20 * time.Millisecond) // well inside the limit
		}
		io.WriteString(out, `{"type":"assistant","text":"waiting"}`+"\n")
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
	s.Idle = 60 * time.Millisecond
	_, err := d.Run(context.Background(), s)
	if !errors.Is(err, ErrIdle) || errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), `{"type":"assistant","text":"waiting"}`) {
		t.Fatalf("Run = %v; want ErrIdle naming the last line", err)
	}
	n := 0
	for _, c := range calls() {
		if c.Argv[1] == "stop" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("docker stop ran %d times, want 1", n)
	}
}

// Agent output is logged at Debug, so the log at Info keeps none of it.
func TestAgentOutputIsLoggedAtDebug(t *testing.T) {
	var info, debug bytes.Buffer
	for _, c := range []struct {
		buf   *bytes.Buffer
		level slog.Level
	}{{&info, slog.LevelInfo}, {&debug, slog.LevelDebug}} {
		l := &lines{log: slog.New(slog.NewTextHandler(c.buf, &slog.HandlerOptions{Level: c.level}))}
		_, _ = l.Write([]byte("one\ntwo"))
		l.flush()
		if l.all.String() != "one\ntwo" {
			t.Errorf("kept %q", l.all.String())
		}
	}
	if info.Len() != 0 || strings.Count(debug.String(), "agent output") != 2 {
		t.Errorf("info log %q, debug log %q", info.String(), debug.String())
	}
}

// Decision 2026-11-17-local-model-providers point 3: a turn whose provider
// is on the runner's own machine gets host.docker.internal mapped to the
// host in the PROXY container only, which allows exactly that port. The
// agent container still has no route to the host.
func TestRunReachesALocalProviderThroughTheProxyOnly(t *testing.T) {
	d, calls, _ := runner(nil, func(context.Context, io.Writer) error { return nil })
	s := turn
	s.Allow = []string{"host.docker.internal:11434"}
	if _, err := d.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	var proxy, agent []string
	for _, c := range calls() {
		a := strings.Join(c.Argv, " ")
		switch {
		case strings.Contains(a, "<stdin:"):
			agent = c.Argv
		case strings.Contains(a, "--network-alias proxy"):
			proxy = c.Argv
		}
	}
	if !hasPair(proxy, "--add-host", "host.docker.internal:host-gateway") || proxy[len(proxy)-1] != "host.docker.internal:11434" {
		t.Errorf("proxy argv = %q, want host.docker.internal mapped and allowed on 11434", proxy)
	}
	if agent == nil || slices.Contains(agent, "--add-host") || strings.Contains(strings.Join(agent, " "), "host.docker.internal") {
		t.Errorf("agent argv maps a host: %q", agent)
	}
	// Without a local provider the proxy maps nothing, whatever the
	// project's allow_hosts say (config refuses the host there too).
	d, calls, _ = runner(nil, func(context.Context, io.Writer) error { return nil })
	d.Allow = []string{"host.docker.internal:22"}
	if _, err := d.Run(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	for _, c := range calls() {
		if slices.Contains(c.Argv, "--add-host") {
			t.Errorf("argv maps a host without a local provider: %q", c.Argv)
		}
	}
}

func TestDockerMemory(t *testing.T) {
	for _, tt := range []struct {
		out  string
		err  error
		want int64
		ok   bool
	}{
		{"8217108480\n", nil, 8217108480, true},
		{"", errors.New("Cannot connect to the Docker daemon"), 0, false},
		{"0\n", nil, 0, false},
		{"<no value>\n", nil, 0, false},
	} {
		var argv []string
		got, err := DockerMemory(context.Background(), func(_ context.Context, _ []string, a ...string) (string, error) {
			argv = a
			return tt.out, tt.err
		})
		if got != tt.want || (err == nil) != tt.ok {
			t.Errorf("DockerMemory on %q, %v = %d, %v; want %d, ok %v", tt.out, tt.err, got, err, tt.want, tt.ok)
		}
		if !slices.Equal(argv, []string{"docker", "info", "--format", "{{.MemTotal}}"}) {
			t.Errorf("ran %q", argv)
		}
	}
}
