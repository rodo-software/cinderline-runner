package manage

import (
	"os"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/ca"
	"github.com/rodo-software/cinderline-runner/internal/config"
)

// CLR-303: RotateCA replaces the CA in the state folder and keeps the old
// certificate's file, which running containers mount.
func TestRotateCA(t *testing.T) {
	p := config.Paths{State: t.TempDir()}
	old, err := ca.Ensure(ca.Dir(p.State), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, err := RotateCA(p)
	if err != nil {
		t.Fatal(err)
	}
	now, err := ca.Ensure(ca.Dir(p.State), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if now.Cert.Equal(old.Cert) || !now.Cert.Equal(got.Cert) {
		t.Error("the CA on disk is not the new one")
	}
	if _, err := os.Stat(old.CertFile); err != nil {
		t.Errorf("the old certificate is gone: %v", err)
	}
}
