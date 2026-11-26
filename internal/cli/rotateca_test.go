package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/ca"
)

// CLR-247: rotate-ca replaces the runner's CA, and refuses arguments.
func TestRotateCA(t *testing.T) {
	c, out, _ := newCLI(t, "")
	dir := ca.Dir(c.Paths.State)
	old, err := ca.Ensure(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if code := c.Run([]string{"rotate-ca", "now"}); code != 2 {
		t.Errorf("rotate-ca now: code %d, want 2", code)
	}
	if code := c.Run([]string{"rotate-ca"}); code != 0 {
		t.Fatalf("rotate-ca: code %d", code)
	}
	now, err := ca.Ensure(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if now.Cert.Equal(old.Cert) {
		t.Error("the CA was kept")
	}
	if !strings.Contains(out.String(), now.CertFile) {
		t.Errorf("output %q does not name %s", out, now.CertFile)
	}
	if _, err := os.Stat(old.CertFile); err != nil {
		t.Errorf("the old certificate, which running containers mount, is gone: %v", err)
	}
}
