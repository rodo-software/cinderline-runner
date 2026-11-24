package cinderline

import (
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
	maxRefusedHosts  = 20    // last.refused_hosts
	maxHostPort      = 260   // one refused host:port; a longer one is left out
	maxCheckName     = 255   // a failed check's name
)

// MaxChecks is the most failed checks of a review fact or a step.
const MaxChecks = 20

// modelID is a model id the app accepts (AgentClient::MODEL_ID); any other
// id is left out.
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

// checks is the first MaxChecks checks with a name, each text redacted and
// cut to its limit, a log to its end; nil stays nil. One without a name
// is left out: the app refuses it.
func checks(cs []FailedCheck) []FailedCheck {
	if cs == nil {
		return nil
	}
	out := make([]FailedCheck, 0, min(len(cs), MaxChecks))
	for _, c := range cs {
		if len(out) == MaxChecks {
			break
		}
		if c.Name = clip(redact.String(c.Name), maxCheckName); strings.TrimSpace(c.Name) == "" {
			continue
		}
		c.Description = clip(redact.String(c.Description), maxText)
		c.URL = clip(redact.String(c.URL), maxURL)
		c.Log = clipTail(redact.String(c.Log), maxTail)
		out = append(out, c)
	}
	return out
}
