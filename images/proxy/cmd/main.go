// Command proxy is the egress proxy of an agent run (CSR-63). It sits on the
// run's internal network and on an outside one, and lets the agent reach
// exactly the host:port pairs given as its arguments: CONNECT tunnels for
// HTTPS, forwarded requests for plain HTTP. It resolves names itself. Every
// refusal is one JSON line on standard output with the host, the port and
// the time; request content is never logged.
//
// It is built from this one file with the standard library only
// (images/proxy/Dockerfile), so it holds everything it needs.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	allow, err := parseAllow(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "proxy:", err)
		os.Exit(2)
	}
	slog.New(slog.NewJSONHandler(os.Stdout, nil)).Info("listening", "addr", ":3128", "allowed", len(allow))
	srv := &http.Server{Addr: ":3128", Handler: newProxy(allow, os.Stdout), ReadHeaderTimeout: 30 * time.Second}
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
	ip := net.ParseIP(host)
	if ip == nil || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return errPrivateAddress
	}
	return nil
}

var dialPublic = func(ctx context.Context, network, target string, control func(string, string, syscall.RawConn) error) (net.Conn, error) {
	return (&net.Dialer{Timeout: 30 * time.Second, Control: control}).DialContext(ctx, network, target)
}

type proxy struct {
	allow   map[string]bool
	log     *slog.Logger
	forward *httputil.ReverseProxy
}

func newProxy(allow map[string]bool, out io.Writer) http.Handler {
	p := &proxy{
		allow: allow,
		log:   slog.New(slog.NewJSONHandler(out, nil)),
	}
	// A no-op Rewrite keeps the request's absolute URL as the target.
	// A Transport without Proxy ignores proxy variables.
	p.forward = &httputil.ReverseProxy{
		Rewrite:      func(*httputil.ProxyRequest) {},
		Transport:    &http.Transport{DialContext: p.dial},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) { upstreamError(w, err) },
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
	var refused sync.Once
	return dialPublic(ctx, network, target, func(network, address string, c syscall.RawConn) error {
		err := checkPublicAddress(network, address, c)
		if err != nil {
			// Dial may return another address's error; record the refusal here.
			refused.Do(func() { p.log.Warn("refused", "host", host, "port", port) })
		}
		return err
	})
}

func upstreamError(w http.ResponseWriter, err error) {
	if errors.Is(err, errPrivateAddress) {
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}
	http.Error(w, "upstream unreachable", http.StatusBadGateway)
}

// allowed checks target against the list and logs a refusal.
func (p *proxy) allowed(target string) bool {
	host, port, ok := hostPort(target)
	if _, listed := p.allow[host+":"+port]; ok && listed {
		return true
	}
	if !ok {
		host, port = target, ""
	}
	p.log.Warn("refused", "host", host, "port", port)
	return false
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
	target := r.URL.Host
	if r.URL.Port() == "" {
		target = net.JoinHostPort(r.URL.Hostname(), "80")
	}
	if !p.allowed(target) {
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}
	p.forward.ServeHTTP(w, r)
}

func (p *proxy) tunnel(w http.ResponseWriter, r *http.Request) {
	if !p.allowed(r.Host) {
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}
	up, err := p.dial(r.Context(), "tcp", r.Host)
	if err != nil {
		upstreamError(w, err)
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
