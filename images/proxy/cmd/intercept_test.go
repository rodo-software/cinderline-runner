package main

// Acceptance tests for CLR-248: the proxy reads the first request of a
// connection it refuses anyway, over HTTPS with a certificate from the
// turn's intermediate of the runner's CA, and logs its method and URL
// without query, with the reason for the refusal; it never touches an
// allowed host's traffic or the runner's own machine.

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/ca"
)

// interceptingProxy runs the proxy with allow and a turn's intermediate of a
// new runner CA, and returns its address, its log and a pool that trusts
// the CA, as an agent's container does.
func interceptingProxy(t *testing.T, allow ...string) (string, *syncBuffer, *x509.CertPool) {
	t.Helper()
	c, err := ca.Ensure(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := ca.Intermediate(c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	iss, err := newIssuer(string(bundle))
	if err != nil {
		t.Fatal(err)
	}
	set, err := parseAllow(allow)
	if err != nil {
		t.Fatal(err)
	}
	log := &syncBuffer{}
	srv := httptest.NewServer(newProxy(set, iss, "cinderline", log))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)
	return srv.Listener.Addr().String(), log, pool
}

// httpsVia is a client that goes through proxy and trusts roots; it would
// speak HTTP/2 where the server offers it.
func httpsVia(proxy string, roots *x509.CertPool) *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:             http.ProxyURL(&url.URL{Scheme: "http", Host: proxy}),
		TLSClientConfig:   &tls.Config{RootCAs: roots},
		ForceAttemptHTTP2: true,
	}}
}

// waitRefusals waits until the log holds n refusals; an intercepted
// connection logs when it ends.
func waitRefusals(t *testing.T, log *syncBuffer, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := refusals(t, log.String())
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// want checks one refusal's fields; "" for method and url means left out.
func wantRefusal(t *testing.T, got map[string]any, host, port, reason, method, url string) {
	t.Helper()
	if got["host"] != host || got["port"] != port || got["reason"] != reason {
		t.Errorf("refusal = %v, want %s:%s for %s", got, host, port, reason)
	}
	if method == "" {
		if _, ok := got["method"]; ok {
			t.Errorf("refusal = %v, want no method", got)
		}
		if _, ok := got["url"]; ok {
			t.Errorf("refusal = %v, want no url", got)
		}
		return
	}
	if got["method"] != method || got["url"] != url {
		t.Errorf("refusal = %v, want %s %s", got, method, url)
	}
}

// A refused HTTPS host is answered 403 over TLS that the CA's chain
// verifies, over HTTP/1.1 also for a client that would speak HTTP/2, and
// logged with the first request's method and URL, without its query; the
// request's headers and body are never logged.
func TestRefusedHTTPSHostYieldsItsURL(t *testing.T) {
	proxy, log, roots := interceptingProxy(t, "allowed.example:443")
	c := httpsVia(proxy, roots)
	req, _ := http.NewRequest(http.MethodPost, "https://Evil.Example/repo/pkg.tar.gz?token=query-marker#frag", strings.NewReader("body-marker"))
	req.Header.Set("Authorization", "Bearer header-marker")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || resp.ProtoMajor != 1 || !strings.Contains(string(body), "cinderline's egress proxy") {
		t.Errorf("= %s %d %q, want HTTP/1.1 403 naming the egress block", resp.Proto, resp.StatusCode, body)
	}
	for _, u := range []string{"https://evil.example:8443/a/b", "https://93.184.216.34/ip"} {
		resp, err := c.Get(u)
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		resp.Body.Close()
	}
	got := waitRefusals(t, log, 3)
	if len(got) != 3 {
		t.Fatalf("refusals = %v, want 3", got)
	}
	wantRefusal(t, got[0], "evil.example", "443", "not_allowed", "POST", "https://evil.example/repo/pkg.tar.gz")
	wantRefusal(t, got[1], "evil.example", "8443", "not_allowed", "GET", "https://evil.example:8443/a/b")
	wantRefusal(t, got[2], "93.184.216.34", "443", "not_allowed", "GET", "https://93.184.216.34/ip")
	for _, m := range []string{"query-marker", "header-marker", "body-marker", "frag"} {
		if strings.Contains(log.String(), m) {
			t.Errorf("the log holds %s:\n%s", m, log)
		}
	}
}

// An allowed HTTPS host is tunnelled as before: the client sees the host's
// own certificate, and nothing is logged.
func TestAllowedHTTPSHostIsNeverIntercepted(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "real")
	}))
	t.Cleanup(backend.Close)
	proxy, log, roots := interceptingProxy(t, backend.Listener.Addr().String())
	roots.AddCert(backend.Certificate())
	resp, err := httpsVia(proxy, roots).Get(backend.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "real" || !resp.TLS.PeerCertificates[0].Equal(backend.Certificate()) {
		t.Errorf("= %q from %v, want the backend's own certificate", body, resp.TLS.PeerCertificates[0].Subject)
	}
	if got := refusals(t, log.String()); len(got) != 0 {
		t.Errorf("refusals = %v", got)
	}
}

// A client that pins certificates, or ignores the CA, fails the handshake:
// the refusal is logged by host and reason alone.
func TestPinnedClientYieldsHostAndReasonOnly(t *testing.T) {
	proxy, log, _ := interceptingProxy(t)
	if _, err := httpsVia(proxy, x509.NewCertPool()).Get("https://pinned.example/secret/path"); err == nil {
		t.Fatal("a client without the CA accepted the proxy's certificate")
	}
	got := waitRefusals(t, log, 1)
	if len(got) != 1 {
		t.Fatalf("refusals = %v", got)
	}
	wantRefusal(t, got[0], "pinned.example", "443", "not_allowed", "", "")
	if strings.Contains(log.String(), "secret") {
		t.Errorf("the log holds the path:\n%s", log)
	}
}

// A client that speaks only HTTP/2 finds no protocol in common: its
// handshake fails, and the refusal has no URL.
func TestHTTP2OnlyClientYieldsHostAndReasonOnly(t *testing.T) {
	proxy, log, roots := interceptingProxy(t)
	_, c, _ := connect(t, proxy, "h2.example:443")
	tc := tls.Client(c, &tls.Config{RootCAs: roots, ServerName: "h2.example", NextProtos: []string{"h2"}})
	tc.SetDeadline(time.Now().Add(3 * time.Second))
	if err := tc.Handshake(); err == nil {
		t.Fatalf("handshake agreed on %q", tc.ConnectionState().NegotiatedProtocol)
	}
	got := waitRefusals(t, log, 1)
	if len(got) != 1 {
		t.Fatalf("refusals = %v", got)
	}
	wantRefusal(t, got[0], "h2.example", "443", "not_allowed", "", "")
}

// A refused plain-HTTP request is answered 403 and logged with its URL,
// without user info, query or fragment, as the request line has it.
func TestRefusedPlainHTTPYieldsItsURL(t *testing.T) {
	proxy, log, _ := interceptingProxy(t)
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "PUT http://user:pw-marker@Evil.Example:8080/a/b?q=query-marker#frag HTTP/1.1\r\nHost: evil.example:8080\r\nContent-Length: 0\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("= %d, want 403", resp.StatusCode)
	}
	got := refusals(t, log.String())
	if len(got) != 1 {
		t.Fatalf("refusals = %v", got)
	}
	wantRefusal(t, got[0], "evil.example", "8080", "not_allowed", "PUT", "http://evil.example:8080/a/b")
	for _, m := range []string{"pw-marker", "query-marker"} {
		if strings.Contains(log.String(), m) {
			t.Errorf("the log holds %s:\n%s", m, log)
		}
	}
}

// The runner's own machine is never intercepted: a CONNECT to it is
// refused with 403 and logged as runner_machine, without a URL; a plain
// request with its URL.
func TestRunnersMachineIsNeverIntercepted(t *testing.T) {
	proxy, log, _ := interceptingProxy(t)
	targets := []string{"localhost:443", "127.0.0.2:8443", "[::1]:443", "host.docker.internal:11434"}
	for _, target := range targets {
		if status, _, _ := connect(t, proxy, target); status != http.StatusForbidden {
			t.Errorf("CONNECT %s = %d, want 403", target, status)
		}
	}
	resp, err := viaProxy(proxy).Get("http://localhost:9/admin?x=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := refusals(t, log.String())
	if len(got) != 5 {
		t.Fatalf("refusals = %v", got)
	}
	wantRefusal(t, got[0], "localhost", "443", "runner_machine", "", "")
	wantRefusal(t, got[1], "127.0.0.2", "8443", "runner_machine", "", "")
	wantRefusal(t, got[2], "::1", "443", "runner_machine", "", "")
	wantRefusal(t, got[3], "host.docker.internal", "11434", "runner_machine", "", "")
	wantRefusal(t, got[4], "localhost", "9", "runner_machine", "GET", "http://localhost:9/admin")
}

// A host allowed from Cinderline that resolves to a private address is
// refused as private_address, and its first request is read like any
// other refused host's.
func TestPrivateAddressIsReportedWithItsURL(t *testing.T) {
	resolver := privateResolver(t)
	saved := dialPublic
	dialPublic = func(ctx context.Context, network, target string, control func(string, string, syscall.RawConn) error) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second, Resolver: resolver, Control: control}).DialContext(ctx, network, target)
	}
	t.Cleanup(func() { dialPublic = saved })
	proxy, log, roots := interceptingProxy(t, "public:internal.example:443", "public:internal.example:80")
	resp, err := httpsVia(proxy, roots).Get("https://internal.example/metadata?k=v")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("HTTPS = %d, want 403", resp.StatusCode)
	}
	resp, err = viaProxy(proxy).Get("http://internal.example/plain?k=v")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("HTTP = %d, want 403", resp.StatusCode)
	}
	got := waitRefusals(t, log, 2)
	if len(got) != 2 {
		t.Fatalf("refusals = %v", got)
	}
	wantRefusal(t, got[0], "internal.example", "443", "private_address", "GET", "https://internal.example/metadata")
	wantRefusal(t, got[1], "internal.example", "80", "private_address", "GET", "http://internal.example/plain")
}

// A client that stalls, before or after its handshake, or sends a head
// past the cap, is closed within the time limit and logged by host alone.
func TestSlowOrOversizedClientIsCutOff(t *testing.T) {
	saved := interceptTimeout
	interceptTimeout = 300 * time.Millisecond
	t.Cleanup(func() { interceptTimeout = saved })
	proxy, log, roots := interceptingProxy(t)

	// Silent after the CONNECT.
	status, c, r := connect(t, proxy, "silent.example:443")
	if status != http.StatusOK {
		t.Fatalf("CONNECT = %d, want 200", status)
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := r.ReadByte(); err == nil || isTimeout(err) {
		t.Errorf("silent client: read %v, want the proxy to close first", err)
	}

	// Half a request head after the handshake, then nothing; then a head
	// past the cap.
	for _, tt := range []struct{ host, send string }{
		{"slow.example", "GET /slow HTTP/1.1\r\nHost: slow.example\r\n"},
		{"big.example", "GET /big HTTP/1.1\r\nHost: big.example\r\nX-Big: " + strings.Repeat("a", maxHead) + "\r\n\r\n"},
	} {
		status, c, _ := connect(t, proxy, tt.host+":443")
		if status != http.StatusOK {
			t.Fatalf("CONNECT = %d", status)
		}
		tc := tls.Client(c, &tls.Config{RootCAs: roots, ServerName: tt.host})
		tc.SetDeadline(time.Now().Add(3 * time.Second))
		if err := tc.Handshake(); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tc, tt.send)
		if _, err := io.ReadAll(tc); isTimeout(err) {
			t.Errorf("%s: the proxy did not close the connection", tt.host)
		}
	}
	got := waitRefusals(t, log, 3)
	if len(got) != 3 {
		t.Fatalf("refusals = %v", got)
	}
	wantRefusal(t, got[0], "silent.example", "443", "not_allowed", "", "")
	wantRefusal(t, got[1], "slow.example", "443", "not_allowed", "", "")
	wantRefusal(t, got[2], "big.example", "443", "not_allowed", "", "")
}

func isTimeout(err error) bool {
	var ne net.Error
	return err != nil && errors.As(err, &ne) && ne.Timeout()
}

// Without the runner's CA the proxy refuses a CONNECT with 403 as before,
// and logs the host and reason.
func TestWithoutCAConnectIsRefused(t *testing.T) {
	proxy, log := startProxy(t)
	if status, _, _ := connect(t, proxy, "evil.example:443"); status != http.StatusForbidden {
		t.Errorf("CONNECT = %d, want 403", status)
	}
	got := refusals(t, log.String())
	if len(got) != 1 {
		t.Fatalf("refusals = %v", got)
	}
	wantRefusal(t, got[0], "evil.example", "443", "not_allowed", "", "")
}

// A host's certificate is made once and kept; it names an address as an
// IP address.
func TestCertificatesAreCachedPerHost(t *testing.T) {
	c, err := ca.Ensure(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := ca.Intermediate(c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	iss, err := newIssuer(string(bundle))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := iss.certificate("a.example")
	b, _ := iss.certificate("a.example")
	ip, err := iss.certificate("10.0.0.1")
	if err != nil || a != b {
		t.Fatalf("certificate cached: %v, %v", a == b, err)
	}
	leaf, _ := x509.ParseCertificate(ip.Certificate[0])
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "10.0.0.1" || len(leaf.DNSNames) != 0 {
		t.Errorf("leaf for an address: %v %v", leaf.IPAddresses, leaf.DNSNames)
	}
	if _, err := newIssuer("not pem"); err == nil {
		t.Error("newIssuer accepted garbage")
	}
}

func TestRequestURL(t *testing.T) {
	long := "/" + strings.Repeat("a", 3000)
	for _, tt := range []struct{ scheme, host, port, raw, want string }{
		{"https", "evil.example", "443", "/p?q=1#f", "https://evil.example/p"},
		{"http", "evil.example", "80", "http://u:p@evil.example/p?q", "http://evil.example/p"},
		{"http", "evil.example", "8080", "/", "http://evil.example:8080/"},
		{"https", "2001:db8::1", "443", "*", "https://[2001:db8::1]/*"},
		{"https", "2001:db8::1", "8443", "/x", "https://[2001:db8::1]:8443/x"},
		{"https", "e.example", "443", "/caf%C3%A9 x", "https://e.example/caf%C3%A9%20x"},
		{"https", "e.example", "443", long, ("https://e.example" + long)[:maxURL]},
		{"https", "e.example", "443", "/" + strings.Repeat("b", maxURL-len("https://e.example/")-1) + "%2Fzz", ("https://e.example/" + strings.Repeat("b", maxURL-len("https://e.example/")-1))},
	} {
		u, err := url.Parse(tt.raw)
		if err != nil {
			u = &url.URL{Path: tt.raw}
		}
		if got := requestURL(tt.scheme, tt.host, tt.port, u); got != tt.want || len(got) > maxURL {
			t.Errorf("requestURL(%s, %q) = %q, want %q", tt.host, tt.raw, got, tt.want)
		}
	}
}
