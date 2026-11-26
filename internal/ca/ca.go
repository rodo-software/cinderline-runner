// Package ca is the runner's own certificate authority (app decision
// 2026-11-25-blocked-host-urls): the egress proxy signs certificates with it
// for the hosts it refuses, to read the URL a refused connection was for,
// and every agent and check container trusts it. One per runner, in its
// state folder; its key never leaves the machine and never enters an
// agent's container.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/name"
)

// Validity is how long a new CA is valid; Renew is how long before its end
// a CA is replaced. A turn's containers live hours at most, so a CA is
// never replaced under one that still needs it.
const (
	Validity = 5 * 365 * 24 * time.Hour
	Renew    = 30 * 24 * time.Hour
)

// keyFile holds the key and its certificate together, so a reader always
// gets a matching pair, also while `rotate-ca` replaces it. Only the
// runner's user may read it.
const keyFile = "ca.key"

// CA is the runner's CA as loaded from its folder.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	// CertFile is the certificate alone, PEM, the one file of the folder a
	// container may mount. It is named by the certificate's hash and never
	// rewritten, so a container started before a rotation keeps its own.
	CertFile string
}

// Dir is the CA's folder in the runner's state folder.
func Dir(state string) string { return filepath.Join(state, "ca") }

// mu serialises this process's changes to the folder; another process (the
// daemon and `rotate-ca`) only ever replaces whole files.
var mu sync.Mutex

// Ensure returns the CA in dir, made first when there is none, the one
// there cannot be read, or it ends within Renew of now. A new CA only
// affects the containers started after it.
func Ensure(dir string, now time.Time) (*CA, error) {
	mu.Lock()
	defer mu.Unlock()
	if c, err := load(dir); err == nil && now.Add(Renew).Before(c.Cert.NotAfter) && !now.Before(c.Cert.NotBefore) {
		return c, writeCert(dir, c)
	}
	// Missing, unreadable or near its end: nothing outside the runner
	// depends on the CA, so a new one replaces it.
	return create(dir, now)
}

// Rotate replaces the CA in dir with a new one, whatever its state.
func Rotate(dir string, now time.Time) (*CA, error) {
	mu.Lock()
	defer mu.Unlock()
	return create(dir, now)
}

// create makes a new CA: an ECDSA P-256 key and a self-signed certificate
// that may sign the proxy's certificates, and through one intermediate,
// which leaves the proxy ticket (CLR-248) the choice of keeping this key out
// of the proxy's container too.
func create(dir string, now time.Time) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name.Name + " runner CA", Organization: []string{name.Name + " runner"}},
		NotBefore:             now.Add(-time.Hour), // a container's clock a little behind
		NotAfter:              now.Add(Validity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            1,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	pair := append(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), certPEM(cert)...)
	if err := writeFile(dir, keyFile, pair, 0o600); err != nil {
		return nil, fmt.Errorf("write the CA key: %w", err)
	}
	c := &CA{Cert: cert, Key: key}
	return c, writeCert(dir, c)
}

// load reads the pair in dir's keyFile and checks the key is the
// certificate's.
func load(dir string) (*CA, error) {
	b, err := os.ReadFile(filepath.Join(dir, keyFile))
	if err != nil {
		return nil, err
	}
	var c CA
	for block, rest := pem.Decode(b); block != nil; block, rest = pem.Decode(rest) {
		switch block.Type {
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, err
			}
			ek, ok := k.(*ecdsa.PrivateKey)
			if !ok {
				return nil, errors.New("the CA key is not ECDSA")
			}
			c.Key = ek
		case "CERTIFICATE":
			if c.Cert, err = x509.ParseCertificate(block.Bytes); err != nil {
				return nil, err
			}
		}
	}
	if c.Key == nil || c.Cert == nil || !c.Cert.IsCA || !c.Key.PublicKey.Equal(c.Cert.PublicKey) {
		return nil, errors.New("the CA file holds no matching key and CA certificate")
	}
	return &c, nil
}

// writeCert sets c.CertFile and writes it when it is not there yet.
func writeCert(dir string, c *CA) error {
	sum := sha256.Sum256(c.Cert.Raw)
	base := "ca-" + hex.EncodeToString(sum[:6]) + ".crt"
	c.CertFile = filepath.Join(dir, base)
	if _, err := os.Stat(c.CertFile); err == nil {
		return nil
	}
	// 0644: the container's user is not the runner's, and a certificate is public.
	if err := writeFile(dir, base, certPEM(c.Cert), 0o644); err != nil {
		return fmt.Errorf("write the CA certificate: %w", err)
	}
	return nil
}

func certPEM(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}

// writeFile replaces dir/base with b in one rename, so no reader sees half
// of it.
func writeFile(dir, base string, b []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(dir, base+".tmp-*") // made 0600
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	if err = errors.Join(err, f.Chmod(perm), f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, base))
}
