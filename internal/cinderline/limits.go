package cinderline

import (
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/rodo-software/cinderline-runner/internal/redact"
)

// The app's limits on what the runner sends (app/Rules/RunnerRules.php in
// the app repository). A request over one is refused for good, so the
// runner cuts texts and lists to them before sending.
const (
	maxRunning       = 20    // poll running
	maxListed        = 200   // poll held and review
	maxErrors        = 50    // poll report.errors
	MaxErrorMessage  = 2000  // poll report.errors[].message
	maxConflicts     = 1000  // conflicts of a review fact or a step
	maxText          = 1000  // one conflict or workspace note
	maxNotes         = 20    // workspace.notes
	maxWorkspaceList = 50    // workspace.extra_repositories and linked_projects
	maxStepError     = 16000 // last.error
	maxLongText      = 65000 // last.report and last.commits
	maxTail          = 4000  // last.tail, its end
	maxURL           = 2000  // last.pull_request
	maxCommit        = 100   // last.commit
	maxName          = 100   // runner_version, a client's name and version, an agent, a model
	maxRef           = 50    // an issue ref in poll; a longer one names no issue and is left out
	maxIssue         = 30    // an issue ref in next
	maxKey           = 30    // a linked project's key
	maxClients       = 50    // report.clients
	maxEnvironments  = 10    // report.environments
	maxProviders     = 50    // report.model_providers and report.free_providers
	maxProviderName  = 50    // a model provider's name; a longer one is left out
	maxModels        = 500   // report.provider_models[].models and report.clients[].models
	maxClientModel   = 100   // a client's model id: a team slot's model; a longer one is left out
	maxProviderModel = 150   // a provider's model id; a longer one is left out
	maxSessions      = 2     // sessions
	maxPath          = 255   // workspace branch, base and paths
	maxSecrets       = 50    // last.secrets
	maxHostPort      = 260   // one refused host:port; a longer one is left out
	maxMethod        = 20    // a refused request's method; a longer one is left out with its URL
	maxRefusedURL    = 2000  // a refused request's URL; a longer one is left out with its method
	maxCheckName     = 255   // a failed check's name
	maxTools         = 200   // last.usage.tools
	maxToolCalls     = 1e6   // one tool's calls in last.usage.tools
	maxReason        = 2000  // last.reason and a review fact's reason
	maxSkills        = 10    // workspace.skills
	maxSkillName     = 64    // a skill package's name in workspace.skills
)

// MaxChecks is the most failed checks of a review fact or a step.
const MaxChecks = 20

// MaxRefused is the most refused hosts of a step (last.refused).
const MaxRefused = 20

// modelID is a model id the app accepts (AgentClient::MODEL_ID); any other
// id is left out.
// aURL is a URL in a text: a scheme, ://, and what follows up to a space,
// a quote or a bracket.
var aURL = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>()\[\]{}]*`)

// noURLs is s with every URL replaced by "<URL>".
func noURLs(s string) string { return aURL.ReplaceAllString(s, "<URL>") }

var modelID = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,150}$`)

// modelIDs is the first maxModels ids of s the app accepts, of at most chars
// characters; nil stays nil, as no list.
func modelIDs(s []string, chars int) []string {
	if s == nil {
		return nil
	}
	out := []string{}
	for _, id := range s {
		if len(out) < maxModels && len(id) <= chars && modelID.MatchString(id) {
			out = append(out, id)
		}
	}
	return out
}

// clip is s cut to its first n characters, as the app counts them (runes).
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// clipTail is s cut to its last n characters.
func clipTail(s string, n int) string {
	r := []rune(s)
	return string(r[max(0, len(r)-n):])
}

// first is the first n entries of s.
func first[T any](s []T, n int) []T { return s[:min(len(s), n)] }

// refs is the first n entries of s that fit maxRef: a clipped ref would
// name another issue.
func refs(s []string, n int) []string {
	if s == nil {
		return nil
	}
	return fitting(s, n, maxRef)
}

// fitting is the first n entries of s of at most chars characters; never nil.
func fitting(s []string, n, chars int) []string {
	out := []string{}
	for _, r := range s {
		if len(out) < n && utf8.RuneCountInString(r) <= chars {
			out = append(out, r)
		}
	}
	return out
}

// clipAll is the first n entries of s, each clipped to chars; nil stays nil.
func clipAll(s []string, n, chars int) []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, min(len(s), n))
	for _, v := range first(s, n) {
		out = append(out, clip(v, chars))
	}
	return out
}

// checks is the first MaxChecks checks with a name, each text redacted,
// without URLs and cut to its limit, a log to its end; nil stays nil. One
// without a name is left out: the app refuses it. Only URL keeps its URL:
// the agent never gets a forge's URL (CL-375), and the app drops that field
// from what the agent sees.
func checks(cs []FailedCheck) []FailedCheck {
	if cs == nil {
		return nil
	}
	out := make([]FailedCheck, 0, min(len(cs), MaxChecks))
	for _, c := range cs {
		if len(out) == MaxChecks {
			break
		}
		if c.Name = clip(redact.String(noURLs(c.Name)), maxCheckName); strings.TrimSpace(c.Name) == "" {
			continue
		}
		c.Description = clip(redact.String(noURLs(c.Description)), maxText)
		c.URL = clip(redact.String(c.URL), maxURL)
		c.Log = clipTail(redact.String(noURLs(c.Log)), maxTail)
		out = append(out, c)
	}
	return out
}

// reason is the reason of a step or a review fact to send: the forge's own
// words, redacted, without URLs, cut to its limit; nil or nothing but
// spaces is nil, as no reason (CLR-245).
func reason(s *string) *string {
	if s == nil {
		return nil
	}
	v := clip(redact.String(noURLs(*s)), maxReason)
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return &v
}

// refused is the first MaxRefused refused hosts that fit the app's limits,
// with a known reason; nil stays nil. A host that is too long or holds a
// secret is left out, not redacted or clipped, so each stays a host:port.
// So is a method and URL that do not fit, hold a secret, or still have a
// query, fragment or user info, which the proxy already removed: the host
// stays, without them.
func refused(rs []RefusedHost) []RefusedHost {
	if rs == nil {
		return nil
	}
	out := make([]RefusedHost, 0, min(len(rs), MaxRefused))
	for _, r := range rs {
		if len(out) == MaxRefused {
			break
		}
		switch r.Reason {
		case RefusedNotAllowed, RefusedPrivateAddress, RefusedStrict, RefusedRunnerMachine:
		default:
			continue
		}
		if utf8.RuneCountInString(r.Host) > maxHostPort || redact.String(r.Host) != r.Host {
			continue
		}
		if r.Method == nil || r.URL == nil || !cleanRequest(*r.Method, *r.URL) {
			r.Method, r.URL = nil, nil
		}
		out = append(out, r)
	}
	return out
}

// cleanRequest says a refused request's method and URL may be sent as they
// are.
func cleanRequest(method, u string) bool {
	p, err := url.Parse(u)
	return method != "" && len(method) <= maxMethod && redact.String(method) == method &&
		utf8.RuneCountInString(u) <= maxRefusedURL && redact.String(u) == u &&
		err == nil && (p.Scheme == "http" || p.Scheme == "https") && p.User == nil && p.RawQuery == "" && !p.ForceQuery && p.Fragment == ""
}
