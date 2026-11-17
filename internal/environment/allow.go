package environment

import (
	"net"
	"net/url"
	"strings"
)

// Registries are the package registries every run may reach (decision
// 2026-11-09-network-allowlist): Composer, npm and Go, as an install behind
// a logging proxy showed.
var Registries = []string{
	"repo.packagist.org:443", "packagist.org:443", "codeload.github.com:443", "api.github.com:443",
	"registry.npmjs.org:443", "proxy.golang.org:443", "sum.golang.org:443",
}

// HostPort is the host:port of an http(s) URL for the allowlist, the host
// in lower case and the scheme's port when the URL has none; empty for
// anything else.
func HostPort(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	switch {
	case port != "":
	case u.Scheme == "https":
		port = "443"
	case u.Scheme == "http":
		port = "80"
	}
	if port == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// ProjectHosts are the hosts every run of a project may reach besides its
// client's: Cinderline for MCP, the registries, then the project's
// allow_hosts. No forge: git runs in the helper container, off the run's
// network.
func ProjectHosts(cinderlineURL string, allowHosts []string) []string {
	var hosts []string
	if hp := HostPort(cinderlineURL); hp != "" {
		hosts = append(hosts, hp)
	}
	return append(append(hosts, Registries...), allowHosts...)
}

// HostGateway is the name of the runner's own machine in the proxy
// container, and only there (decision 2026-11-17-local-model-providers).
const HostGateway = "host.docker.internal"

// ProviderURL is a model provider's base URL as the agent reaches it, and
// its host:port for the allowlist. A provider on the runner's own machine
// (localhost, 127.0.0.1, ::1 or host.docker.internal) becomes HostGateway
// on its port; config has checked the URL.
func ProviderURL(rawURL string) (string, string) {
	hp := HostPort(rawURL)
	host, port, _ := net.SplitHostPort(hp)
	switch host {
	case "localhost", "127.0.0.1", "::1", HostGateway:
	default:
		return rawURL, hp
	}
	u, _ := url.Parse(rawURL)
	u.Host = net.JoinHostPort(HostGateway, port)
	return u.String(), u.Host
}
