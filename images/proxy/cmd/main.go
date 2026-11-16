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
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"strings"
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
// lower case, no wildcards.
func parseAllow(entries []string) (map[string]bool, error) {
	set := map[string]bool{}
	for _, e := range entries {
		host, port, ok := hostPort(e)
		if !ok || strings.ContainsAny(host, "*/@ ") {
			return nil, fmt.Errorf("allowlist entry %q is not host:port", e)
		}
		set[host+":"+port] = true
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

type proxy struct {
	allow   map[string]bool
	log     *slog.Logger
	forward *httputil.ReverseProxy
}

func newProxy(allow map[string]bool, out io.Writer) http.Handler {
	return &proxy{
		allow: allow,
		log:   slog.New(slog.NewJSONHandler(out, nil)),
		// A no-op Rewrite keeps the request's absolute URL as the target.
		// The zero Transport ignores proxy variables.
		forward: &httputil.ReverseProxy{Rewrite: func(*httputil.ProxyRequest) {}, Transport: &http.Transport{}},
	}
}

// allowed checks target against the list and logs a refusal.
func (p *proxy) allowed(target string) bool {
	host, port, ok := hostPort(target)
	if ok && p.allow[host+":"+port] {
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
	up, err := net.DialTimeout("tcp", r.Host, 30*time.Second)
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
