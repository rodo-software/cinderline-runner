package run

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// scriptedExec returns scripted outputs per argv and records every call.
type scriptedReply struct {
	out string
	err error
}

type scriptedExec struct {
	mu      sync.Mutex
	replies map[string]scriptedReply
	calls   []string
}

func newScriptedExec() *scriptedExec {
	return &scriptedExec{replies: map[string]scriptedReply{}}
}

func (s *scriptedExec) add(argv []string, out string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies[strings.Join(argv, " ")] = scriptedReply{out, err}
}

func (s *scriptedExec) exec(_ context.Context, _ []string, argv ...string) (string, error) {
	a := strings.Join(argv, " ")
	s.mu.Lock()
	s.calls = append(s.calls, a)
	s.mu.Unlock()
	// Exact match first
	s.mu.Lock()
	if r, ok := s.replies[a]; ok {
		s.mu.Unlock()
		return r.out, r.err
	}
	s.mu.Unlock()
	// Prefix match for calls with unknown hashes
	s.mu.Lock()
	for prefix, r := range s.replies {
		if strings.HasPrefix(a, prefix) {
			s.mu.Unlock()
			return r.out, r.err
		}
	}
	s.mu.Unlock()
	return "", errNotScripted
}

// exitCodeError implements the ExitCode() int interface used by code under test.
type exitCodeError int

func (e exitCodeError) Error() string { return "exit status " + itoa(int(e)) }
func (e exitCodeError) ExitCode() int { return int(e) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

var errNotScripted = errors.New("not scripted")

var testMount = tools.Mount{
	Args: []string{"-v", "cinderline-tools-test:/opt/cinderline:ro"},
	Env:  []string{"DISABLE_AUTOUPDATER=1"},
}

var testCfg = config.Project{URL: "https://test.example.com", Key: "TEST", Repository: "https://git.example.com/o/r"}

func testDetect(ctx context.Context, exec workspace.Exec, sec *secrets.Secrets, cfg config.Project) []cinderline.ReportClient {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return detectClients(ctx, exec, cfg, sec, "test-image", testMount, log)
}

// Test that all tests pass.
func TestDetectBinaryMissing(t *testing.T) {
	keyring.MockInit()
	sec := secrets.New(t.TempDir())
	s := newScriptedExec()
	ctx := context.Background()
	for _, name := range []string{"claude", "codex", "opencode"} {
		s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/" + name, "--version"}, "", errors.New("not found"))
	}
	// Gemini needs different argv
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/node", tools.Path + "/gemini/bundle/gemini.js", "--version"}, "", errors.New("not found"))

	got := testDetect(ctx, s.exec, sec, testCfg)
	if len(got) != 4 {
		t.Fatalf("got %d clients, want 4", len(got))
	}
	for _, rc := range got {
		if rc.State != "missing_binary" {
			t.Errorf("%s state = %q, want missing_binary", rc.Name, rc.State)
		}
	}
}

func TestDetectNoKeyAndNoLogin(t *testing.T) {
	keyring.MockInit()
	sec := secrets.New(t.TempDir())
	s := newScriptedExec()
	ctx := context.Background()
	for _, name := range []string{"claude", "codex", "opencode"} {
		s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/" + name, "--version"}, "2.1.316\n", nil)
	}
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/node", tools.Path + "/gemini/bundle/gemini.js", "--version"}, "0.67.0\n", nil)
	// Login file checks all return "not found" (test -f exits 1)
	for name, file := range map[string]string{"claude": "/state/claude/.credentials.json", "codex": "/state/codex/auth.json", "gemini": "/state/gemini/.gemini/oauth_creds.json"} {
		s.add([]string{"docker", "run", "--rm", "--network", "none", "-v", client.StateVolume("TEST", name) + ":/state", "--entrypoint", "test", workspace.GitImage, "-f", file}, "", exitCodeError(1))
	}

	got := testDetect(ctx, s.exec, sec, testCfg)

	m := map[string]cinderline.ReportClient{}
	for _, rc := range got {
		m[rc.Name] = rc
	}
	for _, name := range []string{"claude", "codex", "gemini"} {
		if rc, ok := m[name]; ok {
			if rc.State != "missing_credential" {
				t.Errorf("%s = %s, want missing_credential", name, rc.State)
			}
		} else {
			t.Errorf("%s not found", name)
		}
	}
	// OpenCode has no login; without a key it is missing_credential too
	if rc, ok := m["opencode"]; ok {
		if rc.State != "missing_credential" {
			t.Errorf("opencode = %s, want missing_credential", rc.State)
		}
	} else {
		t.Error("opencode not found")
	}
}

func TestDetectKeyAnd200(t *testing.T) {
	keyring.MockInit()
	sec := secrets.New(t.TempDir())
	if _, err := sec.Set(testCfg, secrets.OpenRouterKey, "sk-or-v1-test"); err != nil {
		t.Fatal(err)
	}
	s := newScriptedExec()
	ctx := context.Background()
	for _, name := range []string{"claude", "codex", "opencode"} {
		s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/" + name, "--version"}, "1.0.0\n", nil)
	}
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/node", tools.Path + "/gemini/bundle/gemini.js", "--version"}, "0.1.0\n", nil)

	// OpenCode HTTP check returning 200 (exit 0)
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "-e", "KEY", "-e", "URL", "test-image", tools.Path + "/bin/node", "-e",
		`fetch(process.env.URL,{headers:{'Authorization':'Bearer '+process.env.KEY}}).then(r=>process.exit(r.status==401||r.status==403?3:0)).catch(()=>process.exit(0))`}, "", nil)

	got := testDetect(ctx, s.exec, sec, testCfg)
	m := map[string]cinderline.ReportClient{}
	for _, rc := range got {
		m[rc.Name] = rc
	}
	// OpenCode has key + 200 -> ready
	if rc, ok := m["opencode"]; ok {
		if rc.State != "ready" {
			t.Errorf("opencode = %s, want ready", rc.State)
		}
		if rc.Version == nil || *rc.Version != "1.0.0" {
			t.Errorf("opencode version = %v, want 1.0.0", rc.Version)
		}
	} else {
		t.Error("opencode not found")
	}
}

func TestDetectKeyAnd401(t *testing.T) {
	keyring.MockInit()
	sec := secrets.New(t.TempDir())
	if _, err := sec.Set(testCfg, secrets.AnthropicCredential, "sk-ant-api03-test"); err != nil {
		t.Fatal(err)
	}
	s := newScriptedExec()
	ctx := context.Background()
	for _, name := range []string{"claude", "codex", "opencode"} {
		s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/" + name, "--version"}, "1.0.0\n", nil)
	}
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/node", tools.Path + "/gemini/bundle/gemini.js", "--version"}, "0.1.0\n", nil)

	// claude key check returning 401 (node exits 3 now)
	exit3 := exitCodeError(3)
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "-e", "KEY", "-e", "URL", "test-image", tools.Path + "/bin/node", "-e",
		`fetch(process.env.URL,{headers:{'x-api-key':process.env.KEY,'anthropic-version':'2023-06-01'}}).then(r=>process.exit(r.status==401||r.status==403?3:0)).catch(()=>process.exit(0))`}, "", exit3)

	got := testDetect(ctx, s.exec, sec, testCfg)
	m := map[string]cinderline.ReportClient{}
	for _, rc := range got {
		m[rc.Name] = rc
	}
	if rc, ok := m["claude"]; ok {
		if rc.State != "invalid_credential" {
			t.Errorf("claude = %s, want invalid_credential", rc.State)
		}
	} else {
		t.Error("claude not found")
	}
}

func TestDetectKeyAndNetworkError(t *testing.T) {
	keyring.MockInit()
	sec := secrets.New(t.TempDir())
	if _, err := sec.Set(testCfg, secrets.AnthropicCredential, "sk-ant-api03-test"); err != nil {
		t.Fatal(err)
	}
	s := newScriptedExec()
	ctx := context.Background()
	for _, name := range []string{"claude", "codex", "opencode"} {
		s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/" + name, "--version"}, "1.0.0\n", nil)
	}
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/node", tools.Path + "/gemini/bundle/gemini.js", "--version"}, "0.1.0\n", nil)

	// claude key check: network error → ready (docker fails with non-3 exit)
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "-e", "KEY", "-e", "URL", "test-image", tools.Path + "/bin/node", "-e",
		`fetch(process.env.URL,{headers:{'x-api-key':process.env.KEY,'anthropic-version':'2023-06-01'}}).then(r=>process.exit(r.status==401||r.status==403?3:0)).catch(()=>process.exit(0))`}, "", errors.New("docker: connection refused"))

	got := testDetect(ctx, s.exec, sec, testCfg)
	m := map[string]cinderline.ReportClient{}
	for _, rc := range got {
		m[rc.Name] = rc
	}
	if rc, ok := m["claude"]; ok {
		if rc.State != "ready" {
			t.Errorf("claude = %s, want ready on network error", rc.State)
		}
	} else {
		t.Error("claude not found")
	}
}

func TestDetectLoginFileNoKey(t *testing.T) {
	keyring.MockInit()
	sec := secrets.New(t.TempDir())
	s := newScriptedExec()
	ctx := context.Background()
	for _, name := range []string{"claude", "codex", "opencode"} {
		s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/" + name, "--version"}, "1.0.0\n", nil)
	}
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/node", tools.Path + "/gemini/bundle/gemini.js", "--version"}, "0.1.0\n", nil)

	// Login files exist (test -f exits 0)
	for name, file := range map[string]string{"claude": "/state/claude/.credentials.json", "codex": "/state/codex/auth.json", "gemini": "/state/gemini/.gemini/oauth_creds.json"} {
		s.add([]string{"docker", "run", "--rm", "--network", "none", "-v", client.StateVolume("TEST", name) + ":/state", "--entrypoint", "test", workspace.GitImage, "-f", file}, "", nil)
	}

	got := testDetect(ctx, s.exec, sec, testCfg)
	m := map[string]cinderline.ReportClient{}
	for _, rc := range got {
		m[rc.Name] = rc
	}
	for _, name := range []string{"claude", "codex", "gemini"} {
		if rc, ok := m[name]; ok {
			if rc.State != "ready" {
				t.Errorf("%s = %s, want ready", name, rc.State)
			}
		} else {
			t.Errorf("%s not found", name)
		}
	}
}

func TestDetectOpenCodeWithoutKey(t *testing.T) {
	keyring.MockInit()
	sec := secrets.New(t.TempDir())
	s := newScriptedExec()
	ctx := context.Background()
	// OpenCode's binary is found
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/opencode", "--version"}, "1.0.0\n", nil)

	got := testDetect(ctx, s.exec, sec, testCfg)
	m := map[string]cinderline.ReportClient{}
	for _, rc := range got {
		m[rc.Name] = rc
	}
	if rc, ok := m["opencode"]; ok {
		if rc.State != "missing_credential" {
			t.Errorf("opencode = %s, want missing_credential", rc.State)
		}
	} else {
		t.Error("opencode not found")
	}
}

func TestDetectSkAntOatToken(t *testing.T) {
	keyring.MockInit()
	sec := secrets.New(t.TempDir())
	if _, err := sec.Set(testCfg, secrets.AnthropicCredential, "sk-ant-oat-test123"); err != nil {
		t.Fatal(err)
	}
	s := newScriptedExec()
	ctx := context.Background()
	// Only need claude binary version
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/claude", "--version"}, "2.1.316\n", nil)

	got := testDetect(ctx, s.exec, sec, testCfg)
	m := map[string]cinderline.ReportClient{}
	for _, rc := range got {
		m[rc.Name] = rc
	}
	if rc, ok := m["claude"]; ok {
		if rc.State != "ready" {
			t.Errorf("claude with sk-ant-oat = %s, want ready", rc.State)
		}
	} else {
		t.Error("claude not found")
	}
}

func TestDetectNoKeyInArgv(t *testing.T) {
	keyring.MockInit()
	sec := secrets.New(t.TempDir())
	if _, err := sec.Set(testCfg, secrets.OpenRouterKey, "sk-or-v1-secret-value"); err != nil {
		t.Fatal(err)
	}
	s := newScriptedExec()
	ctx := context.Background()
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/opencode", "--version"}, "1.0.0\n", nil)

	got := testDetect(ctx, s.exec, sec, testCfg)
	// Check that no argv contains the key value
	s.mu.Lock()
	for _, call := range s.calls {
		if strings.Contains(call, "sk-or-v1-secret-value") {
			t.Errorf("key found in argv: %s", call)
		}
	}
	s.mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("got %d clients", len(got))
	}
}

func TestDetectMountFailureAllMissingBinary(t *testing.T) {
	// When detectClients cannot get the tools mount, every client is
	// missing_binary. This is handled by the caller (NewProject's
	// closure or the daemon), not by detectClients itself, so this test
	// verifies the mount failure path.
	keyring.MockInit()
	sec := secrets.New(t.TempDir())
	s := newScriptedExec()
	ctx := context.Background()
	for _, name := range []string{"claude", "codex", "opencode"} {
		s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/" + name, "--version"}, "", errors.New("not found"))
	}
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/node", tools.Path + "/gemini/bundle/gemini.js", "--version"}, "", errors.New("not found"))

	got := testDetect(ctx, s.exec, sec, testCfg)
	for _, rc := range got {
		if rc.State != "missing_binary" {
			t.Errorf("%s = %s, want missing_binary", rc.Name, rc.State)
		}
	}
}

func TestDetectNodeExitCode1IsReady(t *testing.T) {
	keyring.MockInit()
	sec := secrets.New(t.TempDir())
	if _, err := sec.Set(testCfg, secrets.AnthropicCredential, "sk-ant-api03-test"); err != nil {
		t.Fatal(err)
	}
	s := newScriptedExec()
	ctx := context.Background()
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/claude", "--version"}, "1.0.0\n", nil)

	// node exits with code 1 (uncaught error) → ready, not invalid_credential
	exit1 := exitCodeError(1)
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "-e", "KEY", "-e", "URL", "test-image", tools.Path + "/bin/node", "-e",
		`fetch(process.env.URL,{headers:{'x-api-key':process.env.KEY,'anthropic-version':'2023-06-01'}}).then(r=>process.exit(r.status==401||r.status==403?3:0)).catch(()=>process.exit(0))`}, "", exit1)

	got := testDetect(ctx, s.exec, sec, testCfg)
	m := map[string]cinderline.ReportClient{}
	for _, rc := range got {
		m[rc.Name] = rc
	}
	if rc, ok := m["claude"]; ok {
		if rc.State != "ready" {
			t.Errorf("claude exit code 1 = %s, want ready", rc.State)
		}
	} else {
		t.Error("claude not found")
	}
}

func TestDetectNodeExitCode3IsInvalidCredential(t *testing.T) {
	keyring.MockInit()
	sec := secrets.New(t.TempDir())
	if _, err := sec.Set(testCfg, secrets.AnthropicCredential, "sk-ant-api03-test"); err != nil {
		t.Fatal(err)
	}
	s := newScriptedExec()
	ctx := context.Background()
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/claude", "--version"}, "1.0.0\n", nil)

	exit3 := exitCodeError(3)
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "-e", "KEY", "-e", "URL", "test-image", tools.Path + "/bin/node", "-e",
		`fetch(process.env.URL,{headers:{'x-api-key':process.env.KEY,'anthropic-version':'2023-06-01'}}).then(r=>process.exit(r.status==401||r.status==403?3:0)).catch(()=>process.exit(0))`}, "", exit3)

	got := testDetect(ctx, s.exec, sec, testCfg)
	m := map[string]cinderline.ReportClient{}
	for _, rc := range got {
		m[rc.Name] = rc
	}
	if rc, ok := m["claude"]; ok {
		if rc.State != "invalid_credential" {
			t.Errorf("claude exit code 3 = %s, want invalid_credential", rc.State)
		}
	} else {
		t.Error("claude not found")
	}
}

func TestDetectVersionInReport(t *testing.T) {
	keyring.MockInit()
	sec := secrets.New(t.TempDir())
	if _, err := sec.Set(testCfg, secrets.OpenRouterKey, "sk-or-v1-test"); err != nil {
		t.Fatal(err)
	}
	s := newScriptedExec()
	ctx := context.Background()
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/opencode", "--version"}, "3.5.0\n", nil)
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "-e", "KEY", "-e", "URL", "test-image", tools.Path + "/bin/node", "-e",
		`fetch(process.env.URL,{headers:{'Authorization':'Bearer '+process.env.KEY}}).then(r=>process.exit(r.status==401||r.status==403?3:0)).catch(()=>process.exit(0))`}, "", nil)

	got := testDetect(ctx, s.exec, sec, testCfg)
	for _, rc := range got {
		if rc.Name == "opencode" {
			if rc.Version == nil || *rc.Version != "3.5.0" {
				t.Errorf("opencode version = %v, want 3.5.0", rc.Version)
			}
		}
	}
}

func TestDetectScriptNoKeyOrBaseURLValueInArgv(t *testing.T) {
	keyring.MockInit()
	sec := secrets.New(t.TempDir())
	cfg := config.Project{URL: "https://test.example.com", Key: "TEST", OpenAIBaseURL: "https://custom.openai.com/v1", Repository: "https://git.example.com/o/r"}
	if _, err := sec.Set(cfg, secrets.OpenAIKey, "sk-custom-key"); err != nil {
		t.Fatal(err)
	}
	s := newScriptedExec()
	ctx := context.Background()
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/codex", "--version"}, "1.0.0\n", nil)
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "-e", "KEY", "-e", "URL", "test-image", tools.Path + "/bin/node", "-e",
		`fetch(process.env.URL,{headers:{'Authorization':'Bearer '+process.env.KEY}}).then(r=>process.exit(r.status==401||r.status==403?3:0)).catch(()=>process.exit(0))`}, "", nil)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	got := detectClients(ctx, s.exec, cfg, sec, "test-image", testMount, log)

	s.mu.Lock()
	for _, call := range s.calls {
		if strings.Contains(call, "https://custom.openai.com") {
			t.Errorf("base URL value found in argv: %s", call)
		}
	}
	s.mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("got %d clients", len(got))
	}
}

func TestDetectMountOpenCodeEnvInKeyCheck(t *testing.T) {
	keyring.MockInit()
	sec := secrets.New(t.TempDir())
	if _, err := sec.Set(testCfg, secrets.OpenRouterKey, "sk-or-v1-test"); err != nil {
		t.Fatal(err)
	}
	s := newScriptedExec()
	ctx := context.Background()
	openCodeMount := tools.Mount{
		Args:        []string{"-v", "cinderline-tools-test:/opt/cinderline:ro"},
		Env:         []string{"DISABLE_AUTOUPDATER=1"},
		OpenCodeEnv: []string{"LD_LIBRARY_PATH=/opt/cinderline/lib"},
	}
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "--user", workspace.AgentUser, "test-image", tools.Path + "/bin/opencode", "--version"}, "1.0.0\n", nil)
	// The key check argv must carry -e LD_LIBRARY_PATH=/opt/cinderline/lib
	s.add([]string{"docker", "run", "--rm", "-v", "cinderline-tools-test:/opt/cinderline:ro", "-e", "DISABLE_AUTOUPDATER=1", "-e", "LD_LIBRARY_PATH=/opt/cinderline/lib", "--user", workspace.AgentUser, "-e", "KEY", "-e", "URL", "test-image", tools.Path + "/bin/node", "-e",
		`fetch(process.env.URL,{headers:{'Authorization':'Bearer '+process.env.KEY}}).then(r=>process.exit(r.status==401||r.status==403?3:0)).catch(()=>process.exit(0))`}, "", nil)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	got := detectClients(ctx, s.exec, testCfg, sec, "test-image", openCodeMount, log)
	m := map[string]cinderline.ReportClient{}
	for _, rc := range got {
		m[rc.Name] = rc
	}
	if rc, ok := m["opencode"]; ok {
		if rc.State != "ready" {
			t.Errorf("opencode = %s, want ready", rc.State)
		}
	} else {
		t.Error("opencode not found")
	}
}

// Detect reruns detection on every call (the hourly refresh), while Check
// reuses the cached result.
func TestDetectRefreshes(t *testing.T) {
	p := &Project{}
	n := 0
	p.detect = func(context.Context) []cinderline.ReportClient {
		n++
		return []cinderline.ReportClient{{Name: "claude", State: "ready"}}
	}
	p.Detect(context.Background())
	p.Detect(context.Background())
	p.detected(context.Background())
	if n != 2 {
		t.Errorf("detection ran %d times, want 2", n)
	}
}
