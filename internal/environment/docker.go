package environment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	defaultimage "github.com/rodo-software/cinderstack-runner/images/default"
	"github.com/rodo-software/cinderstack-runner/internal/name"
	"github.com/rodo-software/cinderstack-runner/internal/workspace"
)

// MySQLImage is the run's database, MySQL 8 as today's compose file, pinned
// by digest (mysql 8.0.46, multi-arch index).
const MySQLImage = "mysql:8.0.46@sha256:7dcddc01f13bab2f15cde676d44d01f61fc9f99fe7785e86196dfc07d358ae2b"

// DBEnv are the variables today's compose file sets, so Laravel's tests in
// the clone reach the run's MySQL; real environment variables win over the
// project's .env file.
var DBEnv = []string{"DB_HOST=mysql", "DB_PORT=3306", "DB_DATABASE=laravel_testing", "DB_USERNAME=laravel", "DB_PASSWORD=laravel"}

// mysqlEnv creates the database and user DBEnv names.
var mysqlEnv = []string{"-e", "MYSQL_DATABASE=laravel_testing", "-e", "MYSQL_USER=laravel", "-e", "MYSQL_PASSWORD=laravel", "-e", "MYSQL_ROOT_PASSWORD=root"}

// tailChars is how much of a run's output is kept for comments, as today.
const tailChars = 4000

// Docker is the default Environment: the default image, a private network
// with a fresh MySQL per run (decision 2026-11-05-runner-rewrite-details
// point 13), and every container and network labelled with the project and
// issue (review item 9).
type Docker struct {
	Project string         // project key, in names and labels
	Exec    workspace.Exec // nil means workspace.RunCommand
}

// Image returns the default image's tag, building it from the embedded
// Dockerfile when missing. The tag is the Dockerfile's hash, so a runner
// with a changed Dockerfile builds its own.
func (d *Docker) Image(ctx context.Context) (string, error) {
	sum := sha256.Sum256(defaultimage.Dockerfile)
	tag := name.Name + "-default:" + hex.EncodeToString(sum[:6])
	if _, err := d.exec(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", tag); err == nil {
		return tag, nil
	}
	dir, err := os.MkdirTemp("", name.Name+"-image-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), defaultimage.Dockerfile, 0o600); err != nil {
		return "", err
	}
	_, err = d.exec(ctx, "docker", "build", "--quiet", "--tag", tag, dir)
	return tag, err
}

// network is the name of the issue's private network; its MySQL is
// network+"-mysql".
func (d *Docker) network(issue string) string {
	return name.Name + "-" + d.Project + "-" + issue
}

// up creates the issue's private network and starts a fresh MySQL on it,
// reachable as mysql. A failure leaves neither behind.
func (d *Docker) up(ctx context.Context, issue string) error {
	labels := workspace.Labels(d.Project, issue)
	net := d.network(issue)
	if _, err := d.exec(ctx, append(append([]string{"docker", "network", "create"}, labels...), net)...); err != nil {
		return err
	}
	argv := append(append([]string{"docker", "run", "--detach", "--rm"}, labels...),
		"--name", net+"-mysql", "--network", net, "--network-alias", "mysql")
	argv = append(append(argv, mysqlEnv...), MySQLImage)
	if _, err := d.exec(ctx, argv...); err != nil {
		return errors.Join(err, d.down(ctx, issue))
	}
	return nil
}

// down removes the issue's MySQL, with its data volume, and its network. It
// tries both, so one failure does not leave the other behind.
func (d *Docker) down(ctx context.Context, issue string) error {
	net := d.network(issue)
	_, err := d.exec(ctx, "docker", "rm", "--force", "--volumes", net+"-mysql")
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
