// Package loopback keeps a package's tests off the network (CLR-202): a
// test's HTTP request to any host but this machine is refused, and fails
// the package's run even when the code under test only logs the error.
package loopback

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

var (
	mu     sync.Mutex
	stray  []string // "METHOD URL" of every refused request
	direct = http.DefaultTransport
	off    atomic.Bool // set by Off; a flag, not a swap of DefaultTransport, so goroutines left by earlier tests do not race
)

type guard struct{}

func (guard) RoundTrip(r *http.Request) (*http.Response, error) {
	if host := r.URL.Hostname(); !off.Load() && host != "localhost" && !isLoopbackIP(host) {
		mu.Lock()
		stray = append(stray, r.Method+" "+r.URL.Redacted())
		mu.Unlock()
		return nil, fmt.Errorf("loopback: a test sent %s %s to %s, which is not this machine; serve it with httptest", r.Method, r.URL.Redacted(), r.URL.Host)
	}
	return direct.RoundTrip(r)
}

func isLoopbackIP(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Run is a package's TestMain body: os.Exit(loopback.Run(m)). Every request
// through http.DefaultTransport (http.DefaultClient and every http.Client
// without a Transport) must go to loopback; any other fails the run.
func Run(m *testing.M) int {
	http.DefaultTransport = guard{}
	code := m.Run()
	mu.Lock()
	defer mu.Unlock()
	if len(stray) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: tests sent requests off this machine, refused:\n")
		for _, s := range slices.Compact(slices.Sorted(slices.Values(stray))) {
			fmt.Fprintf(os.Stderr, "\t%s\n", s)
		}
		code = 1
	}
	return code
}

// Off lets t reach the network, for a test that needs it by design.
func Off(t testing.TB) {
	off.Store(true)
	t.Cleanup(func() { off.Store(false) })
}
