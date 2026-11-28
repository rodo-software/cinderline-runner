package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/name"
)

const port = 7420

// do sends method path to a guarded handler on dir, with Host
// 127.0.0.1:port and the given headers and cookie value ("" for none).
func do(t *testing.T, dir, method, path, cookie string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return doTo(t, Handler(dir, port, Handlers{}), method, path, cookie, header)
}

// doTo is do on h.
func doTo(t *testing.T, h http.Handler, method, path, cookie string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Host = "127.0.0.1:7420"
	for k, v := range header {
		if k == "Host" {
			r.Host = v
		} else {
			r.Header.Set(k, v)
		}
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: name.Name, Value: cookie})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// token is dir's token, made if missing.
func token(t *testing.T, dir string) string {
	t.Helper()
	tok, err := Token(dir)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestHandlerServesTheStaticFiles(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	for path, want := range map[string]string{"/": "<title>", "/app.css": "", "/mock-run.html": "<title>"} {
		w := do(t, dir, "GET", path, tok, nil)
		if w.Code != http.StatusOK || w.Body.Len() == 0 || !strings.Contains(w.Body.String(), want) {
			t.Errorf("GET %s = %d, %d bytes, want 200 containing %q", path, w.Code, w.Body.Len(), want)
		}
	}
	if w := do(t, dir, "GET", "/static/app.css", tok, nil); w.Code != http.StatusNotFound {
		t.Errorf("GET /static/app.css = %d, want 404: files are served from the root", w.Code)
	}
}

func TestHandlerServesTheOverview(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	for path, want := range map[string]string{
		"/":            "text/html; charset=utf-8",
		"/app.js":      "text/javascript; charset=utf-8",
		"/overview.js": "text/javascript; charset=utf-8",
	} {
		w := do(t, dir, "GET", path, tok, nil)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != want {
			t.Errorf("GET %s = %d, %q, want 200 %q", path, w.Code, w.Header().Get("Content-Type"), want)
		}
	}
	if w := do(t, dir, "GET", "/", tok, nil); !strings.Contains(w.Body.String(), `src="overview.js"`) {
		t.Error("GET / does not load overview.js")
	}
}

func TestHandlerServesTheLogPage(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	for path, want := range map[string]string{
		"/log.html": "text/html; charset=utf-8",
		"/log.js":   "text/javascript; charset=utf-8",
	} {
		w := do(t, dir, "GET", path, tok, nil)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != want {
			t.Errorf("GET %s = %d, %q, want 200 %q", path, w.Code, w.Header().Get("Content-Type"), want)
		}
	}
	if w := do(t, dir, "GET", "/log.html", tok, nil); !strings.Contains(w.Body.String(), `src="log.js"`) {
		t.Error("GET /log.html does not load log.js")
	}
}

func TestHandlerServesTheRunsPage(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	for path, want := range map[string]string{
		"/runs.html": "text/html; charset=utf-8",
		"/runs.js":   "text/javascript; charset=utf-8",
	} {
		w := do(t, dir, "GET", path, tok, nil)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != want {
			t.Errorf("GET %s = %d, %q, want 200 %q", path, w.Code, w.Header().Get("Content-Type"), want)
		}
	}
	if w := do(t, dir, "GET", "/runs.html", tok, nil); !strings.Contains(w.Body.String(), `src="runs.js"`) {
		t.Error("GET /runs.html does not load runs.js")
	}
}

func TestHandlerServesTheRunPage(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	for path, want := range map[string]string{
		"/run.html": "text/html; charset=utf-8",
		"/run.js":   "text/javascript; charset=utf-8",
	} {
		w := do(t, dir, "GET", path, tok, nil)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != want {
			t.Errorf("GET %s = %d, %q, want 200 %q", path, w.Code, w.Header().Get("Content-Type"), want)
		}
	}
	if w := do(t, dir, "GET", "/run.html?project=CLR&issue=CLR-1&run=x", tok, nil); !strings.Contains(w.Body.String(), `src="run.js"`) {
		t.Error("GET /run.html does not load run.js")
	}
}

// CLR-304: the Projects page, a project's page, and the command's name they
// show in the commands to type.
func TestHandlerServesTheProjectPages(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	for path, want := range map[string]string{
		"/projects.html": "text/html; charset=utf-8",
		"/projects.js":   "text/javascript; charset=utf-8",
		"/project.html":  "text/html; charset=utf-8",
		"/project.js":    "text/javascript; charset=utf-8",
		// CLR-307: the page that adds a project.
		"/project-add.html": "text/html; charset=utf-8",
		"/project-add.js":   "text/javascript; charset=utf-8",
	} {
		w := do(t, dir, "GET", path, tok, nil)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != want {
			t.Errorf("GET %s = %d, %q, want 200 %q", path, w.Code, w.Header().Get("Content-Type"), want)
		}
	}
	for page, script := range map[string]string{"/projects.html": "projects.js", "/project.html?key=CLR": "project.js", "/project-add.html": "project-add.js"} {
		if w := do(t, dir, "GET", page, tok, nil); !strings.Contains(w.Body.String(), `src="`+script+`"`) {
			t.Errorf("GET %s does not load %s", page, script)
		}
	}
	w := do(t, dir, "GET", "/api/name", tok, nil)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"name":"`+name.Name+`"}` {
		t.Errorf("GET /api/name = %d %s", w.Code, w.Body)
	}
}

// CLR-309: the Settings page, the runner-wide settings.
func TestHandlerServesTheSettingsPage(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	for path, want := range map[string]string{
		"/settings.html": "text/html; charset=utf-8",
		"/settings.js":   "text/javascript; charset=utf-8",
	} {
		w := do(t, dir, "GET", path, tok, nil)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != want {
			t.Errorf("GET %s = %d, %q, want 200 %q", path, w.Code, w.Header().Get("Content-Type"), want)
		}
	}
	if w := do(t, dir, "GET", "/settings.html", tok, nil); !strings.Contains(w.Body.String(), `src="settings.js"`) {
		t.Error("GET /settings.html does not load settings.js")
	}
}

// CLR-308: the Providers page, the runner's model providers.
func TestHandlerServesTheProvidersPage(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	for path, want := range map[string]string{
		"/providers.html": "text/html; charset=utf-8",
		"/providers.js":   "text/javascript; charset=utf-8",
	} {
		w := do(t, dir, "GET", path, tok, nil)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != want {
			t.Errorf("GET %s = %d, %q, want 200 %q", path, w.Code, w.Header().Get("Content-Type"), want)
		}
	}
	if w := do(t, dir, "GET", "/providers.html", tok, nil); !strings.Contains(w.Body.String(), `src="providers.js"`) {
		t.Error("GET /providers.html does not load providers.js")
	}
}

func TestTokenIsMadeOnceAndPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	tok := token(t, dir)
	if len(tok) != 64 {
		t.Errorf("token %q, want 64 hex characters", tok)
	}
	fi, err := os.Stat(filepath.Join(dir, "web-token"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("web-token mode %o, want 600", perm)
	}
	if again := token(t, dir); again != tok {
		t.Errorf("second Token = %q, want the first %q", again, tok)
	}
}

func TestTokenRefusesAnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "web-token"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Token(dir); err == nil {
		t.Error("Token of an empty file: no error")
	}
	// An empty token must never match an empty ?token=.
	if w := do(t, dir, "GET", "/?token=", "", nil); w.Code == http.StatusSeeOther {
		t.Error("an empty token file let an empty ?token= in")
	}
}

func TestWrongHostIsForbidden(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	for _, host := range []string{"evil.example:7420", "127.0.0.1:7421", "127.0.0.1", "localhost", "[::1]:7420"} {
		if w := do(t, dir, "GET", "/", tok, map[string]string{"Host": host}); w.Code != http.StatusForbidden {
			t.Errorf("Host %s: %d, want 403", host, w.Code)
		}
	}
	if w := do(t, dir, "GET", "/", tok, map[string]string{"Host": "localhost:7420"}); w.Code != http.StatusOK {
		t.Errorf("Host localhost:7420: %d, want 200", w.Code)
	}
}

func TestNoCookieIsUnauthorized(t *testing.T) {
	dir := t.TempDir()
	token(t, dir)
	w := do(t, dir, "GET", "/", "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no cookie: %d, want 401", w.Code)
	}
	if want := "Open this page with the command: " + name.Name + " web"; strings.TrimSpace(w.Body.String()) != want {
		t.Errorf("body %q, want %q", w.Body, want)
	}
}

func TestWrongCookieIsUnauthorized(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	if w := do(t, dir, "GET", "/", tok[1:]+"0", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong cookie: %d, want 401", w.Code)
	}
}

func TestRightTokenSetsTheCookie(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	w := do(t, dir, "GET", "/?token="+tok, "", nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("right token: %d to %q, want 303 to /", w.Code, w.Header().Get("Location"))
	}
	cs := w.Result().Cookies()
	if len(cs) != 1 {
		t.Fatalf("cookies %v, want one", cs)
	}
	c := cs[0]
	if c.Name != name.Name || c.Value != tok || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Errorf("cookie %+v, want %s=<token>, HttpOnly, SameSite=Strict, Path=/", c, name.Name)
	}
}

func TestWrongTokenIsUnauthorized(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	w := do(t, dir, "GET", "/?token=nope", tok, nil) // even with a right cookie
	if w.Code != http.StatusUnauthorized || len(w.Result().Cookies()) != 0 {
		t.Errorf("wrong token: %d, cookies %v, want 401 and none", w.Code, w.Result().Cookies())
	}
}

func TestReplacedTokenFileTakesEffect(t *testing.T) {
	dir := t.TempDir()
	old := token(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "web-token"), []byte(strings.Repeat("ab", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	if w := do(t, dir, "GET", "/", old, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("old cookie after a new token: %d, want 401", w.Code)
	}
}

func TestPostWithoutOriginIsForbidden(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	if w := do(t, dir, "POST", "/api/x", tok, nil); w.Code != http.StatusForbidden {
		t.Errorf("POST without Origin: %d, want 403", w.Code)
	}
}

func TestPostWithForeignOriginIsForbidden(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	for _, h := range []map[string]string{
		{"Origin": "http://evil.example"},
		{"Origin": "http://localhost:7420"}, // not the request's own Host
		{"Origin": "null"},
		{"Sec-Fetch-Site": "same-site"},
		{"Sec-Fetch-Site": "cross-site"},
	} {
		if w := do(t, dir, "POST", "/api/x", tok, h); w.Code != http.StatusForbidden {
			t.Errorf("POST with %v: %d, want 403", h, w.Code)
		}
	}
}

func TestPostWithRightOriginPasses(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	for _, h := range []map[string]string{
		{"Origin": "http://127.0.0.1:7420"},
		{"Sec-Fetch-Site": "same-origin"},
	} {
		// The file server answers POST like GET; what counts is that the guard let it through.
		if w := do(t, dir, "POST", "/", tok, h); w.Code != http.StatusOK {
			t.Errorf("POST with %v: %d, want 200", h, w.Code)
		}
	}
}

func TestHeaders(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	// A stand-in for the API's handlers: the file server drops Cache-Control on its errors.
	h := guard(dir, port, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, c := range []struct {
		path, cookie string
		noStore      bool
	}{
		{"/", tok, false},
		{"/", "", false}, // a refusal too
		{"/api/x", tok, true},
		{"/api/x", "", true},
	} {
		w := doTo(t, h, "GET", c.path, c.cookie, nil)
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s: X-Content-Type-Options %q, want nosniff", c.path, got)
		}
		if got := w.Header().Get("Cache-Control") == "no-store"; got != c.noStore {
			t.Errorf("GET %s: Cache-Control %q, want no-store %v", c.path, w.Header().Get("Cache-Control"), c.noStore)
		}
	}
}
