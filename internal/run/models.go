package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// The fetch of the providers' models (decisions 2026-11-17-provider-models
// and 2026-11-24-model-lists).
const (
	ModelsEvery   = 10 * time.Minute // also each client's list (Project.ListModels)
	maxModelsBody = 4 << 20          // a longer answer is not read as a model list
)

// modelsTimeout is how long one provider may take to answer; a variable so tests need not wait.
var modelsTimeout = 5 * time.Second

// openRouterModels is OpenRouter's public list of the models that take
// tools; a variable so tests can serve it.
var openRouterModels = "https://openrouter.ai/api/v1/models?supported_parameters=tools"

// Models are the model ids of the runner's providers, openrouter's too, as
// last fetched well, shared by every project's report. A nil *Models has none.
type Models struct {
	mu   sync.Mutex
	list []modelList // openrouter first when fetched, then the config's order
}

// modelList is the last good fetch of one provider's or client's models;
// ids nil and at zero: none yet.
type modelList struct {
	name string   // the provider or client
	ids  []string // never changed once stored
	at   time.Time
}

// Get is the last good lists with their age, openrouter's only for a
// project whose OpenRouter key is usable; never nil.
func (m *Models) Get(openRouter bool) []cinderline.ProviderModels {
	out := []cinderline.ProviderModels{}
	if m == nil {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.list {
		if l.name == "openrouter" && !openRouter {
			continue
		}
		pm := cinderline.ProviderModels{Provider: l.name, Models: l.ids}
		if l.ids == nil {
			pm.Models = []string{}
		} else {
			pm.ModelsAge = ageOf(l.at)
		}
		out = append(out, pm)
	}
	return out
}

// ageOf is the whole seconds since at.
func ageOf(at time.Time) *int {
	age := int(max(0, time.Since(at)/time.Second))
	return &age
}

// modelSource is where one provider's models are asked for.
type modelSource struct{ provider, url, key string }

// Watch fetches the providers' models at once and then every ten minutes,
// until ctx is done, with OpenRouter's when a project has its key. A poll
// never waits for it: the report takes the last lists.
func (m *Models) Watch(ctx context.Context, providers []config.Provider, openRouter bool, sec *secrets.Secrets, log *slog.Logger) {
	keyed, err := modelProviders(providers, sec)
	if err != nil {
		log.Warn("provider keys not read, their models not fetched", "err", err)
		return
	}
	var sources []modelSource
	if openRouter {
		sources = append(sources, modelSource{provider: "openrouter", url: openRouterModels})
	}
	for _, p := range providers {
		sources = append(sources, modelSource{p.Name, modelsURL(p.BaseURL), keyed[p.Name].Key})
	}
	t := time.NewTicker(ModelsEvery)
	defer t.Stop()
	for m.refresh(ctx, sources, log); ; m.refresh(ctx, sources, log) {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// refresh asks every provider at once. A provider that answers gets its new
// list; one that fails keeps its last good list, whose age grows, and a log
// line: never an error of the runner.
func (m *Models) refresh(ctx context.Context, sources []modelSource, log *slog.Logger) {
	got := make([]modelList, len(sources))
	var wg sync.WaitGroup
	for i, s := range sources {
		wg.Go(func() {
			ids, err := fetchModels(ctx, s.url, s.key)
			if err != nil {
				if ctx.Err() == nil {
					log.Info("models of a provider not fetched", "provider", s.provider, "err", err)
				}
				return
			}
			got[i] = modelList{s.provider, ids, time.Now()}
		})
	}
	wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	last := map[string]modelList{}
	for _, l := range m.list {
		last[l.name] = l
	}
	for i, s := range sources {
		if got[i].ids == nil {
			got[i] = last[s.provider]
			got[i].name = s.provider
		}
	}
	m.list = got
}

// fetchModels is the ids of an OpenAI-style model list at url, asked with
// the provider's key when it has one; never nil without an error.
func fetchModels(ctx context.Context, url, key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("answered %s", resp.Status)
	}
	var list struct {
		Data *[]struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxModelsBody)).Decode(&list); err != nil {
		return nil, fmt.Errorf("not a model list: %w", err)
	}
	if list.Data == nil {
		return nil, errors.New("not a model list: no data")
	}
	ids := make([]string, 0, len(*list.Data))
	for _, d := range *list.Data {
		ids = append(ids, d.ID) // the report leaves out ids the app refuses
	}
	return ids, nil
}

// modelsURL is the provider's models endpoint as the runner reaches it:
// from its own machine, where host.docker.internal need not resolve.
func modelsURL(base string) string {
	u, err := url.Parse(strings.TrimRight(base, "/") + "/models")
	if err != nil || u.Hostname() != environment.HostGateway {
		return strings.TrimRight(base, "/") + "/models"
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort("127.0.0.1", port)
	} else {
		u.Host = "127.0.0.1"
	}
	return u.String()
}
