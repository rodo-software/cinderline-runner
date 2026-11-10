//go:build integration

package run

// The M8 check of plan section 3 (CSR-63): an agent reaches what its run
// needs and nothing else. Real Docker, the real proxy image built from
// images/proxy and the default image as the agent. It needs outbound HTTPS
// to example.com and example.net. The integration CI job
// (.github/workflows/integration.yml) runs it with the M4 test:
// -run Integration ./internal/run/.

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/environment"
)

// allowlistScript prints "<check>: reached" or "<check>: refused" per line.
// MySQL stays reachable by name on the internal network; the proxy lets
// example.com through and nothing else; a process that ignores the proxy
// variables has no route and no DNS; host.docker.internal is gone (Willem's
// decision on CSR-63, option C); unshare -Urm is refused by the default
// seccomp profile without capabilities (the Cohere case in CSR-63).
const allowlistScript = `r() { if "$@" >/dev/null 2>&1; then echo reached; else echo refused; fi; }
echo "mysql: $(r getent hosts mysql)"
echo "allowed: $(r curl -sS --fail --max-time 30 https://example.com/)"
echo "blocked: $(r curl -sS --fail --max-time 30 https://example.net/)"
echo "direct: $(r curl -sS --fail --max-time 10 --noproxy '*' https://example.com/)"
echo "host direct: $(r curl -sS --max-time 10 --noproxy '*' http://host.docker.internal/)"
echo "host via proxy: $(r curl -sS --fail --max-time 10 http://host.docker.internal/)"
echo "unshare: $(r unshare -Urm true)"
`

// lockedBuffer is a log written from the stream goroutine and the test.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestIntegrationAllowlist(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	var logs lockedBuffer
	project := "ITA" + strconv.Itoa(os.Getpid())
	var (
		mu      sync.Mutex
		refused []string
	)
	env := &environment.Docker{
		Project: project,
		Log:     slog.New(slog.NewTextHandler(&logs, nil)),
		Allow:   []string{"example.com:443"},
		OnRefused: func(host string) {
			mu.Lock()
			defer mu.Unlock()
			refused = append(refused, host)
		},
	}
	t.Cleanup(func() {
		if err := env.Cleanup(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		t.Log(logs.String())
	})

	out, err := env.Run(ctx, environment.Spec{
		Issue:   project + "-1",
		Dir:     "/tmp",
		Cmd:     []string{"sh", "-c", allowlistScript},
		Timeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Run = %v\n%s", err, out)
	}
	for _, want := range []string{
		"mysql: reached",
		"allowed: reached",
		"blocked: refused",
		"direct: refused",
		"host direct: refused",
		"host via proxy: refused",
		"unshare: refused",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("agent output lacks %q:\n%s", want, out)
		}
	}
	mu.Lock()
	got := slices.Clone(refused)
	mu.Unlock()
	for _, host := range []string{"example.net:443", "host.docker.internal:80"} {
		if !slices.Contains(got, host) {
			t.Errorf("refused hosts = %q, want %s among them", got, host)
		}
		if !strings.Contains(logs.String(), "host="+host) {
			t.Errorf("run log lacks the refused %s", host)
		}
	}
	if slices.Contains(got, "example.com:443") {
		t.Errorf("the allowed host was refused: %q", got)
	}
}
