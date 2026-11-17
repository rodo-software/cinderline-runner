package environment

import (
	"reflect"
	"testing"
)

// The host:port of a URL, for the allowlist: the scheme's port when the URL
// has none, the host in lower case, empty when it is not an http(s) URL.
func TestHostPort(t *testing.T) {
	for in, want := range map[string]string{
		"https://cs.example.com/mcp":       "cs.example.com:443",
		"http://cs.example.com":            "cs.example.com:80",
		"http://Cinderline.test:8000/api/": "cinderline.test:8000",
		"cs.example.com":                   "",
		"ftp://cs.example.com":             "",
		"https://":                         "",
	} {
		if got := HostPort(in); got != want {
			t.Errorf("HostPort(%q) = %q, want %q", in, got, want)
		}
	}
}

// Willem's decisions on CSR-63 (option A, then A for packagist.org): the
// package registries every run may reach, as a Composer, npm and Go install
// behind a logging proxy showed.
func TestRegistries(t *testing.T) {
	want := []string{
		"repo.packagist.org:443", "packagist.org:443", "codeload.github.com:443", "api.github.com:443",
		"registry.npmjs.org:443", "proxy.golang.org:443", "sum.golang.org:443",
	}
	if !reflect.DeepEqual(Registries, want) {
		t.Errorf("Registries = %q, want %q", Registries, want)
	}
}

// A project's shared hosts: Cinderline (for MCP), the registries, then the
// project's allow_hosts from local config. No forge: git runs in the helper
// container, off the run's network.
func TestProjectHosts(t *testing.T) {
	got := ProjectHosts("https://cs.example.com", []string{"mirror.example:443"})
	want := append(append([]string{"cs.example.com:443"}, Registries...), "mirror.example:443")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ProjectHosts = %q, want %q", got, want)
	}
}

// Decision 2026-11-17-local-model-providers point 3: a provider on the
// runner's own machine is reached as host.docker.internal on exactly its
// port; any other provider as it is configured.
func TestProviderURL(t *testing.T) {
	for in, want := range map[string][2]string{
		"http://studio.local:8080/v1":      {"http://studio.local:8080/v1", "studio.local:8080"},
		"https://LLM.example.com/v1":       {"https://LLM.example.com/v1", "llm.example.com:443"},
		"http://192.168.1.5:11434/v1":      {"http://192.168.1.5:11434/v1", "192.168.1.5:11434"},
		"http://localhost:11434/v1":        {"http://host.docker.internal:11434/v1", "host.docker.internal:11434"},
		"http://LocalHost:11434":           {"http://host.docker.internal:11434", "host.docker.internal:11434"},
		"http://127.0.0.1:8080/v1":         {"http://host.docker.internal:8080/v1", "host.docker.internal:8080"},
		"http://[::1]:8080/v1":             {"http://host.docker.internal:8080/v1", "host.docker.internal:8080"},
		"http://host.docker.internal:1234": {"http://host.docker.internal:1234", "host.docker.internal:1234"},
		"http://localhost/v1":              {"http://host.docker.internal:80/v1", "host.docker.internal:80"},
		"https://127.0.0.1/v1":             {"https://host.docker.internal:443/v1", "host.docker.internal:443"},
	} {
		if u, hp := ProviderURL(in); u != want[0] || hp != want[1] {
			t.Errorf("ProviderURL(%q) = %q, %q, want %q, %q", in, u, hp, want[0], want[1])
		}
	}
}
