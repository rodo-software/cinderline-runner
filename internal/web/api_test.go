package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/manage"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

func TestAPIStatusAnswersTheStatus(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	st := localapi.Status{Version: "1.2.3", PID: 42, Projects: []localapi.ProjectStatus{{Project: "CLR", Connected: true}}, Memory: &localapi.Memory{Budget: 8, Reserved: 2}}
	w := doTo(t, Handler(dir, port, Handlers{Status: func() localapi.Status { return st }}), "GET", "/api/status", tok, nil)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("GET /api/status = %d, %q, want 200 application/json", w.Code, w.Header().Get("Content-Type"))
	}
	want, _ := json.Marshal(st)
	if got := strings.TrimSpace(w.Body.String()); got != string(want) {
		t.Errorf("body %s, want %s", got, want)
	}
}

// stream opens /api/status/stream on a server of h and returns its data
// lines (and event lines, whole) and its comment lines as they come; the stream ends with the test.
func stream(t *testing.T, h Handlers) (data, comments <-chan string) {
	t.Helper()
	return streamPath(t, h, "/api/status/stream")
}

// streamPath is stream of path.
func streamPath(t *testing.T, h Handlers, path string) (data, comments <-chan string) {
	t.Helper()
	dir := t.TempDir()
	tok := token(t, dir)
	srv := httptest.NewServer(Handler(dir, port, h))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequestWithContext(t.Context(), "GET", srv.URL+path, nil)
	req.Host = "127.0.0.1:7420"
	req.AddCookie(&http.Cookie{Name: name.Name, Value: tok})
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream = %d, %q, want 200 text/event-stream", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	d, c := make(chan string, 100), make(chan string, 100)
	go func() {
		s := bufio.NewScanner(resp.Body)
		for s.Scan() {
			switch line := s.Text(); {
			case strings.HasPrefix(line, "data: "):
				d <- strings.TrimPrefix(line, "data: ")
			case strings.HasPrefix(line, "event: "): // as is, so a test tells it from data
				d <- line
			case strings.HasPrefix(line, ":"):
				c <- line
			}
		}
	}()
	return d, c
}

// changing is a status whose version a test sets.
type changing struct {
	mu      sync.Mutex
	version string
}

func (c *changing) status() localapi.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return localapi.Status{Version: c.version}
}

func (c *changing) set(v string) {
	c.mu.Lock()
	c.version = v
	c.mu.Unlock()
}

func next(t *testing.T, ch <-chan string, what string) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(5 * time.Second):
		t.Fatalf("no %s", what)
		return ""
	}
}

func TestStreamSendsTheStatusAtOnce(t *testing.T) {
	c := &changing{version: "1"}
	data, _ := stream(t, Handlers{Status: c.status, Tick: time.Hour}) // no tick comes: the first event must not wait for one
	if got := next(t, data, "first event"); got != `{"version":"1","pid":0,"projects":null}` {
		t.Errorf("first event %s", got)
	}
}

func TestStreamSendsAChangeOnly(t *testing.T) {
	c := &changing{version: "1"}
	data, _ := stream(t, Handlers{Status: c.status, Tick: time.Millisecond})
	next(t, data, "first event")
	select {
	case got := <-data:
		t.Fatalf("event %s without a change", got)
	case <-time.After(50 * time.Millisecond): // fifty ticks
	}
	c.set("2")
	if got := next(t, data, "event after a change"); !strings.Contains(got, `"version":"2"`) {
		t.Errorf("event after a change %s, want version 2", got)
	}
}

func TestStreamPings(t *testing.T) {
	c := &changing{version: "1"}
	_, comments := stream(t, Handlers{Status: c.status, Tick: time.Millisecond})
	if got := next(t, comments, "ping"); got != ": ping" {
		t.Errorf("comment %q, want \": ping\"", got)
	}
}

// following is a log of tail whose new lines a test sends; n is the tail
// length the route asked for.
type following struct {
	tail  string
	lines chan []byte
	n     chan int
}

func newFollowing(tail string) *following {
	return &following{tail: tail, lines: make(chan []byte, 10), n: make(chan int, 1)}
}

func (f *following) follow(n int) ([]byte, <-chan []byte, func(), error) {
	f.n <- n
	return []byte(f.tail), f.lines, func() {}, nil
}

func TestLogSendsTheTailThenNewLines(t *testing.T) {
	f := newFollowing(`{"msg":"one"}` + "\n" + `{"msg":"two"}` + "\n")
	data, _ := streamPath(t, Handlers{Follow: f.follow}, "/api/log?tail=2")
	if n := <-f.n; n != 2 {
		t.Errorf("followed %d lines, want 2", n)
	}
	for _, want := range []string{`{"msg":"one"}`, `{"msg":"two"}`} {
		if got := next(t, data, "tail line"); got != want {
			t.Errorf("tail event %s, want %s", got, want)
		}
	}
	f.lines <- []byte(`{"msg":"three"}` + "\n")
	if got := next(t, data, "new line"); got != `{"msg":"three"}` {
		t.Errorf("new event %s, want {\"msg\":\"three\"}", got)
	}
}

func TestLogTailDefaultsTo200(t *testing.T) {
	f := newFollowing("")
	streamPath(t, Handlers{Follow: f.follow}, "/api/log")
	if n := <-f.n; n != 200 {
		t.Errorf("followed %d lines, want 200", n)
	}
}

func TestLogRefusesABadTail(t *testing.T) {
	dir := t.TempDir()
	tok := token(t, dir)
	f := newFollowing("")
	for _, tail := range []string{"x", "-1", "5001", "1.5"} {
		w := doTo(t, Handler(dir, port, Handlers{Follow: f.follow}), "GET", "/api/log?tail="+tail, tok, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("tail=%q: %d, want 400", tail, w.Code)
		}
	}
	streamPath(t, Handlers{Follow: f.follow}, "/api/log?tail=5000") // fails the test unless 200
	if n := <-f.n; n != 5000 {
		t.Errorf("followed %d lines, want 5000", n)
	}
}

// configHandler serves the config API on a config.json with one project,
// counting the reloads, which answer reloadErr.
func configHandler(t *testing.T, reloadErr error) (h http.Handler, tok string, paths config.Paths, reloads *int) {
	t.Helper()
	dir := t.TempDir()
	paths = config.Paths{Dir: dir, File: filepath.Join(dir, "config.json"), State: dir}
	web := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"}
	if err := (&config.Config{Projects: []config.Project{web}, MaxParallelRuns: 3}).Save(paths.File); err != nil {
		t.Fatal(err)
	}
	reloads = new(int)
	keyring.MockInit() // no test of this package may use t.Parallel
	h = Handler(dir, port, Handlers{Paths: paths, Reload: func() error { *reloads++; return reloadErr },
		Secrets: secrets.New(dir), Exec: lsRemote, Log: slog.New(slog.NewTextHandler(&logged, nil))})
	return h, token(t, dir), paths, reloads
}

// repoToken is the one token lsRemote accepts.
const repoToken = "the-good-git-token"

// logged is what the config API logged, for the tests that look for a token in it.
var logged bytes.Buffer

// lsRemote is a git check that accepts only repoToken; its refusal echoes
// the token it got, as a careless git could.
func lsRemote(_ context.Context, env []string, _ ...string) (string, error) {
	if !slices.Equal(env, []string{"GIT_TOKEN=" + repoToken}) {
		return "", errors.New("authentication failed for " + strings.Join(env, " "))
	}
	return "", nil
}

// put sends body to path of h as the page does: same origin, with the token.
func put(t *testing.T, h http.Handler, tok, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return send(t, h, "PUT", tok, path, body)
}

// send is put with another method.
func send(t *testing.T, h http.Handler, method, tok, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "127.0.0.1:7420"
	r.Header.Set("Origin", "http://127.0.0.1:7420")
	r.AddCookie(&http.Cookie{Name: name.Name, Value: tok})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// CLR-294: GET /api/config answers config.json, whether the page is on and
// where, and the defaults of what the config leaves unset.
func TestAPIConfig(t *testing.T) {
	h, tok, _, _ := configHandler(t, nil)
	var got struct {
		Config   config.Config `json:"config"`
		WebOn    *bool         `json:"web_on"`
		WebAddr  string        `json:"web_addr"`
		Defaults struct {
			Memory            string `json:"memory"`
			MaxParallelRuns   int    `json:"max_parallel_runs"`
			TranscriptDays    int    `json:"transcript_days"`
			TranscriptMaxSize string `json:"transcript_max_size"`
		} `json:"defaults"`
	}
	getJSON(t, h, tok, "/api/config", &got)
	if len(got.Config.Projects) != 1 || got.Config.Projects[0].Key != "WEB" || got.Config.MaxParallelRuns != 3 {
		t.Errorf("config %+v, want config.json's", got.Config)
	}
	if got.WebOn == nil || *got.WebOn || got.WebAddr != "127.0.0.1:7420" {
		t.Errorf("web_on %v, web_addr %q; want false, 127.0.0.1:7420", got.WebOn, got.WebAddr)
	}
	var ids struct {
		Config struct {
			Projects []struct{ ID, Label string } `json:"projects"`
		} `json:"config"`
	}
	getJSON(t, h, tok, "/api/config", &ids)
	if p := ids.Config.Projects; len(p) != 1 || p[0].ID != "WEB" || p[0].Label != "WEB" {
		t.Errorf("projects %+v, want the ID and label WEB, which config.json leaves out", p)
	}
	d := got.Defaults
	if d.Memory != "4g" || d.MaxParallelRuns != 1 || d.TranscriptDays != 14 || d.TranscriptMaxSize != "1g" {
		t.Errorf("defaults %+v", d)
	}
}

// The PUT writes config.json, then reloads the daemon.
func TestAPIStrictHosts(t *testing.T) {
	h, tok, paths, reloads := configHandler(t, nil)
	for _, on := range []bool{true, false} {
		w := put(t, h, tok, "/api/settings/strict-hosts", fmt.Sprintf(`{"on": %v}`, on))
		if w.Code != http.StatusNoContent {
			t.Fatalf("PUT on %v = %d %s, want 204", on, w.Code, w.Body)
		}
		cfg, err := config.Load(paths.File)
		if err != nil || cfg.StrictHosts != on || len(cfg.Projects) != 1 {
			t.Errorf("on %v: config %+v, %v", on, cfg, err)
		}
	}
	if *reloads != 2 {
		t.Errorf("%d reloads, want 2", *reloads)
	}
}

// A body the route cannot take is a 400 that changes and reloads nothing.
func TestAPIWriteRefusesABadBody(t *testing.T) {
	h, tok, paths, reloads := configHandler(t, nil)
	for what, body := range map[string]string{
		"unknown field":    `{"on": true, "off": false}`,
		"too large":        `{"on":` + strings.Repeat(" ", 64<<10) + `true}`, // valid but for its size
		"missing on":       `{}`,
		"empty":            ``, // decodes as {}
		"not JSON":         `on`,
		"two values":       `{"on": true} {"on": true}`,
		"wrong type of on": `{"on": "yes"}`,
	} {
		w := put(t, h, tok, "/api/settings/strict-hosts", body)
		var e struct{ Error string }
		if json.Unmarshal(w.Body.Bytes(), &e); w.Code != http.StatusBadRequest || e.Error == "" || w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: %d %s, want 400 with an error", what, w.Code, w.Body)
		}
	}
	if cfg, err := config.Load(paths.File); err != nil || cfg.StrictHosts || *reloads != 0 {
		t.Errorf("config %+v, %v, %d reloads; want it unchanged", cfg, err, *reloads)
	}
}

// A failed reload is answered as an error, after the write.
func TestAPIWriteReloadFails(t *testing.T) {
	h, tok, paths, _ := configHandler(t, errors.New("the daemon is stopping"))
	w := put(t, h, tok, "/api/settings/strict-hosts", `{"on": true}`)
	var e struct{ Error string }
	if json.Unmarshal(w.Body.Bytes(), &e); w.Code != http.StatusInternalServerError || !strings.Contains(e.Error, "stopping") {
		t.Errorf("PUT = %d %s, want 500 with the reload's error", w.Code, w.Body)
	}
	if cfg, err := config.Load(paths.File); err != nil || !cfg.StrictHosts {
		t.Errorf("config %+v, %v; want it written", cfg, err)
	}
}

// writeError tells a refused value from any other failure.
func TestWriteError(t *testing.T) {
	for err, want := range map[error]int{
		fmt.Errorf("save: %w", manage.Refused{Err: errors.New("max_parallel_runs must be 1 to 10")}): http.StatusBadRequest,
		errors.New("disk full"): http.StatusInternalServerError,
	} {
		w := httptest.NewRecorder()
		writeError(w, err)
		var e struct{ Error string }
		if json.Unmarshal(w.Body.Bytes(), &e); w.Code != want || e.Error == "" {
			t.Errorf("%v: %d %s, want %d", err, w.Code, w.Body, want)
		}
	}
}

// CLR-298: POST adds an extra repository with its token, GET /api/config
// says it has one, and DELETE removes both; each write reloads.
func TestAPIRepos(t *testing.T) {
	h, tok, paths, reloads := configHandler(t, nil)
	w := send(t, h, "POST", tok, "/api/projects/WEB/repos", `{"url": "https://git.example.com/o/lib", "ref": "v1.0", "token": "`+repoToken+`"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("POST = %d %s, want 204", w.Code, w.Body)
	}
	var got struct {
		Config struct {
			Projects []struct {
				Key               string `json:"key"`
				ExtraRepositories []struct {
					URL      string `json:"url"`
					Ref      string `json:"ref"`
					HasToken *bool  `json:"has_token"`
				} `json:"extra_repositories"`
			} `json:"projects"`
		} `json:"config"`
	}
	getJSON(t, h, tok, "/api/config", &got)
	ps := got.Config.Projects
	if len(ps) != 1 || ps[0].Key != "WEB" || len(ps[0].ExtraRepositories) != 1 {
		t.Fatalf("config projects %+v", ps)
	}
	if r := ps[0].ExtraRepositories[0]; r.URL != "https://git.example.com/o/lib" || r.Ref != "v1.0" || r.HasToken == nil || !*r.HasToken {
		t.Errorf("extra repository %+v, want lib at v1.0 with has_token true", r)
	}

	// Without its token the page sees has_token false.
	cfg, _ := config.Load(paths.File)
	secrets.New(paths.State).Delete(cfg.Projects[0], secrets.ExtraGitToken("https://git.example.com/o/lib"))
	getJSON(t, h, tok, "/api/config", &got)
	if r := got.Config.Projects[0].ExtraRepositories[0]; r.HasToken == nil || *r.HasToken {
		t.Errorf("has_token %v after the token was deleted, want false", r.HasToken)
	}

	if w := send(t, h, "DELETE", tok, "/api/projects/WEB/repos", `{"repo": "lib"}`); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", w.Code, w.Body)
	}
	if cfg, err := config.Load(paths.File); err != nil || len(cfg.Projects[0].ExtraRepositories) != 0 {
		t.Errorf("config %+v, %v after DELETE", cfg, err)
	}
	if *reloads != 2 {
		t.Errorf("%d reloads, want 2", *reloads)
	}
}

// A refused add or remove is a 400 that changes and reloads nothing, and
// no answer nor log line holds the token.
func TestAPIReposRefuses(t *testing.T) {
	h, tok, paths, reloads := configHandler(t, nil)
	logged.Reset()
	const wrong = "wrong-token-x"
	for what, c := range map[string]struct{ method, path, body, want string }{
		"no such project":    {"POST", "/api/projects/NOPE/repos", `{"url": "https://git.example.com/o/lib", "token": "` + repoToken + `"}`, "no project NOPE"},
		"own repository":     {"POST", "/api/projects/WEB/repos", `{"url": "https://git.example.com/o/web", "token": "` + repoToken + `"}`, "own repository"},
		"bad ref":            {"POST", "/api/projects/WEB/repos", `{"url": "https://git.example.com/o/lib", "ref": "a..b", "token": "` + repoToken + `"}`, "not a plain branch or tag"},
		"no token":           {"POST", "/api/projects/WEB/repos", `{"url": "https://git.example.com/o/lib"}`, "a git token is needed"},
		"token cannot read":  {"POST", "/api/projects/WEB/repos", `{"url": "https://git.example.com/o/lib", "token": "` + wrong + `"}`, "cannot read"},
		"unknown field":      {"POST", "/api/projects/WEB/repos", `{"url": "https://git.example.com/o/lib", "token": "` + repoToken + `", "x": 1}`, "unknown field"},
		"no such repository": {"DELETE", "/api/projects/WEB/repos", `{"repo": "lib"}`, "no extra repository lib"},
	} {
		w := send(t, h, c.method, tok, c.path, c.body)
		var e struct{ Error string }
		if json.Unmarshal(w.Body.Bytes(), &e); w.Code != http.StatusBadRequest || !strings.Contains(e.Error, c.want) {
			t.Errorf("%s: %d %s, want 400 containing %q", what, w.Code, w.Body, c.want)
		}
		for _, secret := range []string{repoToken, wrong} {
			if strings.Contains(w.Body.String(), secret) || strings.Contains(logged.String(), secret) {
				t.Errorf("%s: the answer or log holds a token: %s / %s", what, w.Body, &logged)
			}
		}
	}
	if cfg, err := config.Load(paths.File); err != nil || len(cfg.Projects[0].ExtraRepositories) != 0 || *reloads != 0 {
		t.Errorf("config %+v, %v, %d reloads; want it unchanged", cfg, err, *reloads)
	}
}

// providerKey is the key the provider tests send, which no answer or log may hold.
const providerKey = "sk-the-provider-key"

// configText is GET /api/config as text, which must never hold providerKey.
func configText(t *testing.T, h http.Handler, tok string) string {
	t.Helper()
	w := doTo(t, h, "GET", "/api/config", tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), providerKey) {
		t.Fatalf("GET /api/config holds the key: %s", w.Body)
	}
	return w.Body.String()
}

// CLR-299: POST adds a provider with its key, GET /api/config says it has
// one, PUT changes it, a PUT that changes nothing does not reload, and
// DELETE, without a body, removes it and its key.
func TestAPIProviders(t *testing.T) {
	h, tok, paths, reloads := configHandler(t, nil)
	logged.Reset()
	for _, body := range []string{
		`{"name": "lan", "base_url": "http://provider.example:8080/v1", "free": true, "max_agents": 2, "lookahead": 1, "key": "` + providerKey + `"}`,
		`{"name": "nokey", "base_url": "http://other.example/v1", "key": ""}`,
	} {
		if w := send(t, h, "POST", tok, "/api/providers", body); w.Code != http.StatusNoContent || strings.Contains(w.Body.String(), providerKey) {
			t.Fatalf("POST = %d %s, want 204", w.Code, w.Body)
		}
	}
	var got struct {
		Config struct {
			Providers []struct {
				config.Provider
				HasKey *bool `json:"has_key"`
			} `json:"providers"`
		} `json:"config"`
	}
	if err := json.Unmarshal([]byte(configText(t, h, tok)), &got); err != nil {
		t.Fatal(err)
	}
	ps := got.Config.Providers
	want := []config.Provider{{Name: "lan", BaseURL: "http://provider.example:8080/v1", Free: true, MaxAgents: 2, Lookahead: 1}, {Name: "nokey", BaseURL: "http://other.example/v1"}}
	if len(ps) != 2 || ps[0].Provider != want[0] || ps[1].Provider != want[1] || ps[0].HasKey == nil || !*ps[0].HasKey || ps[1].HasKey == nil || *ps[1].HasKey {
		t.Fatalf("providers %+v, want %v with has_key true, false", ps, want)
	}

	if w := put(t, h, tok, "/api/providers/lan", `{"free": false, "max_agents": 3}`); w.Code != http.StatusNoContent {
		t.Fatalf("PUT = %d %s, want 204", w.Code, w.Body)
	}
	cfg, err := config.Load(paths.File)
	if err != nil || cfg.Providers[0].Free || cfg.Providers[0].MaxAgents != 3 || cfg.Providers[0].Lookahead != 1 {
		t.Fatalf("config %+v, %v after PUT", cfg, err)
	}
	if w := put(t, h, tok, "/api/providers/lan", `{"max_agents": 3}`); w.Code != http.StatusNoContent || *reloads != 3 {
		t.Fatalf("unchanged PUT = %d %s after %d reloads, want 204 and still 3", w.Code, w.Body, *reloads)
	}

	if w := send(t, h, "DELETE", tok, "/api/providers/lan", ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", w.Code, w.Body)
	}
	if cfg, err := config.Load(paths.File); err != nil || len(cfg.Providers) != 1 || cfg.Providers[0].Name != "nokey" {
		t.Errorf("config %+v, %v after DELETE", cfg, err)
	}
	if _, _, err := secrets.New(paths.State).Get(secrets.Runner, secrets.ProviderKey("lan")); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("key after DELETE: %v, want none", err)
	}
	if *reloads != 4 {
		t.Errorf("%d reloads, want 4", *reloads)
	}
	if strings.Contains(logged.String(), providerKey) {
		t.Errorf("the log holds the key: %s", &logged)
	}
}

// A refused add, set or remove is a 400 that changes and reloads nothing,
// and no answer nor log line holds the key.
func TestAPIProvidersRefuse(t *testing.T) {
	h, tok, paths, reloads := configHandler(t, nil)
	if w := send(t, h, "POST", tok, "/api/providers", `{"name": "lan", "base_url": "http://provider.example:8080/v1", "max_agents": 1, "lookahead": 1}`); w.Code != http.StatusNoContent {
		t.Fatalf("POST = %d %s", w.Code, w.Body)
	}
	*reloads = 0
	logged.Reset()
	add := func(name, url, extra string) string {
		return `{"name": "` + name + `", "base_url": "` + url + `", "key": "` + providerKey + `"` + extra + `}`
	}
	for what, c := range map[string]struct{ method, path, body, want string }{
		"bad name":        {"POST", "/api/providers", add("Lan", "http://h:1/v1", ""), "provider name"},
		"key in the URL":  {"POST", "/api/providers", add("x", "http://u:"+providerKey+"@h:1/v1", ""), "base URL"},
		"duplicate":       {"POST", "/api/providers", add("lan", "http://other:1/v1", ""), "already has a provider lan"},
		"web page's port": {"POST", "/api/providers", add("x", "http://localhost:7420/v1", ""), "web page's port"},
		"max_agents":      {"POST", "/api/providers", add("x", "http://h:1/v1", `, "max_agents": 101`), "max_agents"},
		"unknown field":   {"POST", "/api/providers", add("x", "http://h:1/v1", `, "x": 1`), "unknown field"},
		"set nothing":     {"PUT", "/api/providers/lan", `{}`, "nothing to set"},
		"set unknown":     {"PUT", "/api/providers/nope", `{"free": true}`, "no provider nope"},
		"lookahead alone": {"PUT", "/api/providers/lan", `{"max_agents": 0}`, "needs max_agents"},
		"set the key":     {"PUT", "/api/providers/lan", `{"key": "` + providerKey + `"}`, "unknown field"},
		"remove unknown":  {"DELETE", "/api/providers/nope", ``, "no provider nope"},
	} {
		w := send(t, h, c.method, tok, c.path, c.body)
		var e struct{ Error string }
		if json.Unmarshal(w.Body.Bytes(), &e); w.Code != http.StatusBadRequest || !strings.Contains(e.Error, c.want) {
			t.Errorf("%s: %d %s, want 400 containing %q", what, w.Code, w.Body, c.want)
		}
		if strings.Contains(w.Body.String(), providerKey) || strings.Contains(logged.String(), providerKey) {
			t.Errorf("%s: the answer or log holds the key: %s / %s", what, w.Body, &logged)
		}
	}
	configText(t, h, tok)
	cfg, err := config.Load(paths.File)
	if err != nil || len(cfg.Providers) != 1 || cfg.Providers[0].Lookahead != 1 || *reloads != 0 {
		t.Errorf("config %+v, %v, %d reloads; want it unchanged", cfg, err, *reloads)
	}
	if _, _, err := secrets.New(paths.State).Get(secrets.Runner, secrets.ProviderKey("x")); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("a refused add stored a key: %v", err)
	}
}

// keysInstance is a Cinderline instance that takes only runnerToken, as
// project WEB's, and the config.json of configHandler moved to it, with
// provider lan; its refusal echoes the token it got, as a careless app could.
func keysInstance(t *testing.T, paths config.Paths) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); tok != runnerToken {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"bad token ` + tok + `"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"poll_after":30,"project":{"key":"WEB","name":"Web","repository_url":"https://git.example.com/o/web","forge":"forgejo","base_branch":"main","environment":"default"}}`))
	}))
	t.Cleanup(srv.Close)
	cfg, err := config.Load(paths.File)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Projects[0].URL = srv.URL
	cfg.Providers = []config.Provider{{Name: "lan", BaseURL: "http://provider.example:8080/v1"}}
	if err := cfg.Save(paths.File); err != nil {
		t.Fatal(err)
	}
}

const runnerToken = "the-good-runner-token"

// keyList is GET /api/projects/WEB/keys as kind: set.
func keyList(t *testing.T, h http.Handler, tok string) map[string]bool {
	t.Helper()
	var ks []struct {
		Kind string `json:"kind"`
		Set  *bool  `json:"set"`
	}
	getJSON(t, h, tok, "/api/projects/WEB/keys", &ks)
	m := map[string]bool{}
	for _, k := range ks {
		if k.Set == nil {
			t.Fatalf("key %s has no set", k.Kind)
		}
		m[k.Kind] = *k.Set
	}
	return m
}

// CLR-300: PUT sets each key of a project and a provider's, GET lists
// which are set, DELETE deletes them; each write reloads. No answer and
// no log line holds a value.
func TestAPIKeys(t *testing.T) {
	h, tok, paths, reloads := configHandler(t, nil)
	keysInstance(t, paths)
	logged.Reset()
	var answers strings.Builder
	do := func(method, path, body string, want int) {
		t.Helper()
		w := send(t, h, method, tok, path, body)
		answers.WriteString(w.Body.String())
		if w.Code != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, w.Code, w.Body, want)
		}
	}
	values := map[string]string{"runner-token": runnerToken}
	for _, n := range []string{"git-token", "claude", "opencode", "codex", "gemini"} {
		values[n] = "secret-value-of-" + n
	}
	for n, v := range values {
		do("PUT", "/api/projects/WEB/keys/"+n, `{"value": "`+v+`"}`, http.StatusNoContent)
	}
	do("PUT", "/api/providers/lan/key", `{"value": "`+providerKey+`"}`, http.StatusNoContent)

	p, err := manage.FindProject(paths, "WEB")
	if err != nil {
		t.Fatal(err)
	}
	sec := secrets.New(paths.State)
	for n, v := range values {
		k := secrets.Kind(n)
		if c, ok := secrets.ClientKeys[n]; ok {
			k = c
		}
		if got, _, err := sec.Get(p, k); err != nil || got != v {
			t.Errorf("%s = %q, %v; want it stored", n, got, err)
		}
	}
	if got, _, err := sec.Get(secrets.Runner, secrets.ProviderKey("lan")); err != nil || got != providerKey {
		t.Errorf("lan's key = %q, %v", got, err)
	}
	got := keyList(t, h, tok)
	answers.WriteString(configText(t, h, tok))
	for n := range values {
		if !got[n] {
			t.Errorf("list %v: %s not set", got, n)
		}
	}
	if len(got) != len(values) {
		t.Errorf("list %v, want the %d kinds", got, len(values))
	}

	for n := range values {
		do("DELETE", "/api/projects/WEB/keys/"+n, "", http.StatusNoContent)
	}
	do("DELETE", "/api/providers/lan/key", "", http.StatusNoContent)
	for n, set := range keyList(t, h, tok) {
		if set {
			t.Errorf("%s still set after DELETE", n)
		}
	}
	if _, _, err := sec.Get(secrets.Runner, secrets.ProviderKey("lan")); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("lan's key after DELETE: %v", err)
	}
	if want := 2 * (len(values) + 1); *reloads != want {
		t.Errorf("%d reloads, want %d", *reloads, want)
	}
	for _, v := range append(slices.Collect(maps.Values(values)), providerKey) {
		if strings.Contains(answers.String(), v) || strings.Contains(logged.String(), v) {
			t.Errorf("an answer or the log holds %q:\n%s\n%s", v, &answers, &logged)
		}
	}
}

// A refused write is a 400 that stores and reloads nothing, and neither
// its answer nor the log holds the value.
func TestAPIKeysRefuse(t *testing.T) {
	h, tok, paths, reloads := configHandler(t, nil)
	keysInstance(t, paths)
	logged.Reset()
	const v = "a-secret-value"
	body := `{"value": "` + v + `"}`
	for what, c := range map[string]struct{ method, path, body, want string }{
		"empty value":           {"PUT", "/api/projects/WEB/keys/claude", `{"value": ""}`, "empty value"},
		"no value":              {"PUT", "/api/projects/WEB/keys/claude", `{}`, "empty value"},
		"no body":               {"PUT", "/api/projects/WEB/keys/claude", ``, "empty value"},
		"unknown field":         {"PUT", "/api/projects/WEB/keys/claude", `{"value": "` + v + `", "x": 1}`, "unknown field"},
		"unknown kind":          {"PUT", "/api/projects/WEB/keys/aider", body, "unknown key"},
		"extra repo's token":    {"PUT", "/api/projects/WEB/keys/git-token:x", body, "unknown key"},
		"provider via project":  {"PUT", "/api/projects/WEB/keys/provider:lan", body, "belongs to no project"},
		"unknown project":       {"PUT", "/api/projects/NOPE/keys/claude", body, "no project NOPE"},
		"rejected runner token": {"PUT", "/api/projects/WEB/keys/runner-token", body, "rejected this runner token"},
		"unknown provider":      {"PUT", "/api/providers/nope/key", body, "no provider nope"},
		"empty provider key":    {"PUT", "/api/providers/lan/key", `{"value": ""}`, "empty value"},
		"delete unknown kind":   {"DELETE", "/api/projects/WEB/keys/aider", ``, "unknown key"},
		"delete unknown proj":   {"DELETE", "/api/projects/NOPE/keys/claude", ``, "no project NOPE"},
		"delete unknown prov":   {"DELETE", "/api/providers/nope/key", ``, "no provider nope"},
		"list unknown project":  {"GET", "/api/projects/NOPE/keys", ``, "no project NOPE"},
	} {
		w := send(t, h, c.method, tok, c.path, c.body)
		var e struct{ Error string }
		if json.Unmarshal(w.Body.Bytes(), &e); w.Code != http.StatusBadRequest || !strings.Contains(e.Error, c.want) {
			t.Errorf("%s: %d %s, want 400 containing %q", what, w.Code, w.Body, c.want)
		}
		if strings.Contains(w.Body.String(), v) || strings.Contains(logged.String(), v) {
			t.Errorf("%s: the answer or log holds the value: %s / %s", what, w.Body, &logged)
		}
	}
	for n, set := range keyList(t, h, tok) {
		if set {
			t.Errorf("%s set by a refused write", n)
		}
	}
	if *reloads != 0 {
		t.Errorf("%d reloads, want 0", *reloads)
	}
}

// CLR-295: POST /api/hosts adds an allow_hosts entry, runner-wide for an
// empty project, and DELETE removes it; each reloads.
func TestAPIHosts(t *testing.T) {
	h, tok, paths, reloads := configHandler(t, nil)
	for _, c := range []struct{ method, body string }{
		{"POST", `{"project": "", "host": "PyPI.org:443"}`},
		{"POST", `{"project": "WEB", "host": "files.example.com:443"}`},
		{"POST", `{"project": "WEB", "host": "mirror.example.com:443"}`},
		{"DELETE", `{"project": "WEB", "host": "files.example.com:443"}`},
	} {
		if w := send(t, h, c.method, tok, "/api/hosts", c.body); w.Code != http.StatusNoContent {
			t.Fatalf("%s %s = %d %s, want 204", c.method, c.body, w.Code, w.Body)
		}
	}
	cfg, err := config.Load(paths.File)
	if err != nil || !slices.Equal(cfg.AllowHosts, []string{"pypi.org:443"}) || !slices.Equal(cfg.Projects[0].AllowHosts, []string{"mirror.example.com:443"}) {
		t.Errorf("config %+v, %v", cfg, err)
	}
	if *reloads != 4 {
		t.Errorf("%d reloads, want 4", *reloads)
	}
}

// A refused host or project is a 400 with the command's text that changes
// and reloads nothing.
func TestAPIHostsRefuse(t *testing.T) {
	h, tok, paths, reloads := configHandler(t, nil)
	if w := send(t, h, "POST", tok, "/api/hosts", `{"project": "WEB", "host": "pypi.org:443"}`); w.Code != http.StatusNoContent {
		t.Fatalf("POST = %d %s", w.Code, w.Body)
	}
	*reloads = 0
	for what, c := range map[string]struct{ method, body, want string }{
		"wildcard":        {"POST", `{"project": "WEB", "host": "*.example.com:443"}`, "must be host:port, without wildcards"},
		"runner's own":    {"POST", `{"project": "", "host": "localhost:11434"}`, "is the runner's own machine"},
		"duplicate":       {"POST", `{"project": "WEB", "host": "PyPI.org:443"}`, "project WEB already allows pypi.org:443"},
		"unknown project": {"POST", `{"project": "NOPE", "host": "x.example.com:443"}`, "no project NOPE"},
		"remove missing":  {"DELETE", `{"project": "WEB", "host": "x.example.com:443"}`, "project WEB has no allow_hosts entry x.example.com:443; its entries: pypi.org:443"},
		"remove unknown":  {"DELETE", `{"project": "NOPE", "host": "pypi.org:443"}`, "no project NOPE"},
		"unknown field":   {"POST", `{"project": "WEB", "host": "x.example.com:443", "url": "https://a.example.com"}`, "unknown field"},
	} {
		w := send(t, h, c.method, tok, "/api/hosts", c.body)
		var e struct{ Error string }
		if json.Unmarshal(w.Body.Bytes(), &e); w.Code != http.StatusBadRequest || !strings.Contains(e.Error, c.want) {
			t.Errorf("%s: %d %s, want 400 containing %q", what, w.Code, w.Body, c.want)
		}
	}
	cfg, err := config.Load(paths.File)
	if err != nil || len(cfg.AllowHosts) != 0 || !slices.Equal(cfg.Projects[0].AllowHosts, []string{"pypi.org:443"}) || *reloads != 0 {
		t.Errorf("config %+v, %v, %d reloads; want it unchanged", cfg, err, *reloads)
	}
}
