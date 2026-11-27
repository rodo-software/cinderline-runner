package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesTheStaticFiles(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	for path, want := range map[string]string{"/": "<title>", "/app.css": "", "/mock-run.html": "<title>"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || len(body) == 0 || !strings.Contains(string(body), want) {
			t.Errorf("GET %s = %d, %d bytes, want 200 containing %q", path, resp.StatusCode, len(body), want)
		}
	}
	resp, err := http.Get(srv.URL + "/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /static/app.css = %d, want 404: files are served from the root", resp.StatusCode)
	}
}
