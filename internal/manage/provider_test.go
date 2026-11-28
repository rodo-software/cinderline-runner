package manage

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

const providerKey = "sk-provider-secret"

var lan = config.Provider{Name: "lan", BaseURL: "http://provider.example:8080/v1"}

// providerSetup is a config.json with web and the given providers, and its secrets.
func providerSetup(t *testing.T, ps ...config.Provider) (config.Paths, *secrets.Secrets) {
	t.Helper()
	keyring.MockInit()
	p := paths(t)
	p.State = p.Dir
	if err := (&config.Config{Projects: []config.Project{web}, Providers: ps}).Save(p.File); err != nil {
		t.Fatal(err)
	}
	return p, secrets.New(p.State)
}

func providersOf(t *testing.T, p config.Paths) []config.Provider {
	t.Helper()
	cfg, err := config.Load(p.File)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Providers
}

func keyOf(t *testing.T, sec *secrets.Secrets, name string) string {
	t.Helper()
	v, _, err := sec.Get(secrets.Runner, secrets.ProviderKey(name))
	if err != nil && !errors.Is(err, secrets.ErrNotFound) {
		t.Fatal(err)
	}
	return v
}

// CLR-299: AddProvider stores the provider and its key, runner-wide;
// RemoveProvider removes both.
func TestAddAndRemoveProvider(t *testing.T) {
	p, sec := providerSetup(t)
	store, err := AddProvider(p, sec, lan, providerKey)
	if err != nil || store == "" {
		t.Fatalf("AddProvider = %q, %v", store, err)
	}
	if got := providersOf(t, p); !slices.Equal(got, []config.Provider{lan}) {
		t.Fatalf("providers %v", got)
	}
	if v := keyOf(t, sec, "lan"); v != providerKey {
		t.Fatalf("key %q", v)
	}
	if err := RemoveProvider(p, sec, "lan"); err != nil {
		t.Fatal(err)
	}
	if got := providersOf(t, p); len(got) != 0 || keyOf(t, sec, "lan") != "" {
		t.Errorf("after remove: providers %v, key %q", got, keyOf(t, sec, "lan"))
	}
}

// An empty key stores none and deletes one left under the name, so it
// never goes to the new URL.
func TestAddProviderWithoutKeyDeletesAnOldOne(t *testing.T) {
	p, sec := providerSetup(t)
	if _, err := sec.Set(secrets.Runner, secrets.ProviderKey("lan"), "old-key"); err != nil {
		t.Fatal(err)
	}
	if store, err := AddProvider(p, sec, lan, ""); err != nil || store != "" {
		t.Fatalf("AddProvider = %q, %v", store, err)
	}
	if v := keyOf(t, sec, "lan"); v != "" {
		t.Errorf("old key %q left", v)
	}
}

// Each refusal is Refused, writes nothing and holds no key.
func TestAddProviderRefuses(t *testing.T) {
	for what, c := range map[string]struct {
		p    config.Provider
		want string
	}{
		"bad name":            {config.Provider{Name: "Lan", BaseURL: lan.BaseURL}, "provider name"},
		"openrouter":          {config.Provider{Name: "openrouter", BaseURL: lan.BaseURL}, "not openrouter"},
		"user info":           {config.Provider{Name: "x", BaseURL: "http://u:p@h:1/v1"}, "base URL"},
		"max_agents":          {config.Provider{Name: "x", BaseURL: lan.BaseURL, MaxAgents: 101}, "max_agents"},
		"lookahead alone":     {config.Provider{Name: "x", BaseURL: lan.BaseURL, Lookahead: 1}, "needs max_agents"},
		"duplicate":           {config.Provider{Name: "lan", BaseURL: "http://other:1/v1"}, "already has a provider lan"},
		"the web page's port": {config.Provider{Name: "x", BaseURL: "http://localhost:7420/v1"}, "web page's port"},
	} {
		p, sec := providerSetup(t, lan)
		_, err := AddProvider(p, sec, c.p, providerKey)
		if !errors.As(err, new(Refused)) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want Refused containing %q", what, err, c.want)
			continue
		}
		if strings.Contains(err.Error(), providerKey) {
			t.Errorf("%s: the error holds the key: %v", what, err)
		}
		if got := providersOf(t, p); !slices.Equal(got, []config.Provider{lan}) || keyOf(t, sec, c.p.Name) != "" {
			t.Errorf("%s: providers %v, key %q; want nothing written", what, got, keyOf(t, sec, c.p.Name))
		}
	}
}

// SetProvider changes only what it is given, keeps the key, and says
// whether anything changed.
func TestSetProvider(t *testing.T) {
	p, sec := providerSetup(t, lan)
	if _, err := sec.Set(secrets.Runner, secrets.ProviderKey("lan"), providerKey); err != nil {
		t.Fatal(err)
	}
	yes, two, one := true, 2, 1
	if changed, err := SetProvider(p, "lan", &yes, &two, nil); err != nil || !changed {
		t.Fatalf("SetProvider = %v, %v", changed, err)
	}
	want := config.Provider{Name: "lan", BaseURL: lan.BaseURL, Free: true, MaxAgents: 2}
	if got := providersOf(t, p); !slices.Equal(got, []config.Provider{want}) {
		t.Fatalf("providers %v, want %v", got, want)
	}
	if changed, err := SetProvider(p, "lan", nil, nil, &one); err != nil || !changed {
		t.Fatalf("SetProvider lookahead = %v, %v", changed, err)
	}
	want.Lookahead = 1
	if got := providersOf(t, p); !slices.Equal(got, []config.Provider{want}) {
		t.Fatalf("providers %v, want %v", got, want)
	}
	if changed, err := SetProvider(p, "lan", &yes, nil, nil); err != nil || changed {
		t.Errorf("SetProvider to the same = %v, %v; want unchanged", changed, err)
	}
	if v := keyOf(t, sec, "lan"); v != providerKey {
		t.Errorf("key %q, want it kept", v)
	}
}

func TestSetProviderRefuses(t *testing.T) {
	zero, big := 0, 101
	for what, c := range map[string]struct {
		name                 string
		maxAgents, lookahead *int
		want                 string
	}{
		"nothing to set":    {"lan", nil, nil, "nothing to set"},
		"no such provider":  {"nope", &zero, nil, "no provider nope"},
		"max_agents":        {"lan", &big, nil, "max_agents"},
		"lookahead too big": {"lan", nil, &big, "lookahead"},
		"lookahead alone":   {"lan", &zero, nil, "needs max_agents"}, // lan has a lookahead
	} {
		withLookahead := lan
		withLookahead.MaxAgents, withLookahead.Lookahead = 1, 1
		p, _ := providerSetup(t, withLookahead)
		changed, err := SetProvider(p, c.name, nil, c.maxAgents, c.lookahead)
		if changed || !errors.As(err, new(Refused)) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, %v; want Refused containing %q", what, changed, err, c.want)
		}
		if got := providersOf(t, p); !slices.Equal(got, []config.Provider{withLookahead}) {
			t.Errorf("%s: providers %v; want them unchanged", what, got)
		}
	}
}

func TestRemoveProviderRefusesAnUnknownOne(t *testing.T) {
	p, sec := providerSetup(t, lan)
	if err := RemoveProvider(p, sec, "nope"); !errors.As(err, new(Refused)) || !strings.Contains(err.Error(), "no provider nope") {
		t.Errorf("RemoveProvider = %v, want Refused", err)
	}
	if got := providersOf(t, p); len(got) != 1 {
		t.Errorf("providers %v", got)
	}
}
