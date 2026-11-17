package run

import (
	"context"
	"encoding/json"
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
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// The fetch of the providers' models (decision 2026-11-17-provider-models).
const (
	modelsEvery   = 10 * time.Minute
	maxModelsBody = 4 << 20 // a longer answer is not read as a model list
)

// modelsTimeout is how long one provider may take to answer; a variable so tests need not wait.
var modelsTimeout = 5 * time.Second

// Models are the model ids of the runner's own providers as last fetched,
// shared by every project's report. A nil *Models has none.
type Models struct {
	mu   sync.Mutex
	list []cinderline.ProviderModels
}

// Get is the last fetched list, in the config's order of providers; never nil.
func (m *Models) Get() []cinderline.ProviderModels {
	if m == nil {
		return []cinderline.ProviderModels{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]cinderline.ProviderModels{}, m.list...)
}

// Watch fetches the providers' models at once and then every ten minutes,
// until ctx is done. A poll never waits for it: the report takes the last list.
func (m *Models) Watch(ctx context.Context, providers []config.Provider, sec *secrets.Secrets, log *slog.Logger) {
	keyed, err := modelProviders(providers, sec)
	if err != nil {
		log.Warn("provider keys not read, their models not fetched", "err", err)
		return
	}
	t := time.NewTicker(modelsEvery)
	defer t.Stop()
	for m.refresh(ctx, providers, keyed, log); ; m.refresh(ctx, providers, keyed, log) {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// refresh asks every provider at once and replaces the list. A provider
// that fails gets an empty list and a log line: never an error of the runner.
func (m *Models) refresh(ctx context.Context, providers []config.Provider, keyed map[string]client.Provider, log *slog.Logger) {
	list := make([]cinderline.ProviderModels, len(providers))
	var wg sync.WaitGroup
	for i, p := range providers {
		wg.Go(func() {
			ids, err := fetchModels(ctx, p.BaseURL, keyed[p.Name].Key)
			if err != nil && ctx.Err() == nil {
				log.Info("models of a provider not fetched", "provider", p.Name, "err", err)
			}
			list[i] = cinderline.ProviderModels{Provider: p.Name, Models: ids}
		})
	}
	wg.Wait()
	m.mu.Lock()
	m.list = list
	m.mu.Unlock()
}

// fetchModels is the ids of GET <base URL>/models, OpenAI's model list,
// asked with the provider's key when it has one; never nil.
func fetchModels(ctx context.Context, baseURL, key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL(baseURL), nil)
	if err != nil {
		return []string{}, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return []string{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return []string{}, fmt.Errorf("answered %s", resp.Status)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxModelsBody)).Decode(&list); err != nil {
		return []string{}, fmt.Errorf("not a model list: %w", err)
	}
	ids := make([]string, 0, len(list.Data))
	for _, d := range list.Data {
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
