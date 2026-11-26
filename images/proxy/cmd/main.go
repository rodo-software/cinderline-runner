// Command proxy is the egress proxy of an agent run (CSR-63). It sits on the
// run's internal network and on an outside one, and lets the agent reach
// exactly the host:port pairs given as its arguments: CONNECT tunnels for
// HTTPS, never decrypted, forwarded requests for plain HTTP. It resolves
// names itself. Every refusal is one JSON line on standard output with the
// host, the port, the reason, the time and, when the proxy saw it, the first
// request's method and URL without its query (app decision
// 2026-11-25-blocked-host-urls); headers and bodies are never logged.
//
// To see that URL over HTTPS, a refused CONNECT is answered 200 and its TLS
// is ended with a certificate for the host, signed by the turn's
// intermediate of the runner's CA in $PROXY_CA; without one it is refused
// with 403 as before. The runner's own machine is never intercepted.
//
// It is built from this one file with the standard library only
// (images/proxy/Dockerfile), so it holds everything it needs.
package main

import (
	"bufio"
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	allow, err := parseAllow(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "proxy:", err)
		os.Exit(2)
	}
	iss, err := newIssuer(os.Getenv("PROXY_CA"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "proxy:", err)
		os.Exit(2)
	}
	os.Unsetenv("PROXY_CA")
	slog.New(slog.NewJSONHandler(os.Stdout, nil)).Info("listening", "addr", ":3128", "allowed", len(allow), "intercepts", iss != nil)
	srv := &http.Server{Addr: ":3128", Handler: newProxy(allow, iss, os.Getenv("PROXY_NAME"), os.Stdout), ReadHeaderTimeout: 30 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "proxy:", err)
		os.Exit(1)
	}
}

// parseAllow reads the allowlist: exact host:port entries, the host in
// lower case, no wildcards. A public: prefix marks an app approval: false
// means public addresses only, true means a locally trusted entry.
func parseAllow(entries []string) (map[string]bool, error) {
	set := map[string]bool{}
	for _, e := range entries {
		e, public := strings.CutPrefix(e, "public:")
		host, port, ok := hostPort(e)
		if !ok || strings.ContainsAny(host, "*/@ ") {
			return nil, fmt.Errorf("allowlist entry %q is not host:port", e)
		}
		// Local permission wins regardless of order (decision 2026-11-19-app-host-addresses).
		set[host+":"+port] = set[host+":"+port] || !public
	}
	return set, nil
}

// hostPort splits hp into a lower-case host and a port in 1-65535.
func hostPort(hp string) (host, port string, ok bool) {
	host, port, err := net.SplitHostPort(hp)
	if err != nil || host == "" {
		return "", "", false
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return "", "", false
	}
	return strings.ToLower(host), port, true
}

// dial connects to an allowed target; the tests map host.docker.internal,
// which the runner maps for a local model provider with --add-host.
var dial = (&net.Dialer{Timeout: 30 * time.Second}).DialContext

var errPrivateAddress = errors.New("private network address not allowed")

// Control sees the actual resolved IP before connect, including each
// fallback address, so a DNS change cannot bypass the check.
func checkPublicAddress(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errPrivateAddress
	}
	// A resolved IPv6 link-local address may include an interface zone.
	host, _, _ = strings.Cut(host, "%")
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return errPrivateAddress
	}
	// Unmap so ::ffff:100.64.0.1 meets the IPv4 prefixes below.
	ip = ip.Unmap()
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() ||
		slices.ContainsFunc(specialPrefixes, func(p netip.Prefix) bool { return p.Contains(ip) }) {
		return errPrivateAddress
	}
	return nil
}

// specialPrefixes are non-public ranges the netip methods above miss (CLR-225).
var specialPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),     // "this network"
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT, which Tailscale uses
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64, which embeds any IPv4 address
}

var dialPublic = func(ctx context.Context, network, target string, control func(string, string, syscall.RawConn) error) (net.Conn, error) {
	return (&net.Dialer{Timeout: 30 * time.Second, Control: control}).DialContext(ctx, network, target)
}

type proxy struct {
	allow   map[string]bool
	issuer  *issuer // nil: refused CONNECTs are not intercepted
	block   string  // the body of a 403
	log     *slog.Logger
	forward *httputil.ReverseProxy
}

// newProxy serves allow; iss, if set, signs the certificates of refused
// HTTPS hosts; product names the egress block in a 403's body.
func newProxy(allow map[string]bool, iss *issuer, product string, out io.Writer) http.Handler {
	p := &proxy{
		allow:  allow,
		issuer: iss,
		block:  "Blocked by " + cmp.Or(product, "the runner") + "'s egress proxy: this host is not allowed for this run.",
		log:    slog.New(slog.NewJSONHandler(out, nil)),
	}
	// A no-op Rewrite keeps the request's absolute URL as the target.
	// A Transport without Proxy ignores proxy variables.
	p.forward = &httputil.ReverseProxy{
		Rewrite:      func(*httputil.ProxyRequest) {},
		Transport:    &http.Transport{DialContext: p.dial},
		ErrorHandler: p.forwardError,
	}
	return p
}

func (p *proxy) dial(ctx context.Context, network, target string) (net.Conn, error) {
	host, port, ok := hostPort(target)
	trusted, listed := p.allow[host+":"+port]
	if !ok || !listed {
		return nil, errors.New("host not allowed")
	}
	if trusted {
		return dial(ctx, network, target)
	}
	var private atomic.Bool
	c, err := dialPublic(ctx, network, target, func(network, address string, c syscall.RawConn) error {
		err := checkPublicAddress(network, address, c)
		if err != nil {
			private.Store(true)
		}
		return err
	})
	if err != nil && private.Load() {
		// Dial may return another address's error; the refusal is what counts.
		return nil, errPrivateAddress
	}
	return c, err
}

// The reasons a host is refused, as the runner reports them (app decision
// 2026-11-25-blocked-host-urls). The runner adds strict, which only it knows.
const (
	notAllowed     = "not_allowed"     // on no allowlist
	privateAddress = "private_address" // allowed from Cinderline, but resolves to a private address
	runnerMachine  = "runner_machine"  // the runner's own machine, never intercepted
)

// runnersMachine says host is localhost, a loopback address or
// host.docker.internal: the runner's own machine, as the runner's config
// names it.
func runnersMachine(host string) bool {
	ip, err := netip.ParseAddr(host)
	return host == "localhost" || host == "host.docker.internal" || err == nil && ip.Unmap().IsLoopback()
}

// refuse logs one refusal; method and url only when the proxy read the
// request.
func (p *proxy) refuse(host, port, reason, method, url string) {
	args := []any{"host", host, "port", port, "reason", reason}
	if url != "" {
		args = append(args, "method", method, "url", url)
	}
	p.log.Warn("refused", args...)
}

// httpTarget is the host:port of an absolute http:// URL, port 80 when it
// has none.
func httpTarget(u *url.URL) string {
	if u.Port() == "" {
		return net.JoinHostPort(u.Hostname(), "80")
	}
	return u.Host
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.tunnel(w, r)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "only CONNECT and absolute http:// requests", http.StatusBadRequest)
		return
	}
	target := httpTarget(r.URL)
	host, port, ok := hostPort(target)
	if !ok {
		p.refuse(target, "", notAllowed, "", "")
		http.Error(w, p.block, http.StatusForbidden)
		return
	}
	if _, listed := p.allow[host+":"+port]; !listed {
		reason := notAllowed
		if runnersMachine(host) {
			reason = runnerMachine
		}
		p.refuse(host, port, reason, r.Method, requestURL("http", host, port, r.URL))
		http.Error(w, p.block, http.StatusForbidden)
		return
	}
	p.forward.ServeHTTP(w, r)
}

// forwardError answers a forwarded request whose upstream failed.
func (p *proxy) forwardError(w http.ResponseWriter, r *http.Request, err error) {
	if !errors.Is(err, errPrivateAddress) {
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	host, port, _ := hostPort(httpTarget(r.URL))
	p.refuse(host, port, privateAddress, r.Method, requestURL("http", host, port, r.URL))
	http.Error(w, p.block, http.StatusForbidden)
}

func (p *proxy) tunnel(w http.ResponseWriter, r *http.Request) {
	host, port, ok := hostPort(r.Host)
	if !ok {
		p.refuse(r.Host, "", notAllowed, "", "")
		http.Error(w, p.block, http.StatusForbidden)
		return
	}
	if _, listed := p.allow[host+":"+port]; !listed {
		if runnersMachine(host) {
			p.refuse(host, port, runnerMachine, "", "")
			http.Error(w, p.block, http.StatusForbidden)
			return
		}
		p.intercept(w, host, port, notAllowed)
		return
	}
	up, err := p.dial(r.Context(), "tcp", r.Host)
	if errors.Is(err, errPrivateAddress) {
		p.intercept(w, host, port, privateAddress)
		return
	}
	if err != nil {
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer up.Close()
	c, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer c.Close()
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, buf); done <- struct{}{} }() // buf holds what the client sent early
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done // either side closing ends the tunnel; the defers close both
}

// Bounds on an intercepted connection: the TLS handshake and the first
// request's head must arrive within interceptTimeout and maxHead bytes; its
// body is never read.
var interceptTimeout = 5 * time.Second

const (
	maxHead = 64 << 10
	maxURL  = 2000 // the contract's limit on a refused request's URL
)

// intercept answers a refused CONNECT with 200, ends its TLS with a
// certificate for the host, reads the first request, answers it with 403
// and closes, then logs the refusal: with the request's method and URL when
// it came, by host and reason alone when the handshake or the request
// failed, as for a client that pins its certificates or ignores the CA.
// Without an issuer the CONNECT is refused with 403.
func (p *proxy) intercept(w http.ResponseWriter, host, port, reason string) {
	if p.issuer == nil {
		p.refuse(host, port, reason, "", "")
		http.Error(w, p.block, http.StatusForbidden)
		return
	}
	c, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		p.refuse(host, port, reason, "", "")
		http.Error(w, p.block, http.StatusForbidden)
		return
	}
	defer c.Close()
	var method, u string
	defer func() { p.refuse(host, port, reason, method, u) }()
	if c.SetDeadline(time.Now().Add(interceptTimeout)) != nil {
		return
	}
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	tc := tls.Server(bufferedConn{c, buf.Reader}, &tls.Config{
		// HTTP/2 clients fall back to HTTP/1.1, which is all this reads.
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return p.issuer.certificate(strings.ToLower(cmp.Or(hello.ServerName, host)))
		},
	})
	if tc.Handshake() != nil {
		return
	}
	r, err := http.ReadRequest(bufio.NewReader(io.LimitReader(tc, maxHead)))
	if err != nil {
		return
	}
	method, u = r.Method, requestURL("https", host, port, r.URL)
	body := p.block + "\n"
	_, _ = io.WriteString(tc, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: "+
		strconv.Itoa(len(body))+"\r\nConnection: close\r\n\r\n"+body)
	_ = tc.Close()
}

// bufferedConn reads what the server buffered after the CONNECT first.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// requestURL is a refused request's URL as the runner reports it:
// scheme://host[:port]/path with the refused host and port, never the
// request's own Host, and without query, fragment or user info, cut to
// maxURL bytes. The escaped path is ASCII, so bytes are characters.
func requestURL(scheme, host, port string, u *url.URL) string {
	h := host
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	if !(scheme == "http" && port == "80" || scheme == "https" && port == "443") {
		h += ":" + port
	}
	path := u.EscapedPath()
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	s := scheme + "://" + h + path
	if len(s) > maxURL {
		s = s[:maxURL]
		// Not half an escape.
		if i := strings.LastIndexByte(s[len(s)-2:], '%'); i >= 0 {
			s = s[:len(s)-2+i]
		}
	}
	return s
}

// maxCerts bounds the certificate cache: an agent that tries endless hosts
// gets new certificates, not the proxy's memory.
const maxCerts = 1000

// issuer signs the certificates of refused hosts with the turn's
// intermediate of the runner's CA; the CA's own key never enters the
// proxy. One leaf key serves every host.
type issuer struct {
	ca      *x509.Certificate // the intermediate
	caKey   *ecdsa.PrivateKey
	leafKey *ecdsa.PrivateKey

	mu    sync.Mutex
	certs map[string]*tls.Certificate // by host, for the life of the process
}

// newIssuer reads the intermediate's certificate and PKCS #8 key from
// bundle, PEM; "" is none.
func newIssuer(bundle string) (*issuer, error) {
	if bundle == "" {
		return nil, nil
	}
	var s issuer
	rest := []byte(bundle)
	for block, r := pem.Decode(rest); block != nil; block, r = pem.Decode(r) {
		switch block.Type {
		case "CERTIFICATE":
			c, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("PROXY_CA: %w", err)
			}
			s.ca = c
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("PROXY_CA: %w", err)
			}
			s.caKey, _ = k.(*ecdsa.PrivateKey)
		}
	}
	if s.ca == nil || s.caKey == nil || !s.ca.IsCA || !s.caKey.PublicKey.Equal(s.ca.PublicKey) {
		return nil, errors.New("PROXY_CA holds no matching ECDSA key and CA certificate")
	}
	leaf, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	s.leafKey, s.certs = leaf, map[string]*tls.Certificate{}
	return &s, nil
}

// certificate returns host's certificate, made on first use: valid for the
// host's name or address until the intermediate ends.
func (s *issuer) certificate(host string) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.certs[host]; c != nil {
		return c, nil
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		NotBefore:    time.Now().Add(-time.Hour), // a container's clock a little behind
		NotAfter:     s.ca.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		tmpl.IPAddresses = []net.IP{ip.AsSlice()}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, s.ca, &s.leafKey.PublicKey, s.caKey)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der, s.ca.Raw}, PrivateKey: s.leafKey}
	if len(s.certs) < maxCerts {
		s.certs[host] = c
	}
	return c, nil
}
