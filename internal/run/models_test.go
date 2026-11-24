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

const openAIList = `{"object":"list","data":[{"id":"llama3.1:8b","object":"model"},{"id":"qwen/qwen3-coder"}]}`

// Decision 2026-11-17-provider-models: the runner's providers are asked for
// their models, with the provider's key when it has one, and reported in
// the config's order.
func TestWatchReportsEachProvidersModels(t *testing.T) {
	keyed, keyedAuth := modelServer(t, 200, openAIList)
	open, openAuth := modelServer(t, 200, `{"data":[{"id":"gemma"}]}`)
	f := newFixture(t)
	if _, err := f.p.Secrets.Set(secrets.Runner, secrets.ProviderKey("studio"), "sk-studio"); err != nil {
		t.Fatal(err)
	}
	providers := []config.Provider{{Name: "studio", BaseURL: keyed.URL + "/v1/"}, {Name: "lan", BaseURL: open.URL + "/v1"}}
	m := &Models{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Watch(ctx, providers, false, f.p.Secrets, discard()); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(m.Get(false)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	want := []cinderline.ProviderModels{{Provider: "studio", Models: []string{"llama3.1:8b", "qwen/qwen3-coder"}, ModelsAge: age(0)}, {Provider: "lan", Models: []string{"gemma"}, ModelsAge: age(0)}}
	if got := m.Get(false); !slices.EqualFunc(got, want, equalModels) {
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

// equalModels compares provider, models and whether there is an age.
func equalModels(a, b cinderline.ProviderModels) bool {
	return a.Provider == b.Provider && slices.Equal(a.Models, b.Models) && (a.ModelsAge == nil) == (b.ModelsAge == nil)
}

func age(n int) *int { return &n }

// sources are the model sources of the providers, as Watch makes them.
func sources(providers ...config.Provider) []modelSource {
	var s []modelSource
	for _, p := range providers {
		s = append(s, modelSource{provider: p.Name, url: modelsURL(p.BaseURL)})
	}
	return s
}

// A provider that answers with anything but a model list, or not in time,
// and never answered well has an empty list and no age; that is not an
// error of the runner's report.
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
	refused, _ := modelServer(t, 401, openAIList)
	other, _ := modelServer(t, 200, `{"models":["x"]}`)
	for name, url := range map[string]string{"timeout": slow.URL, "html": html.URL, "array": array.URL, "401": refused.URL, "other": other.URL, "down": "http://127.0.0.1:1"} {
		m := &Models{}
		m.refresh(context.Background(), sources(config.Provider{Name: "p", BaseURL: url + "/v1"}), discard())
		if got := m.Get(false); len(got) != 1 || got[0].Provider != "p" || got[0].Models == nil || len(got[0].Models) != 0 || got[0].ModelsAge != nil {
			t.Errorf("%s: models = %+v, want p with an empty list and no age", name, got)
		}
	}
}

// A provider on the runner's own machine may be configured as
// host.docker.internal, which the host itself need not resolve.
func TestModelsURLReachesTheHostGatewayOnLoopback(t *testing.T) {
	for in, want := range map[string]string{
		"http://host.docker.internal:11434/v1":  "http://127.0.0.1:11434/v1/models",
		"http://host.docker.internal/v1/":       "http://127.0.0.1/v1/models",
		"https://provider.example:8080/api/v1/": "https://provider.example:8080/api/v1/models",
		"http://localhost:8080":                 "http://localhost:8080/models",
	} {
		if got := modelsURL(in); got != want {
			t.Errorf("modelsURL(%q) = %q, want %q", in, got, want)
		}
	}
	s, _ := modelServer(t, 200, openAIList)
	gateway := strings.Replace(s.URL, "127.0.0.1", "host.docker.internal", 1) + "/v1"
	m := &Models{}
	m.refresh(context.Background(), sources(config.Provider{Name: "local", BaseURL: gateway}), discard())
	if got := m.Get(false); len(got) != 1 || len(got[0].Models) != 2 {
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
		_, _ = w.Write([]byte(openAIList))
	}))
	defer slow.Close()
	f := newFixture(t)
	last := []cinderline.ProviderModels{{Provider: "slow", Models: []string{"old"}, ModelsAge: age(0)}}
	f.p.Models = &Models{list: []modelList{{"slow", []string{"old"}, time.Now()}}}
	refreshed := make(chan struct{})
	go func() {
		f.p.Models.refresh(context.Background(), sources(config.Provider{Name: "slow", BaseURL: slow.URL}), discard())
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
	if got := m.Get(true); got == nil || len(got) != 0 {
		t.Errorf("models = %#v", got)
	}
}

// Decision 2026-11-24-model-lists: a provider that fails keeps its last good
// list, and its age grows from that fetch; a later good fetch replaces it.
func TestAFailedFetchKeepsTheLastGoodListAndItsAge(t *testing.T) {
	var mu sync.Mutex
	status := 200
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(openAIList))
	}))
	defer s.Close()
	src := sources(config.Provider{Name: "studio", BaseURL: s.URL})
	m := &Models{}
	m.refresh(context.Background(), src, discard())
	m.list[0].at = time.Now().Add(-25 * time.Minute) // fetched 25 minutes ago
	mu.Lock()
	status = 503
	mu.Unlock()
	m.refresh(context.Background(), src, discard())
	got := m.Get(false)
	if len(got) != 1 || len(got[0].Models) != 2 || got[0].ModelsAge == nil || *got[0].ModelsAge < 25*60 || *got[0].ModelsAge > 25*60+5 {
		t.Fatalf("after a failed fetch: %+v, want the last list about 1500 s old", got)
	}
	mu.Lock()
	status = 200
	mu.Unlock()
	m.refresh(context.Background(), src, discard())
	if got := m.Get(false); *got[0].ModelsAge > 5 {
		t.Errorf("after a good fetch the age is %d", *got[0].ModelsAge)
	}
}

// Decision 2026-11-24-model-lists: OpenRouter's models that take tools come
// first, asked without a key, and only a project with a usable OpenRouter
// key reports them.
func TestOpenRouterModelsGoToProjectsWithItsKey(t *testing.T) {
	var mu sync.Mutex
	var query, auth string
	or := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		query, auth = r.URL.RawQuery, r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = w.Write([]byte(`{"data":[{"id":"z-ai/glm-5.2"},{"id":"deepseek/deepseek-v4-flash:free"}]}`))
	}))
	defer or.Close()
	studio, _ := modelServer(t, 200, openAIList)
	defer func(u string) { openRouterModels = u }(openRouterModels)
	openRouterModels = or.URL + "/api/v1/models?supported_parameters=tools"
	f := newFixture(t)
	m := &Models{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Watch(ctx, []config.Provider{{Name: "studio", BaseURL: studio.URL + "/v1"}}, true, f.p.Secrets, discard())
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(m.Get(true)) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	want := []cinderline.ProviderModels{
		{Provider: "openrouter", Models: []string{"z-ai/glm-5.2", "deepseek/deepseek-v4-flash:free"}, ModelsAge: age(0)},
		{Provider: "studio", Models: []string{"llama3.1:8b", "qwen/qwen3-coder"}, ModelsAge: age(0)},
	}
	if got := m.Get(true); !slices.EqualFunc(got, want, equalModels) {
		t.Errorf("with the key: %+v", got)
	}
	if got := m.Get(false); !slices.EqualFunc(got, want[1:], equalModels) {
		t.Errorf("without the key: %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if query != "supported_parameters=tools" || auth != "" {
		t.Errorf("asked with query %q and authorization %q", query, auth)
	}
	f.p.Models = m
	f.p.openRouter = true
	l := &loop{p: f.p, c: cinderline.New("http://x", "t", "0")}
	if got := l.report().ProviderModels; len(got) != 2 || got[0].Provider != "openrouter" {
		t.Errorf("report provider_models = %+v", got)
	}
}

// Decision 2026-11-24-model-lists: each ready client but OpenCode lists its
// models; the report sends each list with its age, and a client that fails
// keeps its last good list while its age grows.
func TestClientsReportTheirLastGoodModelList(t *testing.T) {
	f := newFixture(t)
	f.p.clients = []cinderline.ReportClient{{Name: "claude", State: "ready"}, {Name: "codex", State: "ready"}, {Name: "gemini", State: "missing_credential"}, {Name: "opencode", State: "ready"}}
	var asked []string
	answer := map[string][]string{"claude": {"claude-opus-5-5", "claude-sonnet-5-5"}, "codex": {}}
	f.p.listModels = func(_ context.Context, names []string) map[string][]string {
		asked = names
		return answer
	}
	l := &loop{p: f.p, c: cinderline.New("http://x", "t", "0")}
	if got := l.report().Clients; got[0].Models != nil || got[0].ModelsAge != nil {
		t.Errorf("before a list: %+v", got[0])
	}
	f.p.ListModels(context.Background())
	if !slices.Equal(asked, []string{"claude", "codex"}) {
		t.Errorf("asked %q, want the ready clients but opencode", asked)
	}
	got := l.report().Clients
	if !slices.Equal(got[0].Models, answer["claude"]) || got[0].ModelsAge == nil || *got[0].ModelsAge != 0 {
		t.Errorf("claude: %+v", got[0])
	}
	if got[1].Models == nil || len(got[1].Models) != 0 || got[1].ModelsAge == nil {
		t.Errorf("codex found none: %+v, want an empty list with an age", got[1])
	}
	if got[2].Models != nil || got[3].Models != nil || got[3].ModelsAge != nil {
		t.Errorf("gemini or opencode got a list: %+v %+v", got[2], got[3])
	}

	f.p.mu.Lock()
	f.p.models["claude"] = modelList{"claude", answer["claude"], time.Now().Add(-15 * time.Minute)}
	f.p.mu.Unlock()
	answer = map[string][]string{} // every client fails
	f.p.ListModels(context.Background())
	if got := l.report().Clients[0]; !slices.Equal(got.Models, []string{"claude-opus-5-5", "claude-sonnet-5-5"}) || *got.ModelsAge < 15*60 {
		t.Errorf("after a failed list: %+v, want the last list 15 minutes old", got)
	}
}
