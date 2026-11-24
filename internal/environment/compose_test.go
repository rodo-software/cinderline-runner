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
		"name": "p",
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
		{"other network", svc("db", func(s *composeService) { s.Networks = map[string]any{"front": nil} }), "joins network front"},
		{"network alias", svc("db", func(s *composeService) {
			s.Networks = map[string]any{"default": map[string]any{"aliases": []any{"proxy"}}}
		}), "service db sets options on its network"},
		{"env_file outside", svc("agent", func(s *composeService) { s.EnvFile[0].Path = "/home/w/.aws/credentials" }), "env_file /home/w/.aws/credentials outside"},
		{"build outside", svc("agent", func(s *composeService) { s.Build.Context = "/home/w" }), "builds from /home/w outside"},
		{"dockerfile outside", svc("agent", func(s *composeService) { s.Build.Dockerfile = "../../../etc/passwd" }), "dockerfile ../../../etc/passwd outside"},
		{"build option", svc("agent", func(s *composeService) { s.Build.keys = append(s.Build.keys, "network") }), "service agent sets build.network"},
		// compose tags what it builds as image: cinderline-proxy:<hash> would replace the egress proxy.
		{"build and image", svc("agent", func(s *composeService) { s.Image = "cinderline-proxy:abc" }), "service agent sets both build and image"},
		{"secret from env", func(c *composeConfig) { c.Secrets["s"] = composeSource{Environment: "GIT_TOKEN"} }, "secret s is taken from the environment"},
		{"secret file outside", func(c *composeConfig) { c.Secrets["s"] = composeSource{File: "/home/w/.ssh/id_ed25519"} }, "secret s reads /home/w/.ssh/id_ed25519 outside"},
		{"external volume", func(c *composeConfig) { c.Volumes["data"] = map[string]any{"name": "data", "external": true} }, "volume data is external"},
		// A local volume with type none and o bind is a bind mount of the host's root.
		{"volume driver_opts", func(c *composeConfig) {
			c.Volumes["data"]["driver_opts"] = map[string]any{"type": "none", "o": "bind", "device": "/"}
		}, "volume data sets driver_opts"},
		{"volume driver", func(c *composeConfig) { c.Volumes["data"]["driver"] = "local" }, "volume data sets driver"},
		// Another name could be the runner's own volume with the clones.
		{"volume name", func(c *composeConfig) { c.Volumes["data"]["name"] = "cinderline-CSR-repo" }, "volume data is named cinderline-CSR-repo, not p_data"},
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

// A service key the runner does not know is refused, as decoded from
// compose's JSON: a list of refused keys let security_opt, runtime and
// device_cgroup_rules through, because the struct had no field for them.
func TestComposeCheckRefusesServiceKeys(t *testing.T) {
	for key, value := range map[string]string{
		"security_opt":        `["seccomp=unconfined"]`,
		"runtime":             `"sysbox-runc"`,
		"device_cgroup_rules": `["c 1:3 mr"]`,
		"privileged":          `true`,
		"cap_add":             `["NET_ADMIN"]`,
		"devices":             `[{"source": "/dev/kvm", "target": "/dev/kvm"}]`,
		"ports":               `[{"target": 5432, "published": "5432"}]`,
		"volumes_from":        `["other"]`,
		"network_mode":        `"host"`,
		"pid":                 `"host"`,
		"ipc":                 `"host"`,
		"uts":                 `"host"`,
		"cgroup":              `"host"`,
		"userns_mode":         `"host"`,
		"cgroup_parent":       `"/"`,
		"extra_hosts":         `["proxy=10.0.0.1"]`,
		"sysctls":             `{"net.ipv4.ip_forward": "1"}`,
		"group_add":           `["0"]`,
		"gpus":                `[{"count": -1}]`,
		"deploy":              `{"resources": {"reservations": {"devices": [{"capabilities": ["gpu"]}]}}}`,
		"use_api_socket":      `true`,
		"post_start":          `[{"command": ["true"], "privileged": true}]`,
		"container_name":      `"cinderline-CSR-CSR-31-proxy"`,
		"logging":             `{"driver": "syslog"}`,
		"dns":                 `["10.0.0.1"]`,
		"links":               `["db"]`,
		"labels":              `{"cinderline.project": "OTHER"}`,
		"storage_opt":         `{"size": "1G"}`,
		"a_key_of_tomorrow":   `1`,
	} {
		t.Run(key, func(t *testing.T) {
			cfg := goodConfig()
			var s composeService
			if err := json.Unmarshal([]byte(`{"image": "postgres:17", "`+key+`": `+value+`}`), &s); err != nil {
				t.Fatal(err)
			}
			cfg.Services["db"] = s
			err := cfg.check(src, "agent")
			if want := "service db sets " + key + ", which the runner does not allow"; err == nil || err.Error() != want {
				t.Errorf("check = %v, want %q", err, want)
			}
		})
	}
}

// What compose's normalised config holds for an ordinary file passes.
func TestComposeCheckAcceptsServiceKeys(t *testing.T) {
	cfg := goodConfig()
	var s composeService
	if err := json.Unmarshal([]byte(`{"image": "mysql:8.0", "command": null, "entrypoint": null, "environment": {"MYSQL_DATABASE": "x"},
		"healthcheck": {"test": ["CMD", "mysqladmin", "ping"]}, "depends_on": {"agent": {"condition": "service_started"}},
		"networks": {"default": null}, "tmpfs": ["/tmp"], "user": "mysql", "restart": "no"}`), &s); err != nil {
		t.Fatal(err)
	}
	cfg.Services["db"] = s
	if err := cfg.check(src, "agent"); err != nil {
		t.Error(err)
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
		case argv[1] == "compose" && slices.Contains(argv, "config"):
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
// agent service: the other services started on the run's internal
// network, the agent hardened as ever in the service's image with its
// environment, and everything removed after.
func TestRunInCompose(t *testing.T) {
	d, calls, override := composeRunner(t, `{
		"services": {
			"agent": {"build": {"context": "SRC"}, "environment": {"DB_HOST": "db", "UNSET": null}},
			"db": {"image": "postgres:17", "volumes": [{"type": "volume", "source": "data"}]}
		},
		"name": "cinderline-csr-csr-31",
		"networks": {"default": {"name": "x"}},
		"volumes": {"data": {"name": "cinderline-csr-csr-31_data"}}
	}`)
	// The build context must lie in the runner's temporary folder: SRC
	// becomes that folder when the config is asked for.
	record := d.Exec
	d.Exec = func(ctx context.Context, env []string, argv ...string) (string, error) {
		out, err := record(ctx, env, argv...)
		if argv[1] == "compose" && slices.Contains(argv, "config") {
			out = strings.ReplaceAll(out, "SRC", filepath.ToSlash(filepath.Dir(argv[5])))
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
			dir = filepath.Dir(filepath.Dir(c.Argv[5]))
			if c.Argv[0] != "docker" || c.Argv[3] != "cinderline-csr-csr-31" {
				t.Errorf("compose call %q, want project cinderline-csr-csr-31", c.Argv)
			}
		}
		switch {
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
	if slices.ContainsFunc(agent, func(a string) bool { return strings.HasPrefix(a, "UNSET") }) {
		t.Errorf("agent %q has an unset variable", agent)
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
	if err == nil || !strings.Contains(err.Error(), "service agent sets ports") {
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
	got, memory, err := d.AgentImage(context.Background())
	if err != nil || got != imageTag() || memory != d.Memory {
		t.Errorf("AgentImage = %q, %d, %v, want the default image and the project's memory", got, memory, err)
	}
}

func TestComposeFileMustBeInTheRepository(t *testing.T) {
	d, _, _ := composeRunner(t, "")
	d.SetCompose(&Compose{Base: "main", File: "../compose.yml", Service: "agent"})
	if _, _, err := d.AgentImage(context.Background()); err == nil {
		t.Error("a file outside the repository was accepted")
	}
}

// Decision 2026-11-17-local-model-providers: a compose environment reaches a
// provider on the runner's own machine as the default one does: through the
// proxy, which alone maps host.docker.internal; the agent's NO_PROXY never
// names it.
func TestRunInComposeReachesALocalProviderThroughTheProxyOnly(t *testing.T) {
	d, calls, _ := composeRunner(t, `{"services": {"agent": {"image": "php:8.4"}, "db": {"image": "postgres:17"}}}`)
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
		t.Errorf("proxy argv = %q", proxy)
	}
	if agent == nil || slices.Contains(agent, "--add-host") || strings.Contains(strings.Join(agent, " "), "host.docker.internal") {
		t.Errorf("agent argv = %q", agent)
	}
	if !hasPair(agent, "-e", "HTTP_PROXY=http://proxy:3128") {
		t.Errorf("agent argv %q does not send plain http through the proxy", agent)
	}
}

// CLR-215, decision 2026-11-24-agent-memory-limit: a mem_limit on the agent
// service, which compose writes as a byte count in a string, wins over the
// project's setting; a plain number is read too. Without one the project's
// setting holds. The runner sets no limit on the other services.
func TestRunInComposeMemLimit(t *testing.T) {
	for _, tt := range []struct{ agent, want string }{
		{`{"image": "php:8.4", "mem_limit": "2147483648"}`, "2147483648"},
		{`{"image": "php:8.4", "mem_limit": 1073741824}`, "1073741824"},
		{`{"image": "php:8.4"}`, "8589934592"},
	} {
		d, calls, _ := composeRunner(t, `{"services": {"agent": `+tt.agent+`, "db": {"image": "postgres:17"}}}`)
		d.Memory = 8 << 30
		var override string
		record := d.Exec
		d.Exec = func(ctx context.Context, env []string, argv ...string) (string, error) {
			if i := slices.Index(argv, "up"); i > 0 {
				b, err := os.ReadFile(argv[i-1])
				if err != nil {
					t.Error(err)
				}
				override = string(b)
			}
			return record(ctx, env, argv...)
		}
		if _, err := d.Run(context.Background(), turn); err != nil {
			t.Fatalf("%s: %v", tt.agent, err)
		}
		var agent []string
		for _, c := range calls() {
			if strings.Contains(strings.Join(c.Argv, " "), "<stdin:") {
				agent = c.Argv
			}
		}
		if !hasPair(agent, "--memory", tt.want) || !hasPair(agent, "--memory-swap", tt.want) {
			t.Errorf("%s: agent argv = %q, want --memory and --memory-swap %s", tt.agent, agent, tt.want)
		}
		if override == "" || strings.Contains(override, "mem") {
			t.Errorf("%s: override %q, want one that sets no memory limit", tt.agent, override)
		}
	}
}

func TestRunInComposeRefusesABadMemLimit(t *testing.T) {
	d, _, _ := composeRunner(t, `{"services": {"agent": {"image": "php:8.4", "mem_limit": "-1"}}}`)
	if _, err := d.Run(context.Background(), turn); err == nil || !strings.Contains(err.Error(), "mem_limit") {
		t.Errorf("Run = %v, want the mem_limit refused", err)
	}
}

// CLR-216, decision 2026-11-24-agent-memory-limit: a run's share of the
// memory budget is the agent container's limit plus each other service's
// mem_limit, or 512 MiB for one without.
func TestAgentImageMemory(t *testing.T) {
	for _, tt := range []struct {
		config string
		want   int64
	}{
		{`{"services": {"agent": {"image": "php:8.4"}}}`, 8 << 30},
		{`{"services": {"agent": {"image": "php:8.4", "mem_limit": "2147483648"}}}`, 2 << 30},
		{`{"services": {"agent": {"image": "php:8.4"}, "db": {"image": "postgres:17"}}}`, 8<<30 + 512<<20},
		{`{"services": {"agent": {"image": "php:8.4", "mem_limit": "1073741824"}, "db": {"image": "postgres:17", "mem_limit": "268435456"}, "cache": {"image": "redis:8"}}}`, 1<<30 + 256<<20 + 512<<20},
	} {
		d, _, _ := composeRunner(t, tt.config)
		d.Memory = 8 << 30
		image, memory, err := d.AgentImage(context.Background())
		if err != nil || image != "php:8.4" || memory != tt.want {
			t.Errorf("%s: AgentImage = %q, %d, %v, want php:8.4, %d", tt.config, image, memory, err, tt.want)
		}
	}
}
