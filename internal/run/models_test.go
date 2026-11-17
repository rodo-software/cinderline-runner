package run

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// modelServer answers GET /v1/models with body and status, and keeps the
// Authorization header it saw last.
func modelServer(t *testing.T, status int, body string) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var auth string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s, func() string {
		mu.Lock()
		defer mu.Unlock()
		return auth
	}
}

const modelList = `{"object":"list","data":[{"id":"llama3.1:8b","object":"model"},{"id":"qwen/qwen3-coder"}]}`

// Decision 2026-11-17-provider-models: the runner's providers are asked for
// their models, with the provider's key when it has one, and reported in
// the config's order.
func TestWatchReportsEachProvidersModels(t *testing.T) {
	keyed, keyedAuth := modelServer(t, 200, modelList)
	open, openAuth := modelServer(t, 200, `{"data":[{"id":"gemma"}]}`)
	f := newFixture(t)
	if _, err := f.p.Secrets.Set(secrets.Runner, secrets.ProviderKey("studio"), "sk-studio"); err != nil {
		t.Fatal(err)
	}
	providers := []config.Provider{{Name: "studio", BaseURL: keyed.URL + "/v1/"}, {Name: "lan", BaseURL: open.URL + "/v1"}}
	m := &Models{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Watch(ctx, providers, f.p.Secrets, discard()); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(m.Get()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	want := []cinderline.ProviderModels{{Provider: "studio", Models: []string{"llama3.1:8b", "qwen/qwen3-coder"}}, {Provider: "lan", Models: []string{"gemma"}}}
	if got := m.Get(); !slices.EqualFunc(got, want, equalModels) {
		t.Errorf("models = %+v, want %+v", got, want)
	}
	if keyedAuth() != "Bearer sk-studio" || openAuth() != "" {
		t.Errorf("authorization %q and %q, want the key only for studio", keyedAuth(), openAuth())
	}
	f.p.Models = m
	l := &loop{p: f.p, c: cinderline.New("http://x", "t", "0")}
	if got := l.report().ProviderModels; !slices.EqualFunc(got, want, equalModels) {
		t.Errorf("report provider_models = %+v", got)
	}
}

func equalModels(a, b cinderline.ProviderModels) bool {
	return a.Provider == b.Provider && slices.Equal(a.Models, b.Models)
}

// A provider that answers with anything but a model list, or not in time,
// has no models; that is not an error of the runner's report.
func TestAProviderWithoutAModelListHasNoModels(t *testing.T) {
	defer func(d time.Duration) { modelsTimeout = d }(modelsTimeout)
	modelsTimeout = 50 * time.Millisecond
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	html, _ := modelServer(t, 200, "<html>not here</html>")
	array, _ := modelServer(t, 200, `[{"id":"x"}]`)
	refused, _ := modelServer(t, 401, modelList)
	other, _ := modelServer(t, 200, `{"models":["x"]}`)
	for name, url := range map[string]string{"timeout": slow.URL, "html": html.URL, "array": array.URL, "401": refused.URL, "other": other.URL, "down": "http://127.0.0.1:1"} {
		m := &Models{}
		m.refresh(context.Background(), []config.Provider{{Name: "p", BaseURL: url + "/v1"}}, nil, discard())
		if got := m.Get(); len(got) != 1 || got[0].Provider != "p" || got[0].Models == nil || len(got[0].Models) != 0 {
			t.Errorf("%s: models = %+v, want p with an empty list", name, got)
		}
	}
}

// A provider on the runner's own machine may be configured as
// host.docker.internal, which the host itself need not resolve.
func TestModelsURLReachesTheHostGatewayOnLoopback(t *testing.T) {
	for in, want := range map[string]string{
		"http://host.docker.internal:11434/v1": "http://127.0.0.1:11434/v1/models",
		"http://host.docker.internal/v1/":      "http://127.0.0.1/v1/models",
		"https://studio.lan:1234/api/v1/":      "https://studio.lan:1234/api/v1/models",
		"http://localhost:8080":                "http://localhost:8080/models",
	} {
		if got := modelsURL(in); got != want {
			t.Errorf("modelsURL(%q) = %q, want %q", in, got, want)
		}
	}
	s, _ := modelServer(t, 200, modelList)
	gateway := strings.Replace(s.URL, "127.0.0.1", "host.docker.internal", 1) + "/v1"
	m := &Models{}
	m.refresh(context.Background(), []config.Provider{{Name: "local", BaseURL: gateway}}, nil, discard())
	if got := m.Get(); len(got) != 1 || len(got[0].Models) != 2 {
		t.Errorf("models = %+v", got)
	}
}

// A slow provider never delays a poll: the report takes the last list
// while a fetch is still waiting.
func TestASlowProviderDoesNotDelayTheReport(t *testing.T) {
	asked, release := make(chan struct{}, 1), make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- struct{}{}
		<-release
		_, _ = w.Write([]byte(modelList))
	}))
	defer slow.Close()
	f := newFixture(t)
	last := []cinderline.ProviderModels{{Provider: "slow", Models: []string{"old"}}}
	f.p.Models = &Models{list: last}
	refreshed := make(chan struct{})
	go func() {
		f.p.Models.refresh(context.Background(), []config.Provider{{Name: "slow", BaseURL: slow.URL}}, nil, discard())
		close(refreshed)
	}()
	<-asked
	l := &loop{p: f.p, c: cinderline.New("http://x", "t", "0")}
	reported := make(chan []cinderline.ProviderModels)
	go func() { reported <- l.report().ProviderModels }()
	select {
	case got := <-reported:
		if !slices.EqualFunc(got, last, equalModels) {
			t.Errorf("report = %+v, want the last list", got)
		}
	case <-time.After(2 * time.Second):
		t.Error("the report waited for the provider")
	}
	close(release)
	<-refreshed
	if got := l.report().ProviderModels; len(got) != 1 || len(got[0].Models) != 2 {
		t.Errorf("after the fetch: %+v", got)
	}
}

// Without providers the report has an empty list, never null.
func TestNoProvidersReportNoModels(t *testing.T) {
	var m *Models
	if got := m.Get(); got == nil || len(got) != 0 {
		t.Errorf("models = %#v", got)
	}
}
