package environment

import (
	"bytes"
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
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	defaultimage "github.com/rodo-software/cinderline-runner/images/default"
	proxyimage "github.com/rodo-software/cinderline-runner/images/proxy"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// MySQLImage is the run's database, MySQL 8 as today's compose file, pinned
// by digest (mysql 8.0.46, multi-arch index).
const MySQLImage = "mysql:8.0.46@sha256:7dcddc01f13bab2f15cde676d44d01f61fc9f99fe7785e86196dfc07d358ae2b"

// PublishedImage is the default image on GHCR, multi-arch, without agent
// CLIs (decision 2026-11-05-runner-rewrite-details point 2).
const PublishedImage = "ghcr.io/rodo-software/" + name.Name + "-runner-default"

// publishedDigest pins the published image; publishedSum is the first 12 hex
// of the SHA-256 of the images/default/Dockerfile it was built from, so a
// changed Dockerfile is built locally until it is published and pinned again.
// Pinned to tag v0.1.1 (CSR-151); repeat the pin after every Dockerfile
// change (.github/workflows/default-image.yml, point 3).
var publishedDigest, publishedSum = "sha256:22dc9fdeda6197570675db35feb6826639a35ae3f16079151c7b3cddcb95e95d", "8bc0a5e245dc"

// DBEnv are the variables today's compose file sets, so Laravel's tests in
// the clone reach the run's MySQL; real environment variables win over the
// project's .env file.
var DBEnv = []string{"DB_HOST=mysql", "DB_PORT=3306", "DB_DATABASE=laravel_testing", "DB_USERNAME=laravel", "DB_PASSWORD=laravel"}

// mysqlEnv creates the database and user DBEnv names.
var mysqlEnv = []string{"-e", "MYSQL_DATABASE=laravel_testing", "-e", "MYSQL_USER=laravel", "-e", "MYSQL_PASSWORD=laravel", "-e", "MYSQL_ROOT_PASSWORD=root"}

// ProxyEnv sends the agent's HTTP(S) traffic through the run's egress proxy;
// the agent CLIs, git, Composer, npm and Go read either spelling, curl only
// the lowercase http_proxy. MySQL stays direct on the internal network.
var ProxyEnv = []string{
	"HTTPS_PROXY=http://proxy:3128", "HTTP_PROXY=http://proxy:3128",
	"https_proxy=http://proxy:3128", "http_proxy=http://proxy:3128",
	"NO_PROXY=mysql,localhost,127.0.0.1", "no_proxy=mysql,localhost,127.0.0.1",
}

// hardening keeps the agent from gaining privileges (CSR-63): no
// capabilities, none through setuid binaries, and Docker's default seccomp
// profile, which then refuses unshare and clone with new namespaces.
var hardening = []string{"--cap-drop", "ALL", "--security-opt", "no-new-privileges"}

// Docker is the default Environment: the default image, an internal network
// per run with a fresh MySQL (decision 2026-11-05-runner-rewrite-details
// point 13) and an egress proxy as its only way out (CSR-63), and every
// container and network labelled with the project and issue (review item 9).
type Docker struct {
	Project    string         // project key, in names and labels
	Exec       workspace.Exec // nil means workspace.RunCommand
	Stream     Stream         // the agent container; nil means streamCommand
	Log        *slog.Logger   // the agent's output, line by line; nil means slog.Default()
	BuildImage bool           // config.json build_image: always build the image locally
	// Allow is the project's hosts every run may reach, as host:port
	// (ProjectHosts); each run adds its client's Spec.Allow.
	Allow []string
	// OnRefused is called with each host:port the run's proxy refused; nil: none.
	OnRefused func(hostPort string)
	// OnFetch is called before the default image is pulled or built, a slow
	// first-time step; nil: none. A failed pull calls it again for the build.
	OnFetch func()
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

var issueRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Run runs one agent turn: an internal network with a fresh MySQL and the
// egress proxy allowing d.Allow and s.Allow, then the hardened agent
// container with only DBEnv, ProxyEnv and s.Env, stopped with docker stop at
// s.Timeout. Its output goes to the log line by line as it comes, then the
// hosts the proxy refused. All are removed afterwards, also after a failure
// or a stop of the runner.
func (d *Docker) Run(ctx context.Context, s Spec) (string, error) {
	if !issueRef.MatchString(s.Issue) || len(s.Cmd) == 0 {
		return "", fmt.Errorf("run: issue %q, command %q", s.Issue, s.Cmd)
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
	image, err := d.Image(ctx)
	if err != nil {
		return "", err
	}
	// Without its proxy no agent starts: the run fails closed.
	proxy, err := d.ProxyImage(ctx)
	if err != nil {
		return "", err
	}
	if err := d.up(ctx, s.Issue, proxy, append(slices.Clip(d.Allow), s.Allow...)); err != nil {
		return "", err
	}
	// Removal must also happen when the runner stops.
	defer func() {
		if err := d.down(context.WithoutCancel(ctx), s.Issue); err != nil {
			d.log().Warn("run's containers not removed", "project", d.Project, "issue", s.Issue, "err", err)
		}
	}()

	net := d.network(s.Issue)
	agent := net + "-agent"
	argv := append(append([]string{"docker", "run", "--rm", "--interactive"}, workspace.Labels(d.Project, s.Issue)...),
		"--name", agent, "--network", net)
	// No --add-host host.docker.internal: decision 2026-11-09-network-allowlist.
	argv = append(append(argv, hardening...), "--user", workspace.AgentUser, "--workdir", s.Dir)
	for _, kv := range append(slices.Clip(DBEnv), ProxyEnv...) {
		argv = append(argv, "-e", kv)
	}
	// Secrets go by name: their values travel in the docker command's
	// environment, never in its argv.
	argv = append(append(append(append(argv, names...), s.Mounts...), image), s.Cmd...)

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
	stream := d.Stream
	if stream == nil {
		stream = streamCommand
	}
	err = stream(ctx, s.Env, s.Stdin, out, argv...)
	out.flush()
	d.refused(context.WithoutCancel(ctx), s.Issue)
	if timedOut.Load() {
		err = fmt.Errorf("%w (%s)", ErrTimeout, s.Timeout)
	} else if err != nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return out.all.String(), err
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
			d.OnRefused(hp)
		}
	}
}

func (d *Docker) log() *slog.Logger {
	if d.Log == nil {
		return slog.Default()
	}
	return d.Log
}

// lines keeps all output and logs it a line at a time. The stream writes
// from one goroutine at a time.
type lines struct {
	log     *slog.Logger
	all     bytes.Buffer
	pending []byte
}

func (l *lines) Write(p []byte) (int, error) {
	l.all.Write(p)
	l.pending = append(l.pending, p...)
	for {
		i := bytes.IndexByte(l.pending, '\n')
		if i < 0 {
			return len(p), nil
		}
		l.log.Info("agent output", "line", string(l.pending[:i]))
		l.pending = l.pending[i+1:]
	}
}

func (l *lines) flush() {
	if len(l.pending) > 0 {
		l.log.Info("agent output", "line", string(l.pending))
		l.pending = nil
	}
}

// Image returns the default image: the published one by its pinned digest
// when a pin is set for this Dockerfile, else the locally built one, whose
// tag is the Dockerfile's hash, so a runner with a changed Dockerfile
// builds its own. config.json's build_image always builds locally.
func (d *Docker) Image(ctx context.Context) (string, error) {
	sum := sha256.Sum256(defaultimage.Dockerfile)
	sumHex := hex.EncodeToString(sum[:6])
	tag := name.Name + "-default:" + sumHex
	onFetch := d.OnFetch
	if !d.BuildImage && publishedDigest != "" && publishedSum == sumHex {
		ref := PublishedImage + "@" + publishedDigest
		if _, err := d.exec(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", ref); err == nil {
			return ref, nil
		}
		if onFetch != nil {
			onFetch()
			onFetch = nil // announced once, also when the pull fails and we build
		}
		_, err := d.exec(ctx, "docker", "pull", "--quiet", ref)
		if err == nil {
			return ref, nil
		}
		d.log().Warn("default image pull failed, building it locally", "image", ref, "err", err)
	}
	return d.localImage(ctx, tag, map[string][]byte{"Dockerfile": defaultimage.Dockerfile}, onFetch)
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
// network+"-mysql", network+"-proxy" and network+"-agent".
func (d *Docker) network(issue string) string {
	return name.Name + "-" + d.Project + "-" + issue
}

// up creates the issue's internal network, which has no route out and no
// upstream DNS, starts the egress proxy on it as proxy, allowing allow, and
// connects the proxy to the default bridge as its way out, then starts a
// fresh MySQL as mysql. A failure leaves nothing behind.
func (d *Docker) up(ctx context.Context, issue, proxyImage string, allow []string) error {
	labels := workspace.Labels(d.Project, issue)
	net := d.network(issue)
	if _, err := d.exec(ctx, append(append([]string{"docker", "network", "create", "--internal"}, labels...), net)...); err != nil {
		return err
	}
	// No --rm: down removes them, and an automatic removal would race with that.
	proxy := append(append([]string{"docker", "run", "--detach"}, labels...),
		"--name", net+"-proxy", "--network", net, "--network-alias", "proxy")
	proxy = append(append(append(proxy, hardening...), "--read-only", proxyImage), allow...)
	mysql := append(append([]string{"docker", "run", "--detach"}, labels...),
		"--name", net+"-mysql", "--network", net, "--network-alias", "mysql")
	mysql = append(append(mysql, mysqlEnv...), MySQLImage)
	for _, argv := range [][]string{proxy, {"docker", "network", "connect", "bridge", net + "-proxy"}, mysql} {
		if _, err := d.exec(ctx, argv...); err != nil {
			return errors.Join(err, d.down(ctx, issue))
		}
	}
	return nil
}

// down removes the issue's agent, if a killed docker command left it,
// MySQL with its data volume and the proxy, then the network. It tries both, so one
// failure does not leave the other behind. docker rm --force ignores a
// container that is already gone.
func (d *Docker) down(ctx context.Context, issue string) error {
	net := d.network(issue)
	_, err := d.exec(ctx, "docker", "rm", "--force", "--volumes", net+"-agent", net+"-mysql", net+"-proxy")
	if _, nerr := d.exec(ctx, "docker", "network", "rm", net); nerr != nil {
		err = errors.Join(err, nerr)
	}
	return err
}

// Cleanup removes the project's containers and networks a previous process
// left, so Docker's address pools do not run out (review item 9). It runs
// in recovery at start, when none of them is in use. Volumes stay: they hold
// the clones of paused runs.
func (d *Docker) Cleanup(ctx context.Context) error {
	filter := []string{"--filter", "label=" + workspace.ProjectLabel(d.Project)}
	return errors.Join(
		d.remove(ctx, append([]string{"docker", "ps", "--all", "--quiet"}, filter...), "docker", "rm", "--force", "--volumes"),
		d.remove(ctx, append([]string{"docker", "network", "ls", "--quiet"}, filter...), "docker", "network", "rm"),
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
	x := d.Exec
	if x == nil {
		x = workspace.RunCommand
	}
	return x(ctx, nil, argv...)
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
