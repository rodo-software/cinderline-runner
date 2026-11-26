//go:build integration

package run

// CLR-247: an agent's container trusts the runner's CA, and the public
// roots still. Real Docker, the proxy and the default image. A TLS server
// on this machine, with a certificate the CA signed, is reached through the
// proxy at host.docker.internal, as a local model provider is (decision
// 2026-11-17-local-model-providers); example.com through the public roots.
// Needs outbound HTTPS to example.com, as TestIntegrationAllowlist.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/ca"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

const caScript = `r() { if "$@" >/dev/null 2>&1; then echo reached; else echo refused; fi; }
echo "ca: $(ls "$(dirname "$1")")"
echo "signed by the CA: $(r curl -sS --fail --max-time 30 "$0")"
echo "public: $(r curl -sS --fail --max-time 30 https://example.com/)"
echo "bundle: $(r test -s "$SSL_CERT_FILE" -a "$SSL_CERT_FILE" = "$GIT_SSL_CAINFO" -a "$SSL_CERT_FILE" = "$REQUESTS_CA_BUNDLE" -a "$SSL_CERT_FILE" = "$CURL_CA_BUNDLE")"
echo "node: $(r test "$NODE_EXTRA_CA_CERTS" = "$1")"
`

func TestIntegrationRunnersCA(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	dir := ca.Dir(t.TempDir())
	authority, err := ca.Ensure(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// The container reaches this machine on its bridge address, not on loopback.
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))
	srv.Listener.Close()
	srv.Listener = ln
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf(t, authority, environment.HostGateway)}}
	srv.StartTLS()
	defer srv.Close()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	project := "ITC" + strconv.Itoa(os.Getpid())
	var logs lockedBuffer
	env := &environment.Docker{
		Project: project,
		Exec:    workspace.RunCommand,
		Log:     slog.New(slog.NewTextHandler(&logs, nil)),
		Allow:   []string{"example.com:443"},
		Memory:  4 << 30,
		CA:      dir,
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
		Cmd:     []string{"sh", "-c", caScript, "https://" + environment.HostGateway + ":" + port + "/", environment.CAPath},
		Timeout: 5 * time.Minute,
		Allow:   []string{environment.HostGateway + ":" + port},
	})
	if err != nil {
		t.Fatalf("Run = %v\n%s", err, out)
	}
	for _, want := range []string{
		"ca: ca.crt", // the certificate only, never the key
		"signed by the CA: reached",
		"public: reached",
		"bundle: reached",
		"node: reached",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("agent output lacks %q:\n%s", want, out)
		}
	}
}

// leaf is a server certificate for host, signed by the CA, as the proxy
// signs one for a refused host.
func leaf(t *testing.T, authority *ca.CA, host string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, authority.Cert, &key.PublicKey, authority.Key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
