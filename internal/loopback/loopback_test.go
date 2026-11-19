package loopback

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(Run(m)) }

// CLR-202: loopback answers; any other host is refused before it is dialled,
// with an error naming it, and recorded so the run fails.
func TestOnlyLoopbackIsReached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	for _, u := range []string{srv.URL, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)} {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		resp.Body.Close()
	}

	_, err := guard{}.RoundTrip(httptest.NewRequest(http.MethodGet, "http://provider.example:8080/v1/models", nil))
	if err == nil || !strings.Contains(err.Error(), "provider.example:8080") {
		t.Errorf("err = %v, want a refusal naming provider.example:8080", err)
	}
	mu.Lock()
	got := stray
	stray = nil // this refusal was on purpose: it must not fail the run
	mu.Unlock()
	if len(got) != 1 || got[0] != "GET http://provider.example:8080/v1/models" {
		t.Errorf("stray = %q", got)
	}
}
