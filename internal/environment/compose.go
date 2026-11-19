package environment

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// The defaults of a project's compose environment when Cinderline names
// none (decision 2026-11-08-m6-environments point 1).
const (
	DefaultComposeFile  = "." + name.Name + "/compose.yml"
	DefaultAgentService = "agent"
)

// Compose is a project's own compose file, the environment docker
// (decision 2026-11-08-m6-environments point 1). The file is read from the
// base branch only, so a pull request cannot change what runs for its own
// issue.
type Compose struct {
	Base    string // the base branch
	File    string // the file's path in the repository
	Service string // the service the agent runs in
}

// SetCompose makes the turns that start from now on run in c, or in the
// default environment when c is nil.
func (d *Docker) SetCompose(c *Compose) { d.compose.Store(c) }

// AgentImage is the image agent turns run in: the agent service's,
// pulled or built, or the default image.
func (d *Docker) AgentImage(ctx context.Context) (string, error) {
	c := d.compose.Load()
	if c == nil {
		return d.Image(ctx)
	}
	p, err := d.composeProject(ctx, "", c)
	if p != nil {
		defer os.RemoveAll(p.dir)
	}
	if err != nil {
		return "", err
	}
	return p.image, nil
}

// composed is a checked compose project for one turn.
type composed struct {
	dir      string   // the temporary folder with the base branch's files; remove it after down
	args     []string // docker compose and its project and file flags
	image    string   // the agent service's image
	env      []string // the agent service's environment, then ProxyEnv's for its services
	services []string // the services besides the agent's
}

// composeProject reads c's file from the base branch in the repo volume
// into a temporary folder, checks its normalised config, writes the
// runner's override and pulls or builds the agent service's image. The
// override puts every service on the issue's internal network, made by up,
// and labels them for Cleanup. On an error the caller still removes p.dir
// when p is not nil.
func (d *Docker) composeProject(ctx context.Context, issue string, c *Compose) (*composed, error) {
	file := filepath.FromSlash(c.File)
	if !filepath.IsLocal(file) || c.Service == "" {
		return nil, fmt.Errorf("compose file %q, agent service %q: the file must be a path inside the repository", c.File, c.Service)
	}
	dir, err := os.MkdirTemp("", name.Name+"-compose-")
	if err != nil {
		return nil, err
	}
	p := &composed{dir: dir}
	// Paths in the normalised config are compared with src, so no symlink in it.
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return p, err
	}
	src := filepath.Join(dir, "src")
	// shortcut: the whole base tree passes through memory on every turn; cache it by commit if large repositories make turns slow.
	tarball, err := d.exec(ctx, append(append([]string{"docker", "run", "--rm"}, workspace.Labels(d.Project, issue)...),
		"--network", "none", "-v", workspace.Volume(d.Project, "repo")+":"+workspace.RepoPath+":ro",
		"--entrypoint", "git", workspace.GitImage, "-C", workspace.RepoPath, "archive", "--format=tar", "refs/heads/"+c.Base)...)
	if err != nil {
		return p, fmt.Errorf("reading %s from %s: %w", c.File, c.Base, err)
	}
	if err := untar(src, tarball); err != nil {
		return p, fmt.Errorf("reading %s from %s: %w", c.File, c.Base, err)
	}

	if err := checkComposeFiles(src, filepath.Join(src, file)); err != nil {
		return p, fmt.Errorf("compose file %s on %s refused: %w", c.File, c.Base, err)
	}

	// Explicit dotenv input prevents Compose reading the runner's working directory.
	envFile := filepath.Join(src, ".env")
	if _, err := os.Stat(envFile); os.IsNotExist(err) {
		envFile = filepath.Join(dir, "empty.env")
		if err := os.WriteFile(envFile, nil, 0o600); err != nil {
			return p, err
		}
	} else if err != nil {
		return p, err
	}

	project := strings.ToLower(d.network(issue))
	p.args = []string{"docker", "compose", "--project-name", project, "--file", filepath.Join(src, file)}
	p.args = append(p.args, "--env-file", envFile)
	// Resolve includes without importing their dotenv values into the frozen file.
	out, err := d.composeExec(ctx, append(slices.Clip(p.args), "config", "--no-interpolate", "--no-env-resolution", "--format", "json")...)
	if err != nil {
		return p, fmt.Errorf("compose file %s: %w", c.File, err)
	}
	frozen, err := os.CreateTemp(src, "compose-*.json")
	if err != nil {
		return p, err
	}
	_, writeErr := frozen.WriteString(out)
	if err := errors.Join(writeErr, frozen.Close()); err != nil {
		return p, err
	}
	p.args[5] = frozen.Name()
	// Keep env_file paths visible until the path check has approved them.
	out, err = d.composeExec(ctx, append(slices.Clip(p.args), "config", "--no-env-resolution", "--format", "json")...)
	if err != nil {
		return p, fmt.Errorf("compose file %s: %w", c.File, err)
	}
	var cfg composeConfig
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		return p, fmt.Errorf("compose file %s: reading its config: %w", c.File, err)
	}
	if err := cfg.check(src, c.Service); err != nil {
		return p, fmt.Errorf("compose file %s on %s refused: %w", c.File, c.Base, err)
	}

	out, err = d.composeExec(ctx, append(slices.Clip(p.args), "config", "--format", "json")...)
	if err != nil {
		return p, fmt.Errorf("compose file %s: %w", c.File, err)
	}
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		return p, fmt.Errorf("compose file %s: reading its config: %w", c.File, err)
	}
	if err := cfg.check(src, c.Service); err != nil {
		return p, fmt.Errorf("compose file %s on %s refused: %w", c.File, c.Base, err)
	}

	agent := cfg.Services[c.Service]
	p.image = agent.Image
	override := map[string]any{"networks": map[string]any{"default": map[string]any{"name": d.network(issue), "external": true}}}
	labels := map[string]string{}
	l := workspace.Labels(d.Project, issue)
	for i := 1; i < len(l); i += 2 {
		k, v, _ := strings.Cut(l[i], "=")
		labels[k] = v
	}
	services := map[string]any{}
	for s := range cfg.Services {
		o := map[string]any{"labels": labels}
		if s == c.Service && agent.Build != nil && p.image == "" {
			p.image = name.Name + "-" + strings.ToLower(d.Project) + "-agent"
			o["image"] = p.image
		}
		services[s] = o
		if s != c.Service {
			p.services = append(p.services, s)
		}
	}
	override["services"] = services
	if len(cfg.Volumes) > 0 {
		vols := map[string]any{}
		for v := range cfg.Volumes {
			vols[v] = map[string]any{"labels": map[string]string{composeLabel(d.Project): "1"}}
		}
		override["volumes"] = vols
	}
	b, err := json.Marshal(override)
	if err != nil {
		return p, err
	}
	overrideFile := filepath.Join(dir, "override.json")
	if err := os.WriteFile(overrideFile, b, 0o600); err != nil {
		return p, err
	}
	p.args = append(p.args, "--file", overrideFile)

	if agent.Build != nil {
		_, err = d.composeExec(ctx, append(slices.Clip(p.args), "build", c.Service)...)
	} else if _, ierr := d.exec(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", p.image); ierr != nil {
		_, err = d.exec(ctx, "docker", "pull", "--quiet", p.image)
	}
	if err != nil {
		return p, fmt.Errorf("agent service %s: %w", c.Service, err)
	}

	slices.Sort(p.services)
	for _, k := range slices.Sorted(maps.Keys(agent.Environment)) {
		// A variable without a value would be taken from compose's own environment, which is minimal.
		if v := agent.Environment[k]; v != nil {
			p.env = append(p.env, k+"="+*v)
		}
	}
	noProxy := strings.Join(append(slices.Clip(p.services), "localhost", "127.0.0.1"), ",")
	for _, kv := range ProxyEnv {
		if k, _, _ := strings.Cut(kv, "="); strings.EqualFold(k, "NO_PROXY") {
			kv = k + "=" + noProxy
		}
		p.env = append(p.env, kv)
	}
	return p, nil
}

// composeUp starts the services besides the agent's on the issue's
// network, waiting until they run or are healthy.
func (d *Docker) composeUp(ctx context.Context, p *composed) error {
	if len(p.services) == 0 {
		return nil
	}
	_, err := d.composeExec(ctx, append(append(slices.Clip(p.args), "up", "--detach", "--wait"), p.services...)...)
	return err
}

// composeDown removes the project's containers and volumes; the network is
// the runner's and down removes it.
func (d *Docker) composeDown(ctx context.Context, p *composed) error {
	_, err := d.composeExec(ctx, append(slices.Clip(p.args), "down", "--volumes", "--remove-orphans")...)
	return err
}

// composeLabel marks the volumes of compose projects, which Cleanup removes;
// the clone and state volumes carry only the project label.
func composeLabel(project string) string { return name.Name + ".compose." + project }

// composeConfig is the part of `docker compose config --format json` the
// runner checks and uses.
type composeConfig struct {
	Name     string                    `json:"name"` // the project name, which starts every volume's name
	Services map[string]composeService `json:"services"`
	Networks map[string]struct {
		External bool `json:"external"`
	} `json:"networks"`
	Volumes map[string]map[string]any `json:"volumes"`
	Secrets map[string]composeSource  `json:"secrets"`
	Configs map[string]composeSource  `json:"configs"`
}

type composeSource struct {
	File        string `json:"file"`
	Environment string `json:"environment"`
}

type composeService struct {
	Image       string             `json:"image"`
	Build       *composeBuild      `json:"build"`
	Environment map[string]*string `json:"environment"`
	EnvFile     []struct {
		Path string `json:"path"`
	} `json:"env_file"`
	Networks map[string]any `json:"networks"`
	Volumes  []struct {
		Type   string `json:"type"`
		Source string `json:"source"`
	} `json:"volumes"`

	keys []string // every key the service sets
}

type composeBuild struct {
	Context            string            `json:"context"`
	Dockerfile         string            `json:"dockerfile"`
	AdditionalContexts map[string]string `json:"additional_contexts"`

	keys []string // every key the build sets
}

// serviceKeys and buildKeys are the only keys a service and its build may
// set: what the runner checks below or knows cannot reach the host or pass
// the egress proxy. Decoding into a struct drops unknown keys silently, so a
// list of refused keys misses whatever compose adds next; every other key
// is refused (security_opt, runtime, devices, privileged, ports, ...).
var (
	serviceKeys = []string{
		"build", "cap_drop", "command", "configs", "cpus", "depends_on", "entrypoint", "env_file", "environment",
		"expose", "healthcheck", "image", "init", "mem_limit", "networks", "pids_limit", "platform", "pull_policy",
		"read_only", "restart", "secrets", "shm_size", "stdin_open", "stop_grace_period", "stop_signal", "tmpfs",
		"tty", "ulimits", "user", "volumes", "working_dir",
	}
	buildKeys = []string{"additional_contexts", "args", "context", "dockerfile", "dockerfile_inline", "target"}
)

func (s *composeService) UnmarshalJSON(b []byte) error {
	type plain composeService
	s.keys = objectKeys(b)
	return json.Unmarshal(b, (*plain)(s))
}

func (c *composeBuild) UnmarshalJSON(b []byte) error {
	type plain composeBuild
	c.keys = objectKeys(b)
	return json.Unmarshal(b, (*plain)(c))
}

// objectKeys is the keys of the JSON object b, sorted; none when b is no object.
func objectKeys(b []byte) []string {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	return slices.Sorted(maps.Keys(m))
}

// check refuses what would give a run's containers the host or a way out
// past the egress proxy (decision 2026-11-08-m6-environments point 1, and
// the follow-ups of CSR-52 on CSR-168): every service and build key outside
// serviceKeys and buildKeys, bind mounts and every host path outside the
// repository, other or external networks, volumes that are external, have a
// driver or options or a name of their own, an image name for a built
// service, and secrets taken from the environment. It names every problem,
// so one fix round is enough.
func (cfg composeConfig) check(src, agent string) error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if s, ok := cfg.Services[agent]; !ok {
		bad("there is no agent service %q", agent)
	} else if s.Image == "" && s.Build == nil {
		bad("agent service %s has neither image nor build", agent)
	}
	if _, ok := cfg.Services["proxy"]; ok {
		bad("service proxy: the name is the runner's egress proxy's")
	}
	outside := func(path string) bool {
		rel, err := filepath.Rel(src, path)
		return err != nil || !filepath.IsLocal(rel)
	}
	for _, n := range slices.Sorted(maps.Keys(cfg.Services)) {
		s := cfg.Services[n]
		for _, k := range s.keys {
			if !slices.Contains(serviceKeys, k) {
				bad("service %s sets %s, which the runner does not allow", n, k)
			}
		}
		for net, opts := range s.Networks {
			if net != "default" {
				bad("service %s joins network %s", n, net)
			} else if opts != nil {
				// An alias could answer to the proxy's name.
				bad("service %s sets options on its network", n)
			}
		}
		for _, v := range s.Volumes {
			if v.Type != "volume" && v.Type != "tmpfs" {
				bad("service %s mounts %s (%s): only volumes and tmpfs are allowed", n, v.Source, v.Type)
			}
		}
		for _, f := range s.EnvFile {
			if outside(f.Path) {
				bad("service %s reads env_file %s outside the repository", n, f.Path)
			}
		}
		if s.Build != nil {
			for _, k := range s.Build.keys {
				if !slices.Contains(buildKeys, k) {
					bad("service %s sets build.%s, which the runner does not allow", n, k)
				}
			}
			if s.Image != "" {
				// compose tags what it builds with image: it could replace the proxy's or the default image.
				bad("service %s sets both build and image: the runner names a built image", n)
			}
			ctxs := []string{s.Build.Context}
			for _, c := range s.Build.AdditionalContexts {
				ctxs = append(ctxs, c)
			}
			for _, c := range ctxs {
				// Remote and image contexts name no host path.
				if c != "" && !strings.Contains(c, "://") && !strings.HasPrefix(c, "service:") && outside(c) {
					bad("service %s builds from %s outside the repository", n, c)
				}
			}
			if f := s.Build.Dockerfile; f != "" {
				if !filepath.IsAbs(f) {
					f = filepath.Join(s.Build.Context, f)
				}
				if !strings.Contains(s.Build.Context, "://") && outside(f) {
					bad("service %s builds with dockerfile %s outside the repository", n, s.Build.Dockerfile)
				}
			}
		}
	}
	for n, nw := range cfg.Networks {
		if n != "default" || nw.External {
			bad("network %s: services share only the run's own network", n)
		}
	}
	for n, v := range cfg.Volumes {
		for k := range v {
			// A driver with driver_opts (type none, o bind) is a bind mount of any host path.
			if k != "name" && k != "external" {
				bad("volume %s sets %s: only plain volumes are allowed", n, k)
			}
		}
		if v["external"] == true {
			bad("volume %s is external", n)
		} else if want := cfg.Name + "_" + n; v["name"] != want {
			// Another name could be a volume that exists: the runner's clones, another project's data.
			bad("volume %s is named %v, not %s", n, v["name"], want)
		}
	}
	for kind, m := range map[string]map[string]composeSource{"secret": cfg.Secrets, "config": cfg.Configs} {
		for n, s := range m {
			if s.Environment != "" {
				bad("%s %s is taken from the environment", kind, n)
			}
			if s.File != "" && outside(s.File) {
				bad("%s %s reads %s outside the repository", kind, n, s.File)
			}
		}
	}
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errors.Join(errs...)
}

// untar writes the regular files and folders of a tar stream under dir.
// Symlinks are left out, so no path compose reads can leave the folder;
// os.Root refuses names that would.
func untar(dir, tarball string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(strings.NewReader(tarball))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		n := filepath.FromSlash(strings.TrimSuffix(h.Name, "/"))
		switch h.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(n, 0o700)
		case tar.TypeReg:
			err = writeFile(root, n, h.FileInfo().Mode().Perm()|0o600, tr)
		}
		if err != nil {
			return err
		}
	}
}

func writeFile(root *os.Root, n string, mode os.FileMode, r io.Reader) error {
	if d := filepath.Dir(n); d != "." {
		if err := root.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	f, err := root.OpenFile(n, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	return errors.Join(err, f.Close())
}

// composeEnvKeys are the only variables docker compose gets from the
// runner's environment: what the docker CLI needs to find its daemon,
// config and plugins. Nothing else may reach the file's interpolation.
var composeEnvKeys = []string{
	"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "XDG_RUNTIME_DIR",
	"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY",
	"SystemRoot", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "ProgramData", "ProgramFiles", "PATHEXT",
}

// composeEnv is the runner's environment cut down to composeEnvKeys.
func composeEnv(environ []string) []string {
	var env []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if slices.ContainsFunc(composeEnvKeys, func(key string) bool { return strings.EqualFold(k, key) }) {
			env = append(env, kv)
		}
	}
	return env
}

// composeExec runs a docker compose command with composeEnv as its whole
// environment. Tests' Exec gets that environment as its env.
func (d *Docker) composeExec(ctx context.Context, argv ...string) (string, error) {
	env := composeEnv(os.Environ())
	if d.Exec != nil {
		return d.Exec(ctx, env, argv...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s: %w\n%s", strings.Join(argv, " "), err, tail(stdout.String()+stderr.String(), 20, 2000))
	}
	return stdout.String(), nil
}
