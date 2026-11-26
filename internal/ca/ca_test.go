package ca

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

// CLR-247: the first Ensure makes a CA whose key only the runner's user may
// read, and whose certificate, alone in its own file, is a CA a client can
// verify a certificate it signed against.
func TestEnsureMakesTheCA(t *testing.T) {
	dir := Dir(t.TempDir())
	c, err := Ensure(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, keyFile): 0o600, c.CertFile: 0o644} {
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != want {
				t.Errorf("%s: mode %v, want %v", filepath.Base(path), got, want)
			}
		}
	}
	if !c.Cert.IsCA || c.Cert.NotAfter.Sub(now) != Validity {
		t.Errorf("certificate: CA %v, valid until %v", c.Cert.IsCA, c.Cert.NotAfter)
	}
	b, err := os.ReadFile(c.CertFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "PRIVATE KEY") {
		t.Fatal("the certificate file holds the key")
	}
	block, rest := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		t.Fatalf("certificate file = %q, want one certificate", b)
	}
	if filepath.Dir(c.CertFile) != dir {
		t.Errorf("certificate file %s is outside %s", c.CertFile, dir)
	}
}

// The same CA comes back until it is near its end; then, or after a
// rotation, a new one with its own certificate file, while the old file
// stays for the containers that mount it.
func TestEnsureKeepsThenRenews(t *testing.T) {
	dir := Dir(t.TempDir())
	first, err := Ensure(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Ensure(dir, now.Add(Validity-Renew-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Cert.Equal(first.Cert) || again.CertFile != first.CertFile {
		t.Error("a CA well within its validity was replaced")
	}
	renewed, err := Ensure(dir, now.Add(Validity-Renew+time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Cert.Equal(first.Cert) || renewed.CertFile == first.CertFile {
		t.Error("a CA near its end was kept")
	}
	rotated, err := Rotate(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Cert.Equal(renewed.Cert) {
		t.Error("Rotate kept the CA")
	}
	loaded, err := Ensure(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Cert.Equal(rotated.Cert) || !loaded.Key.Equal(rotated.Key) {
		t.Error("Ensure did not load the rotated CA")
	}
	for _, f := range []string{first.CertFile, renewed.CertFile, rotated.CertFile} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("certificate file: %v", err)
		}
	}
}

// A CA file that cannot be read, or whose key is not its certificate's, is
// replaced.
func TestEnsureReplacesABrokenCA(t *testing.T) {
	dir := Dir(t.TempDir())
	a, err := Ensure(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Rotate(t.TempDir(), now)
	if err != nil {
		t.Fatal(err)
	}
	mixed, err := x509.MarshalPKCS8PrivateKey(b.Key)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"garbage":   []byte("not a pem"),
		"other key": append(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mixed}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.Cert.Raw})...),
	} {
		if err := os.WriteFile(filepath.Join(dir, keyFile), content, 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Ensure(dir, now)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Cert.Equal(a.Cert) || !c.Key.PublicKey.Equal(c.Cert.PublicKey) {
			t.Errorf("%s: not replaced by a matching pair", name)
		}
	}
}
