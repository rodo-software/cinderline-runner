package environment

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	defaultimage "github.com/rodo-software/cinderline-runner/images/default"
	proxyimage "github.com/rodo-software/cinderline-runner/images/proxy"
	"github.com/rodo-software/cinderline-runner/internal/ca"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// PublishedImage is the default image on GHCR, multi-arch, without agent
// CLIs (decision 2026-11-05-runner-rewrite-details point 2).
const PublishedImage = "ghcr.io/rodo-software/" + name.Name + "-runner-default"

// publishedDigest pins the published image; publishedSum is the first 12 hex
// of the SHA-256 of the images/default/Dockerfile it was built from, so a
// changed Dockerfile is built locally until it is published and pinned again.
// Pinned to tag v0.1.1 (CSR-151); repeat the pin after every Dockerfile
// change (.github/workflows/default-image.yml, its header). Pin only a
// manifest list with linux/amd64 and linux/arm64: a pinned image that fails
// to pull stops the runs (decision 2026-11-24-default-image-amd64).
var publishedDigest, publishedSum = "sha256:22dc9fdeda6197570675db35feb6826639a35ae3f16079151c7b3cddcb95e95d", "8bc0a5e245dc"

// ProxyEnv sends the agent's HTTP(S) traffic through the run's egress proxy;
// the agent CLIs, git, Composer, npm and Go read either spelling, curl only
// the lowercase http_proxy. With a compose file its services stay direct.
var ProxyEnv = []string{
	"HTTPS_PROXY=http://proxy:3128", "HTTP_PROXY=http://proxy:3128",
	"https_proxy=http://proxy:3128", "http_proxy=http://proxy:3128",
	"NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1",
}

// CAPath is where an agent's or check's container mounts the runner's CA
// certificate, read-only; never its key (app decision
// 2026-11-25-blocked-host-urls).
const CAPath = "/etc/" + name.Name + "/ca.crt"

// caBundle is the bundle trustScript writes: the image's roots plus the CA.
const caBundle = "/tmp/" + name.Name + "-ca-bundle.pem"

// trustScript runs before the container's command and execs it with its
// argv and stdin unchanged. The container runs as the agent user without
// capabilities, so update-ca-certificates cannot add the CA to the image's
// store; instead it writes a bundle of the image's own roots, the ones its
// SSL_CERT_FILE names first, plus the CA, and points the variables OpenSSL,
// curl, git and Python read at it. The image's own roots, not a fixed list,
// so a compose image's private roots keep working. An image without a
// bundle at a known place keeps its roots and only Node trusts the CA.
const trustScript = `for f in "${SSL_CERT_FILE:-}" /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt /etc/ssl/cert.pem /etc/ssl/ca-bundle.pem; do
  if [ -s "$f" ]; then
    if cat "$f" ` + CAPath + ` > ` + caBundle + ` 2>/dev/null; then
      export SSL_CERT_FILE=` + caBundle + ` REQUESTS_CA_BUNDLE=` + caBundle + ` CURL_CA_BUNDLE=` + caBundle + ` GIT_SSL_CAINFO=` + caBundle + `
    fi
    break
  fi
done
exec "$@"`

// hardening keeps the agent from gaining privileges (CSR-63): no
// capabilities, none through setuid binaries, and Docker's default seccomp
// profile, which then refuses unshare and clone with new namespaces.
var hardening = []string{"--cap-drop", "ALL", "--security-opt", "no-new-privileges"}

// proxyMemory is the egress proxy's memory limit, swap included: a fixed
// small one (decision 2026-11-24-agent-memory-limit).
const proxyMemory = "64m"

// memoryLimit limits a container to bytes of memory without swap, so going
// over kills it instead of slowing the machine (decision 2026-11-24-agent-memory-limit).
func memoryLimit(bytes string) []string {
	return []string{"--memory", bytes, "--memory-swap", bytes}
}

// Docker is the default Environment: the default image, an internal network
// per run with an egress proxy as its only way out (CSR-63) and no other
// service (a project that needs a database brings a compose file), and every
// container and network labelled with the project and issue (review item 9).
type Docker struct {
	Project string         // project key, in names and labels
	Exec    workspace.Exec // runs docker
	Stream  Stream         // the agent container; nil means streamCommand
	Log     *slog.Logger   // the agent's output, line by line; nil means slog.Default()
	// Allow is the project's hosts every run may reach, as host:port
	// (ProjectHosts); each run adds its client's Spec.Allow.
	Allow []string
	// Memory is the agent container's memory limit in bytes, the project's
	// MemoryLimit; a mem_limit on the compose file's agent service wins.
	// Run refuses to start an agent without one.
	Memory int64
	// OnRefused is called with each host:port the run's proxy refused, and
	// the run's issue; nil: none.
	OnRefused func(issue, hostPort string)
	// OnFetch is called before the default image is pulled or built, a slow
	// first-time step; nil: none.
	OnFetch func()
	// CA is the runner's CA folder (ca.Dir), whose certificate every agent
	// and check container trusts; "": none, as in tests without one.
	CA string

	compose atomic.Pointer[Compose] // the project's own compose file; nil: the default environment
}

var _ Environment = (*Docker)(nil)

// Stream runs argv with env added to the runner's environment and stdin as
// its input, and writes its standard output and error to out as they come.
// A command that ran and failed returns an error with an ExitCode method.
type Stream func(ctx context.Context, env []string, stdin string, out io.Writer, argv ...string) error

func streamCommand(ctx context.Context, env []string, stdin string, out io.Writer, argv ...string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

// Run runs one agent turn: an internal network with the egress proxy
// allowing d.Allow and s.Allow, then the hardened agent container with only
// ProxyEnv and s.Env, limited to d.Memory without swap, stopped with docker
// stop at s.Timeout. With a compose file (SetCompose), its services start
// beside the proxy, and the agent runs as hardened in the agent service's
// image with that service's environment too, and its mem_limit if it sets
// one; a run killed at that limit returns an *OutOfMemoryError. Its output
// goes to the log line by line as it comes, then the hosts the proxy
// refused. All are removed afterwards, also after a failure or a stop of the
// runner.
func (d *Docker) Run(ctx context.Context, s Spec) (string, error) {
	if !workspace.IssueRef.MatchString(s.Issue) || len(s.Cmd) == 0 {
		return "", fmt.Errorf("run: issue %q, command %q", s.Issue, s.Cmd)
	}
	if d.Memory <= 0 {
		return "", fmt.Errorf("run %s: no memory limit set", s.Issue)
	}
	var names []string
	for i, kv := range s.Env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			// Not quoted: it may be a secret.
			return "", fmt.Errorf("run %s: environment entry %d is not KEY=value", s.Issue, i)
		}
		names = append(names, "-e", k)
	}
	cmd, trust, err := d.trust(s.Cmd)
	if err != nil {
		return "", fmt.Errorf("run %s: %w", s.Issue, err)
	}
	compose := d.compose.Load()
	image := ""
	if compose == nil {
		i, err := d.Image(ctx)
		if err != nil {
			return "", err
		}
		image = i
	}
	// Without its proxy no agent starts: the run fails closed.
	proxy, err := d.ProxyImage(ctx)
	if err != nil {
		return "", err
	}
	// Only a turn's own provider maps the host, never the project's allow_hosts.
	hostGateway := slices.ContainsFunc(s.Allow, func(hp string) bool { return strings.HasPrefix(hp, HostGateway+":") })
	allow := slices.Concat(d.Allow, s.Allow)
	// Keep app approvals distinct through to dialing (decision 2026-11-19-app-host-addresses).
	for _, hp := range s.AppAllow {
		allow = append(allow, "public:"+hp)
	}
	if err := d.up(ctx, s.Issue, proxy, allow, hostGateway); err != nil {
		return "", err
	}
	var p *composed
	// Removal must also happen when the runner stops.
	defer func() {
		var err error
		if p != nil {
			if p.args != nil {
				err = d.composeDown(context.WithoutCancel(ctx), p)
			}
			err = errors.Join(err, os.RemoveAll(p.dir))
		}
		if err = errors.Join(err, d.down(context.WithoutCancel(ctx), s.Issue)); err != nil {
			d.log().Warn("run's containers not removed", "project", d.Project, "issue", s.Issue, "err", err)
		}
	}()
	env, memory := ProxyEnv, d.Memory
	if compose != nil {
		if p, err = d.composeProject(ctx, s.Issue, compose); err != nil {
			return "", err
		}
		if err := d.composeUp(ctx, p); err != nil {
			return "", err
		}
		image, env, memory = p.image, p.env, cmp.Or(p.memory, memory)
	}

	net := d.network(s.Issue)
	agent := net + "-agent"
	// No --rm: the container must outlive its command so oomKilled can
	// inspect it; down removes it.
	argv := append(append([]string{"docker", "run", "--interactive"}, workspace.Labels(d.Project, s.Issue)...),
		"--name", agent, "--network", net)
	// No --add-host host.docker.internal: decision 2026-11-09-network-allowlist;
	// a local provider maps it in the proxy only (2026-11-17-local-model-providers).
	argv = slices.Concat(argv, hardening, memoryLimit(strconv.FormatInt(memory, 10)), []string{"--user", workspace.AgentUser, "--workdir", s.Dir})
	for _, kv := range env {
		argv = append(argv, "-e", kv)
	}
	// Secrets go by name: their values travel in the docker command's
	// environment, never in its argv.
	argv = slices.Concat(argv, names, s.Mounts, trust, []string{image}, cmd)

	var timedOut atomic.Bool
	if s.Timeout > 0 {
		t := time.AfterFunc(s.Timeout, func() {
			timedOut.Store(true)
			if _, err := d.exec(context.WithoutCancel(ctx), "docker", "stop", agent); err != nil {
				d.log().Warn("agent not stopped at the time limit", "project", d.Project, "issue", s.Issue, "err", err)
			}
		})
		defer t.Stop()
	}
	out := &lines{log: d.log().With("project", d.Project, "issue", s.Issue)}
	var idled atomic.Bool
	if s.Idle > 0 {
		stall := time.AfterFunc(s.Idle, func() {
			idled.Store(true)
			if _, err := d.exec(context.WithoutCancel(ctx), "docker", "stop", agent); err != nil {
				d.log().Warn("idle agent not stopped", "project", d.Project, "issue", s.Issue, "err", err)
			}
		})
		defer stall.Stop()
		out.touch = func() {
			if !idled.Load() {
				stall.Reset(s.Idle)
			}
		}
	}
	stream := d.Stream
	if stream == nil {
		stream = streamCommand
	}
	err = stream(ctx, s.Env, s.Stdin, out, argv...)
	out.flush()
	kept := out.String()
	if n := out.dropped(); n > 0 {
		d.log().Warn("output cut in the middle; tool counts and OpenCode's usage miss it", "project", d.Project, "issue", s.Issue, "bytes", n)
	}
	d.refused(context.WithoutCancel(ctx), s.Issue)
	if timedOut.Load() {
		err = fmt.Errorf("%w (%s)", ErrTimeout, s.Timeout)
	} else if idled.Load() {
		err = fmt.Errorf("%w: no output for %s; the last line was: %s", ErrIdle, s.Idle, lastLine(kept))
	} else if err != nil && ctx.Err() != nil {
		err = ctx.Err()
	} else if err != nil && d.oomKilled(ctx, s.Issue, agent) {
		err = &OutOfMemoryError{Limit: memory}
	}
	return kept, err
}

// trust returns cmd run through trustScript and the flags that mount the
// CA's certificate and point Node at it, which adds it to its own roots;
// without d.CA, cmd alone. The CA is made or renewed first when needed, so
// a new one reaches only the containers started after it.
func (d *Docker) trust(cmd []string) ([]string, []string, error) {
	if d.CA == "" {
		return cmd, nil, nil
	}
	c, err := ca.Ensure(d.CA, time.Now())
	if err != nil {
		return nil, nil, fmt.Errorf("the runner's CA: %w", err)
	}
	return slices.Concat([]string{"sh", "-c", trustScript, "sh"}, cmd),
		[]string{"-v", c.CertFile + ":" + CAPath + ":ro", "-e", "NODE_EXTRA_CA_CERTS=" + CAPath}, nil
}

// oomKilled reports whether the kernel killed a process of the agent
// container at its memory limit. Docker's own flag, not exit code 137: the
// runner's docker stop ends in a kill too, and a killed child of the
// agent's command may make it exit with any code. Unknown is false, so the
// run stays a failure.
func (d *Docker) oomKilled(ctx context.Context, issue, agent string) bool {
	out, err := d.exec(ctx, "docker", "inspect", "--format", "{{.State.OOMKilled}}", agent)
	if err != nil {
		d.log().Warn("agent's memory state not read", "project", d.Project, "issue", issue, "err", err)
		return false
	}
	return strings.TrimSpace(out) == "true"
}

// refused logs each host the run's proxy refused, by name, and hands it to
// OnRefused for the runner report: a blocked host fails visibly, and a model
// that probes once in many turns still shows up.
func (d *Docker) refused(ctx context.Context, issue string) {
	log := d.log().With("project", d.Project, "issue", issue)
	out, err := d.exec(ctx, "docker", "logs", d.network(issue)+"-proxy")
	if err != nil {
		log.Warn("proxy log not read", "err", err)
		return
	}
	for _, line := range strings.Split(out, "\n") {
		var e struct{ Time, Msg, Host, Port string }
		if json.Unmarshal([]byte(line), &e) != nil || e.Msg != "refused" {
			continue
		}
		hp := e.Host + ":" + e.Port
		log.Warn("host refused", "host", hp, "at", e.Time)
		if d.OnRefused != nil {
			d.OnRefused(issue, hp)
		}
	}
}

func (d *Docker) log() *slog.Logger {
	if d.Log == nil {
		return slog.Default()
	}
	return d.Log
}

// OutputMsg is the message of each logged line of container output. The
// daemon's log handler finds it by this message and cleans its "line".
const OutputMsg = "agent output"

// Output kept per turn or check: its first headBytes, as Codex prints its
// thread ID first, and a rolling last tailBytes, where the clients find the
// report, usage and limits. Past both, the tool counts, and OpenCode's
// summed usage, miss the cut middle (decision 2026-11-24-output-cap).
// lineBytes is the longest line that is logged.
const (
	headBytes = 1 << 20
	tailBytes = 128 << 20
	lineBytes = 1 << 20
)

// lines keeps the output's head and tail, which the clients parse, and logs
// it a line at a time at Debug: a turn prints far more than the log should
// keep. A line still unfinished past lineBytes is not logged, as a part of
// it could hold part of a secret that redaction no longer recognises. The stream
// writes from one goroutine at a time.
type lines struct {
	log     *slog.Logger
	touch   func() // called on every write; nil: none
	head    []byte
	tail    []byte // up to twice tailBytes, as forge's tail
	cut     int64  // bytes dropped from the tail's front
	pending []byte
	long    bool // within a line longer than lineBytes
}

func (l *lines) Write(p []byte) (int, error) {
	if l.touch != nil {
		l.touch()
	}
	n := min(len(p), headBytes-len(l.head))
	l.head = append(l.head, p[:n]...)
	l.tail = append(l.tail, p[n:]...)
	if len(l.tail) > 2*tailBytes {
		drop := len(l.tail) - tailBytes
		l.cut += int64(drop)
		l.tail = append(l.tail[:0], l.tail[drop:]...)
	}
	l.pending = append(l.pending, p...)
	for {
		i := bytes.IndexByte(l.pending, '\n')
		if i < 0 {
			break
		}
		if !l.long {
			l.log.Debug(OutputMsg, "line", string(l.pending[:i]))
		}
		l.long = false
		l.pending = l.pending[i+1:]
	}
	if len(l.pending) > lineBytes {
		if !l.long {
			l.log.Debug(OutputMsg, "line", fmt.Sprintf("[a line of more than %d bytes, not logged]", lineBytes))
		}
		l.long, l.pending = true, nil
	}
	return len(p), nil
}

func (l *lines) flush() {
	if len(l.pending) > 0 && !l.long {
		l.log.Debug(OutputMsg, "line", string(l.pending))
	}
	l.pending = nil
}

// dropped is how many bytes of output were not kept.
func (l *lines) dropped() int64 {
	return l.cut + int64(max(0, len(l.tail)-tailBytes))
}

// String is the output kept: all of it, or its head and tail with a line
// between them that names the bytes cut. The cut lines' ends are not JSON,
// so the clients skip them.
func (l *lines) String() string {
	end := l.tail[max(0, len(l.tail)-tailBytes):]
	var b strings.Builder // one copy: the tail can be 128 MB
	b.Grow(len(l.head) + 64 + len(end))
	b.Write(l.head)
	if n := l.dropped(); n > 0 {
		fmt.Fprintf(&b, "\n[%d bytes of output cut]\n", n)
	}
	b.Write(end)
	return b.String()
}

// DockerMemory is Docker's total memory in bytes, as `docker info` reports
// it: the VM's on Docker Desktop, the host's on Linux.
func DockerMemory(ctx context.Context, exec workspace.Exec) (int64, error) {
	out, err := exec(ctx, nil, "docker", "info", "--format", "{{.MemTotal}}")
	if err != nil {
		return 0, fmt.Errorf("docker info: %w", err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("docker info: memory %q is not a positive number of bytes", strings.TrimSpace(out))
	}
	return n, nil
}

// Image returns the default image: the published one by its pinned digest
// when a pin is set for this Dockerfile, else the locally built one, whose
// tag is the Dockerfile's hash, so a runner with a changed Dockerfile
// builds its own. A failed pull of the pinned image is an error, not a
// local build from unpinned inputs; the next poll tries again (decision
// 2026-11-24-default-image-amd64).
func (d *Docker) Image(ctx context.Context) (string, error) {
	sum := sha256.Sum256(defaultimage.Dockerfile)
	sumHex := hex.EncodeToString(sum[:6])
	if publishedDigest == "" || publishedSum != sumHex {
		tag := name.Name + "-default:" + sumHex
		return d.localImage(ctx, tag, map[string][]byte{"Dockerfile": defaultimage.Dockerfile}, d.OnFetch)
	}
	ref := PublishedImage + "@" + publishedDigest
	if _, err := d.exec(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", ref); err == nil {
		return ref, nil
	}
	if d.OnFetch != nil {
		d.OnFetch()
	}
	if _, err := d.exec(ctx, "docker", "pull", "--quiet", ref); err != nil {
		return "", fmt.Errorf("pull default image %s: %w", ref, err)
	}
	return ref, nil
}

// ProxyImage returns the egress proxy's image, built locally from the
// embedded Dockerfile and source and tagged with their hash.
func (d *Docker) ProxyImage(ctx context.Context) (string, error) {
	sum := sha256.Sum256(append(append(slices.Clip(proxyimage.Dockerfile), 0), proxyimage.Main...))
	tag := name.Name + "-proxy:" + hex.EncodeToString(sum[:6])
	return d.localImage(ctx, tag, map[string][]byte{"Dockerfile": proxyimage.Dockerfile, "main.go": proxyimage.Main}, nil)
}

// localImage returns tag, built from files when Docker does not have it;
// onBuild, if set, is called before the build.
func (d *Docker) localImage(ctx context.Context, tag string, files map[string][]byte, onBuild func()) (string, error) {
	if _, err := d.exec(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", tag); err == nil {
		return tag, nil
	}
	dir, err := os.MkdirTemp("", name.Name+"-image-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	for f, b := range files {
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o600); err != nil {
			return "", err
		}
	}
	if onBuild != nil {
		onBuild()
	}
	_, err = d.exec(ctx, "docker", "build", "--quiet", "--tag", tag, dir)
	return tag, err
}

// network is the name of the issue's internal network; its containers are
// network+"-proxy" and network+"-agent".
func (d *Docker) network(issue string) string {
	return name.Name + "-" + d.Project + "-" + issue
}

// up creates the issue's internal network, which has no route out and no
// upstream DNS, starts the egress proxy on it as proxy, allowing allow, and
// connects the proxy to the default bridge as its way out. hostGateway maps
// HostGateway to the runner's machine in the proxy, which reaches only the
// ports allow lists on it. What an earlier
// turn left under these names is removed first when it is in the way. A
// failure leaves nothing behind.
func (d *Docker) up(ctx context.Context, issue, proxyImage string, allow []string, hostGateway bool) error {
	labels := workspace.Labels(d.Project, issue)
	net := d.network(issue)
	create := append(append([]string{"docker", "network", "create", "--internal"}, labels...), net)
	if _, err := d.exec(ctx, create...); err != nil {
		// A turn whose removal failed left the network: without this every
		// later turn of the issue fails until a restart. One more try.
		_ = d.down(ctx, issue)
		if _, err := d.exec(ctx, create...); err != nil {
			return err
		}
	}
	// No --rm: down removes them, and an automatic removal would race with that.
	proxy := append(append([]string{"docker", "run", "--detach"}, labels...),
		"--name", net+"-proxy", "--network", net, "--network-alias", "proxy")
	if hostGateway {
		proxy = append(proxy, "--add-host", HostGateway+":host-gateway")
	}
	proxy = slices.Concat(proxy, hardening, []string{"--read-only"}, memoryLimit(proxyMemory), []string{proxyImage}, allow)
	for _, argv := range [][]string{proxy, {"docker", "network", "connect", "bridge", net + "-proxy"}} {
		if _, err := d.exec(ctx, argv...); err != nil {
			return errors.Join(err, d.down(ctx, issue))
		}
	}
	return nil
}

// down removes the issue's agent and the proxy, then the network. It tries
// both, so one failure does not leave the other behind. docker rm --force
// ignores a container that is already gone.
func (d *Docker) down(ctx context.Context, issue string) error {
	net := d.network(issue)
	_, err := d.exec(ctx, "docker", "rm", "--force", "--volumes", net+"-agent", net+"-proxy")
	if _, nerr := d.exec(ctx, "docker", "network", "rm", net); nerr != nil {
		err = errors.Join(err, nerr)
	}
	return err
}

// Cleanup removes the project's containers and networks a previous process
// left, so Docker's address pools do not run out (review item 9). It runs
// in recovery at start, when none of them is in use. Volumes stay, they hold
// the clones of paused runs, except those of compose projects.
func (d *Docker) Cleanup(ctx context.Context) error {
	filter := []string{"--filter", "label=" + workspace.ProjectLabel(d.Project)}
	return errors.Join(
		d.remove(ctx, append([]string{"docker", "ps", "--all", "--quiet"}, filter...), "docker", "rm", "--force", "--volumes"),
		d.remove(ctx, append([]string{"docker", "network", "ls", "--quiet"}, filter...), "docker", "network", "rm"),
		d.remove(ctx, []string{"docker", "volume", "ls", "--quiet", "--filter", "label=" + composeLabel(d.Project)}, "docker", "volume", "rm", "--force"),
	)
}

// remove runs rm with the IDs list prints, if there are any.
func (d *Docker) remove(ctx context.Context, list []string, rm ...string) error {
	out, err := d.exec(ctx, list...)
	if ids := strings.Fields(out); err == nil && len(ids) > 0 {
		_, err = d.exec(ctx, append(rm, ids...)...)
	}
	return err
}

func (d *Docker) exec(ctx context.Context, argv ...string) (string, error) {
	return d.Exec(ctx, nil, argv...)
}

// tail is the last lines of output, at most maxChars characters, for a
// comment when a run fails.
func tail(output string, lines, maxChars int) string {
	l := strings.Split(strings.TrimSpace(output), "\n")
	r := []rune(strings.Join(l[max(0, len(l)-lines):], "\n"))
	if len(r) > maxChars {
		return "…" + string(r[len(r)-maxChars:])
	}
	return string(r)
}

// lastLine is the output's last non-empty line, at most 300 characters, for
// the note of an idle run.
func lastLine(out string) string {
	out = strings.TrimSpace(out)
	line := out[strings.LastIndexByte(out, '\n')+1:]
	if r := []rune(line); len(r) > 300 {
		line = string(r[:300]) + "…"
	}
	return line
}
