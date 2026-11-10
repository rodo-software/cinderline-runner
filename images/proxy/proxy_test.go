package proxyimage

import (
	"bytes"
	"os"
	"testing"
)

// CSR-63: the runner builds the proxy image on a machine without this
// repository, from the embedded Dockerfile and the embedded source, into a
// static binary on an empty base.
func TestProxyImageEmbedsItsSource(t *testing.T) {
	src, err := os.ReadFile("cmd/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(Main, src) {
		t.Error("Main is not cmd/main.go")
	}
	df, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(Dockerfile, df) {
		t.Error("Dockerfile is not images/proxy/Dockerfile")
	}
	for _, want := range []string{"COPY main.go", "CGO_ENABLED=0", "FROM scratch", "USER 65534:65534", `ENTRYPOINT ["/proxy"]`} {
		if !bytes.Contains(Dockerfile, []byte(want)) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
}
