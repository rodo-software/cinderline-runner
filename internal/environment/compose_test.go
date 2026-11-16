package environment

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// tarOf is a tar stream with the given regular files and a symlink.
func tarOf(t *testing.T, files map[string]string) string {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for n, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tw, body)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	return b.String()
}

func TestUntarLeavesSymlinksOut(t *testing.T) {
	dir := t.TempDir()
	if err := untar(dir, tarOf(t, map[string]string{".cinderline/compose.yml": "services: {}\n"})); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".cinderline", "compose.yml")); err != nil || string(b) != "services: {}\n" {
		t.Errorf("compose.yml = %q, %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "link")); !os.IsNotExist(err) {
		t.Errorf("symlink written: %v", err)
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o644, Typeflag: tar.TypeReg})
	tw.Close()
	if err := untar(t.TempDir(), b.String()); err == nil {
		t.Error("a name outside the folder was written")
	}
}

func TestComposeEnvIsMinimal(t *testing.T) {
	got := composeEnv([]string{"PATH=/bin", "HOME=/home/w", "ANTHROPIC_API_KEY=sk", "DOCKER_HOST=unix:///x", "Path=C:\\x", "COMPOSE_FILE=evil.yml"})
	if want := []string{"PATH=/bin", "HOME=/home/w", "DOCKER_HOST=unix:///x", "Path=C:\\x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("composeEnv = %q, want %q", got, want)
	}
}

const src = "/tmp/x/src"

// goodConfig is a normalised config the runner accepts.
func goodConfig() composeConfig {
	var cfg composeConfig
	if err := json.Unmarshal([]byte(`{
		"services": {
			"agent": {"build": {"context": "/tmp/x/src", "dockerfile": "Dockerfile"}, "environment": {"DB_HOST": "db", "UNSET": null},
				"env_file": [{"path": "/tmp/x/src/.env.testing"}], "networks": {"default": null}},
			"db": {"image": "postgres:17", "volumes": [{"type": "volume", "source": "data", "target": "/var/lib/postgresql"}, {"type": "tmpfs", "target": "/tmp"}]}
		},
		"networks": {"default": {"name": "p_default"}},
		"volumes": {"data": {"name": "p_data"}},
		"secrets": {"s": {"file": "/tmp/x/src/secret.txt"}}
	}`), &cfg); err != nil {
		panic(err)
	}
	return cfg
}

func TestComposeCheckAccepts(t *testing.T) {
	if err := goodConfig().check(src, "agent"); err != nil {
		t.Error(err)
	}
}

// Decision 2026-11-08-m6-environments point 1 and the follow-ups of CSR-52:
// each one is refused, with the service named.
func TestComposeCheckRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*composeConfig)
		want string
	}{
		{"no agent service", func(c *composeConfig) { delete(c.Services, "agent") }, `no agent service "agent"`},
		{"bind mount", func(c *composeConfig) {
			s := c.Services["db"]
			s.Volumes[0].Type, s.Volumes[0].Source = "bind", "/home/w"
			c.Services["db"] = s
		}, "service db mounts /home/w (bind)"},
		{"privileged", svc("db", func(s *composeService) { s.Privileged = true }), "service db is privileged"},
		{"host network", svc("db", func(s *composeService) { s.NetworkMode = "host" }), "network_mode host"},
		{"other network", svc("db", func(s *composeService) { s.Networks = map[string]any{"front": nil} }), "joins network front"},
		{"host pid", svc("db", func(s *composeService) { s.Pid = "host" }), "sets pid: host"},
		{"host uts", svc("db", func(s *composeService) { s.Uts = "host" }), "sets uts: host"},
		{"host cgroup", svc("db", func(s *composeService) { s.Cgroup = "host" }), "sets cgroup: host"},
		{"cap_add", svc("db", func(s *composeService) { s.CapAdd = []string{"NET_ADMIN"} }), "adds capabilities"},
		{"devices", svc("db", func(s *composeService) { s.Devices = []json.RawMessage{[]byte(`"/dev/kvm"`)} }), "maps devices"},
		{"ports", svc("db", func(s *composeService) { s.Ports = []json.RawMessage{[]byte(`{"target": 5432}`)} }), "publishes ports"},
		{"volumes_from", svc("db", func(s *composeService) { s.VolumesFrom = []string{"other"} }), "uses volumes_from"},
		{"env_file outside", svc("agent", func(s *composeService) { s.EnvFile[0].Path = "/home/w/.aws/credentials" }), "env_file /home/w/.aws/credentials outside"},
		{"build outside", svc("agent", func(s *composeService) { s.Build.Context = "/home/w" }), "builds from /home/w outside"},
		{"secret from env", func(c *composeConfig) { c.Secrets["s"] = composeSource{Environment: "GIT_TOKEN"} }, "secret s is taken from the environment"},
		{"secret file outside", func(c *composeConfig) { c.Secrets["s"] = composeSource{File: "/home/w/.ssh/id_ed25519"} }, "secret s reads /home/w/.ssh/id_ed25519 outside"},
		{"external volume", func(c *composeConfig) {
			c.Volumes["data"] = struct {
				External bool `json:"external"`
			}{true}
		}, "volume data is external"},
		{"service named proxy", func(c *composeConfig) { c.Services["proxy"] = composeService{Image: "x"} }, "service proxy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := goodConfig()
			tc.edit(&cfg)
			err := cfg.check(src, "agent")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("check = %v, want %q", err, tc.want)
			}
		})
	}
}

func svc(name string, edit func(*composeService)) func(*composeConfig) {
	return func(c *composeConfig) {
		s := c.Services[name]
		edit(&s)
		c.Services[name] = s
	}
}

// composeRunner is runner with the base branch's tar and the normalised
// config scripted; it records the override the runner wrote.
func composeRunner(t *testing.T, config string) (*Docker, func() []execCall, *string) {
	t.Helper()
	d, calls, _ := runner(nil, func(_ context.Context, out io.Writer) error {
		io.WriteString(out, "done\n")
		return nil
	})
	tarball := tarOf(t, map[string]string{".cinderline/compose.yml": "services: {}\n"})
	var mu sync.Mutex
	override := new(string)
	record := d.Exec
	d.Exec = func(ctx context.Context, env []string, argv ...string) (string, error) {
		out, err := record(ctx, env, argv...)
		a := strings.Join(argv, " ")
		switch {
		case strings.Contains(a, " archive --format=tar refs/heads/main"):
			return tarball, err
		case strings.HasSuffix(a, " config --format json"):
			return config, err
		case argv[1] == "compose" && slices.Contains(argv, "build"):
			b, rerr := os.ReadFile(argv[slices.Index(argv, "build")-1])
			if rerr != nil {
				t.Error(rerr)
			}
			mu.Lock()
			*override = string(b)
			mu.Unlock()
		}
		return out, err
	}
	d.SetCompose(&Compose{Base: "main", File: DefaultComposeFile, Service: DefaultAgentService})
	return d, calls, override
}

// A project with environment docker runs its turn in its own compose file's
// agent service: no MySQL, the other services started on the run's internal
// network, the agent hardened as ever in the service's image with its
// environment, and everything removed after.
func TestRunInCompose(t *testing.T) {
	d, calls, override := composeRunner(t, `{
		"services": {
			"agent": {"build": {"context": "SRC"}, "environment": {"DB_HOST": "db", "UNSET": null}},
			"db": {"image": "postgres:17", "volumes": [{"type": "volume", "source": "data"}]}
		},
		"networks": {"default": {"name": "x"}},
		"volumes": {"data": {}}
	}`)
	// The build context must lie in the runner's temporary folder: SRC
	// becomes that folder when the config is asked for.
	record := d.Exec
	d.Exec = func(ctx context.Context, env []string, argv ...string) (string, error) {
		out, err := record(ctx, env, argv...)
		if strings.HasSuffix(strings.Join(argv, " "), " config --format json") {
			out = strings.ReplaceAll(out, "SRC", filepath.ToSlash(filepath.Dir(filepath.Dir(argv[5]))))
		}
		return out, err
	}
	out, err := d.Run(context.Background(), turn)
	if err != nil {
		t.Fatal(err)
	}
	if out != "done\n" {
		t.Errorf("output = %q", out)
	}
	got := calls()
	var steps []string
	var agent, up, down []string
	var dir string
	for _, c := range got {
		a := strings.Join(c.Argv, " ")
		if strings.Contains(a, secret) || (c.Argv[1] == "compose" && slices.ContainsFunc(c.Env, func(kv string) bool { return strings.Contains(kv, secret) })) {
			t.Errorf("secret reaches %q with env %q", c.Argv, c.Env)
		}
		if c.Argv[1] == "compose" {
			if !reflect.DeepEqual(c.Env, composeEnv(os.Environ())) {
				t.Errorf("compose env = %q, want the minimal one", c.Env)
			}
			dir = filepath.Dir(filepath.Dir(filepath.Dir(c.Argv[5])))
			if c.Argv[0] != "docker" || c.Argv[3] != "cinderline-csr-csr-31" {
				t.Errorf("compose call %q, want project cinderline-csr-csr-31", c.Argv)
			}
		}
		switch {
		case strings.Contains(a, "mysql"):
			if strings.Contains(a, "--network-alias mysql") {
				t.Errorf("MySQL started: %q", c.Argv)
			}
		case strings.Contains(a, "<stdin:"):
			agent = c.Argv
		case strings.Contains(a, " up --detach --wait"):
			up = c.Argv
		case strings.Contains(a, " down --volumes"):
			down = c.Argv
		}
		steps = append(steps, strings.Join(c.Argv[1:3], " "))
	}
	if want := []string{"run --detach", "network connect"}; !slices.Contains(steps, want[0]) || !slices.Contains(steps, want[1]) {
		t.Errorf("steps %q lack the proxy", steps)
	}
	if up == nil || up[len(up)-1] != "db" {
		t.Errorf("up = %q, want db started", up)
	}
	if down == nil {
		t.Error("compose project not taken down")
	}
	if last := got[len(got)-1].Argv; !reflect.DeepEqual(last, []string{"docker", "network", "rm", "cinderline-CSR-CSR-31"}) {
		t.Errorf("last call %q, want the network removed", last)
	}
	if _, err := os.Stat(dir); dir == "" || !os.IsNotExist(err) {
		t.Errorf("temporary folder %q left: %v", dir, err)
	}
	for _, kv := range []string{"DB_HOST=db", "NO_PROXY=db,localhost,127.0.0.1", "HTTPS_PROXY=http://proxy:3128"} {
		if !hasPair(agent, "-e", kv) {
			t.Errorf("agent %q lacks -e %s", agent, kv)
		}
	}
	for _, flag := range [][2]string{{"--cap-drop", "ALL"}, {"--user", "1000:1000"}, {"--network", "cinderline-CSR-CSR-31"}, {"-e", "ANTHROPIC_API_KEY"}} {
		if !hasPair(agent, flag[0], flag[1]) {
			t.Errorf("agent %q lacks %s %s", agent, flag[0], flag[1])
		}
	}
	if hasPair(agent, "-e", "DB_HOST=mysql") || slices.ContainsFunc(agent, func(a string) bool { return strings.HasPrefix(a, "UNSET") }) {
		t.Errorf("agent %q has the default database or an unset variable", agent)
	}
	if i := slices.Index(agent, "claude"); i < 1 || agent[i-1] != "cinderline-csr-agent" {
		t.Errorf("agent %q, want the built agent image before the command", agent)
	}
	var o struct {
		Networks map[string]struct {
			Name     string
			External bool
		}
		Services map[string]struct {
			Labels map[string]string
			Image  string
		}
		Volumes map[string]struct{ Labels map[string]string }
	}
	if err := json.Unmarshal([]byte(*override), &o); err != nil {
		t.Fatalf("override %q: %v", *override, err)
	}
	if n := o.Networks["default"]; n.Name != "cinderline-CSR-CSR-31" || !n.External {
		t.Errorf("override network = %+v, want the run's internal network", n)
	}
	if s := o.Services["db"]; s.Labels["cinderline.project"] != "CSR" || s.Labels["cinderline.issue"] != "CSR-31" {
		t.Errorf("db labels = %v", s.Labels)
	}
	if o.Services["agent"].Image != "cinderline-csr-agent" || o.Volumes["data"].Labels["cinderline.compose.CSR"] != "1" {
		t.Errorf("override = %s", *override)
	}
}

// A refused file runs no agent and leaves nothing behind.
func TestRunInComposeRefused(t *testing.T) {
	d, calls, _ := composeRunner(t, `{"services": {"agent": {"image": "php:8.4", "ports": [{"target": 80, "published": "8080"}]}}}`)
	_, err := d.Run(context.Background(), turn)
	if err == nil || !strings.Contains(err.Error(), "service agent publishes ports") {
		t.Fatalf("Run = %v, want the ports refused", err)
	}
	got := calls()
	for _, c := range got {
		if strings.Contains(strings.Join(c.Argv, " "), "<stdin:") || slices.Contains(c.Argv, "up") {
			t.Errorf("ran %q", c.Argv)
		}
	}
	if last := got[len(got)-1].Argv; !reflect.DeepEqual(last, []string{"docker", "network", "rm", "cinderline-CSR-CSR-31"}) {
		t.Errorf("last call %q, want the network removed", last)
	}
}

// Without a compose file the default environment is unchanged.
func TestSetComposeNilIsTheDefault(t *testing.T) {
	d, _, _ := composeRunner(t, "")
	d.SetCompose(nil)
	got, err := d.AgentImage(context.Background())
	if err != nil || got != imageTag() {
		t.Errorf("AgentImage = %q, %v, want the default image", got, err)
	}
}

func TestComposeFileMustBeInTheRepository(t *testing.T) {
	d, _, _ := composeRunner(t, "")
	d.SetCompose(&Compose{Base: "main", File: "../compose.yml", Service: "agent"})
	if _, err := d.AgentImage(context.Background()); err == nil {
		t.Error("a file outside the repository was accepted")
	}
}
