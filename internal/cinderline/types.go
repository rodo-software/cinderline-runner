package cinderline

import "github.com/rodo-software/cinderline-runner/internal/config"

// PollRequest is the body of POST /poll: the runner's report and what it
// holds now.
type PollRequest struct {
	Report  Report   `json:"report"`
	Running []string `json:"running"` // never null
	Idle    bool     `json:"idle"`
	// ManagerIdle says the runner's Project Manager slot is free. Only a
	// runner with the slot sends it, false too, as sending the field claims
	// it (decision 2026-11-17-project-manager).
	ManagerIdle bool `json:"manager_idle"`
	// MaxParallel is the most team runs at once; only a runner with more
	// than one team slot sends it, and then gets Jobs (decision
	// 2026-11-24-lanes in the app repository).
	MaxParallel int `json:"max_parallel,omitempty"`
	// Held, when set, asks what to do with these issues' sessions and clones.
	Held []string `json:"held,omitzero"`
	// Review, when not nil, asks for the Review list; an empty list asks too.
	Review []ReviewFact `json:"review,omitzero"`
}

// Report is what the runner tells the app about itself, always whole.
type Report struct {
	RunnerVersion      string         `json:"runner_version"`
	Clients            []ReportClient `json:"clients"`
	Environments       []string       `json:"environments"`
	Errors             []ReportError  `json:"errors"`
	RefusedConnections int            `json:"refused_connections"`
	// ModelProviders are the providers OpenCode agents can use here:
	// openrouter, then the runner's own (decision 2026-11-17-local-model-providers).
	ModelProviders []string `json:"model_providers"`
	// ProviderModels are the model ids each of the runner's providers served
	// at its last good fetch, openrouter's first when the project's key is
	// set (decisions 2026-11-17-provider-models, 2026-11-24-model-lists).
	ProviderModels []ProviderModels `json:"provider_models"`
	// FreeProviders are the runner's own providers marked free, never
	// openrouter, each also in ModelProviders; left out when none is
	// (decision 2026-11-19-free-model-providers).
	FreeProviders []string `json:"free_providers,omitempty"`
	// StrictHosts says the runner ignores the allow_hosts of a step, so the
	// app does not ask a person to open a host (decision 2026-11-17-hosts-from-cinderline).
	StrictHosts bool `json:"strict_hosts"`
}

// ProviderModels is the model ids one provider served at its last good
// fetch; empty, with no age, when there was none.
type ProviderModels struct {
	Provider string   `json:"provider"`
	Models   []string `json:"models"`
	// ModelsAge is the seconds since Models was fetched; nil: never.
	ModelsAge *int `json:"models_age,omitempty"`
}

// ReportClient is the state of one agent CLI on this machine: ready,
// missing_binary, missing_credential or invalid_credential.
type ReportClient struct {
	Name    string  `json:"name"`
	Version *string `json:"version"`
	State   string  `json:"state"`
	// Models are the models the client can run with its credential, its
	// default first, from its last good fetch; nil: no list yet, and never
	// for opencode (decision 2026-11-24-model-lists). ModelsAge is the
	// seconds since that fetch, set with Models.
	Models    []string `json:"models,omitzero"`
	ModelsAge *int     `json:"models_age,omitempty"`
}

// ReportError is one error the runner holds.
type ReportError struct {
	Kind    string `json:"kind"`
	Issue   string `json:"issue,omitempty"`
	Message string `json:"message"`
}

// The kinds of ReportError.
const (
	KindRepositoryMismatch = "repository_mismatch" // no issue
	KindNotStarted         = "not_started"         // with the issue
)

// ReviewFact is what the last upkeep pass found of one Review issue.
// PullRequest is open, merged, closed, failed or none.
type ReviewFact struct {
	Ref         string   `json:"ref"`
	PullRequest string   `json:"pull_request"`
	Conflicts   []string `json:"conflicts"`
	// Checks are, with failed, the CI checks that failed on the head.
	Checks []FailedCheck `json:"checks,omitempty"`
}

// FailedCheck is a CI check that failed on a pull request's head. Its
// texts are the forge's and the CI's, untrusted; Log is the end of its log,
// "" when the runner could not read one.
type FailedCheck struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`
	Log         string `json:"log,omitempty"`
}

// PollAnswer is the app's answer to a poll.
type PollAnswer struct {
	PollAfter   int     `json:"poll_after"`   // seconds
	ReviewAfter int     `json:"review_after"` // seconds
	Project     Project `json:"project"`
	Job         *Job    `json:"job"` // nil: no work
	// ManagerJob is the job for the runner's Project Manager slot, nil:
	// none. Only when the poll said ManagerIdle, null unless it was true
	// (decision 2026-11-17-project-manager).
	ManagerJob *Job `json:"manager_job"`
	// Jobs are the jobs for free team slots, one per lane, when the poll
	// had MaxParallel; nil: the app sent none, as one before lanes does.
	Jobs []Job    `json:"jobs"`
	Stop []string `json:"stop"`
	// Held and Review are nil when the poll did not ask for them.
	Held   []HeldItem   `json:"held"`
	Review []ReviewItem `json:"review"`
}

// Project is the project's settings, whole in every poll answer. Empty
// strings stand for the app's nulls.
type Project struct {
	Key            string          `json:"key"`
	Name           string          `json:"name"`
	Workspace      string          `json:"workspace"`
	RepositoryURL  string          `json:"repository_url"`
	Forge          string          `json:"forge"`       // "": tell it from the repository's host
	BaseBranch     string          `json:"base_branch"` // "": the forge's default branch
	Environment    string          `json:"environment"`
	ComposeFile    string          `json:"compose_file"`
	AgentService   string          `json:"agent_service"`
	CheckCommand   string          `json:"check_command"` // run through sh -c in the clone
	IdleMinutes    int             `json:"idle_minutes"`
	LinkedProjects []LinkedProject `json:"linked_projects"`
}

// WorkspaceSlug is the project's workspace, config.DefaultWorkspace when the app names none.
func (p Project) WorkspaceSlug() string {
	if p.Workspace == "" {
		return config.DefaultWorkspace
	}
	return p.Workspace
}

// LinkedProject is a project this project's agents may read.
type LinkedProject struct {
	Key           string `json:"key"`
	Name          string `json:"name"`
	RepositoryURL string `json:"repository_url"`
	BaseBranch    string `json:"base_branch"`
}

// Job is an issue the app offers to run; nothing is claimed yet.
type Job struct {
	Issue JobIssue `json:"issue"`
	Team  Team     `json:"team"`
}

// JobIssue is the offered issue. Body is untrusted text.
type JobIssue struct {
	Ref   string `json:"ref"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Team is the job's team; Agents lists the lead first.
type Team struct {
	Name       string      `json:"name"`
	MaxMinutes int         `json:"max_minutes"`
	MaxTurns   int         `json:"max_turns"`
	Ponytail   bool        `json:"ponytail"`
	Agents     []TeamAgent `json:"agents"`
}

// TeamAgent is one agent of a team.
type TeamAgent struct {
	Name   string  `json:"name"`
	Client string  `json:"client"`
	Model  *string `json:"model"`
	Effort *string `json:"effort"`
}

// HeldItem says what to do with a held issue's session and clone: keep,
// expire (by the runner's own limits) or drop.
type HeldItem struct {
	Ref     string `json:"ref"`
	Session string `json:"session"`
}

// ReviewItem is a Review issue whose branch the runner keeps current. Merge
// says it may ask the forge to merge the pull request once it is current.
type ReviewItem struct {
	Ref   string `json:"ref"`
	Title string `json:"title"`
	Merge bool   `json:"merge"`
}

// NextRequest is the body of POST /runs/{ref}/next.
type NextRequest struct {
	ID        string       `json:"id"`    // new per request, the same on a retry
	State     *string      `json:"state"` // the last answer's; nil on the first call
	Last      *LastStep    `json:"last"`
	Sessions  []Session    `json:"sessions"` // never null
	Workspace RunWorkspace `json:"workspace"`
}

// Session is a client session the runner can resume now. Origin is paused,
// kept or turn.
type Session struct {
	Issue  string `json:"issue"`
	Agent  string `json:"agent"`
	Origin string `json:"origin"`
}

// RunWorkspace is the run's clone and mounts, and the lines about what the
// runner could not prepare.
type RunWorkspace struct {
	Branch            string                `json:"branch"`
	Base              string                `json:"base"`
	ExtraRepositories []WorkspaceRepository `json:"extra_repositories"`
	LinkedProjects    []WorkspaceLink       `json:"linked_projects"`
	Notes             []string              `json:"notes"`
}

// WorkspaceRepository is an extra repository the run fetched; URL has no user info.
type WorkspaceRepository struct {
	Path string `json:"path"`
	URL  string `json:"url"`
}

// WorkspaceLink is a linked project with the folders under /linked the run fetched.
type WorkspaceLink struct {
	Key   string   `json:"key"`
	Paths []string `json:"paths"`
}

// LastStep is the step the runner just did and how it ended. Report, Tail,
// Error, Conflicts and Commits are untrusted.
type LastStep struct {
	Step        string   `json:"step"` // run, resolve, check, merge, publish or stop
	Issue       string   `json:"issue"`
	Agent       *string  `json:"agent"`
	Outcome     string   `json:"outcome"`
	ResetsAt    *string  `json:"resets_at,omitempty"`    // usage_limit only, UTC; nil when unknown
	MemoryLimit *int64   `json:"memory_limit,omitempty"` // out_of_memory only: the killed container's limit, in bytes
	Error       *string  `json:"error"`
	Report      *string  `json:"report"`
	Tail        *string  `json:"tail"`
	Usage       *Usage   `json:"usage"`
	Conflicts   []string `json:"conflicts"`
	Commits     *string  `json:"commits"`
	PullRequest *string  `json:"pull_request"` // its URL
	Merge       *string  `json:"merge"`        // merged, closed, pending or failed; nil when not asked
	Commit      *string  `json:"commit"`
	// Checks are, with merge failed because of CI, the checks that failed.
	Checks []FailedCheck `json:"checks,omitempty"`

	// What the push scan found, at publish and stop: a known one comes with
	// the outcome secret, nothing pushed; the others are warnings.
	Secrets []StepSecret `json:"secrets,omitempty"`
	// RefusedHosts are the host:port pairs the egress proxy refused during
	// a run, resolve or check step, once each, in lower case (decision
	// 2026-11-17-hosts-from-cinderline). Untrusted: the agent chose them.
	RefusedHosts []string `json:"refused_hosts,omitempty"`
}

// StepSecret is a secret the push scan found: its kind and place, never its
// value (decision 2026-11-17-open-questions-answered, point 2).
type StepSecret struct {
	Kind   string  `json:"kind"` // such as "anthropic key" or "private key block"
	Commit string  `json:"commit"`
	Path   *string `json:"path"` // nil: in the commit's message or author
	Known  bool    `json:"known"`
}

// Usage is the tokens and cost of one agent turn.
type Usage struct {
	Issue            string   `json:"issue"`
	Agent            string   `json:"agent"`
	Model            *string  `json:"model"` // nil: the client's default
	InputTokens      int      `json:"input_tokens"`
	OutputTokens     int      `json:"output_tokens"`
	CacheReadTokens  int      `json:"cache_read_tokens"`
	CacheWriteTokens int      `json:"cache_write_tokens"`
	CostUSD          *float64 `json:"cost_usd"` // nil: the app estimates it
	// Tools is the turn's tool calls per tool name, counts only (decision
	// 2026-11-24-tool-use-counts); none: the turn counted no tools.
	Tools []ToolCalls `json:"tools,omitempty"`
}

// ToolCalls is how often a turn called one tool, as the agent CLI names it.
type ToolCalls struct {
	Name  string `json:"name"`
	Calls int    `json:"calls"`
}

// NextAnswer is the next step, and the state to send back with the next call.
type NextAnswer struct {
	State string `json:"state"`
	Step  Step   `json:"step"`
}

// Step is one step of the closed set, flat: Kind says which fields it has.
// Nothing in it is ever run, or used as a path or argument, except Prompt
// as the agent's prompt, Token as its MCP token, an attachment's Name once
// it is one safe file name and AllowHosts once each is a valid host:port.
type Step struct {
	Kind        string           `json:"kind"`
	Issue       *StepIssue       `json:"issue"`       // run and check
	Agent       string           `json:"agent"`       // run and resolve
	Session     string           `json:"session"`     // run and resolve: new or resume; done: paused or kept
	Token       *string          `json:"token"`       // run and resolve
	Prompt      string           `json:"prompt"`      // run and resolve
	Attachments []StepAttachment `json:"attachments"` // run; nil leaves the folder as it is
	// AllowHosts are host:port pairs a person allowed for the issue's runs
	// (run, resolve and check): checked as allow_hosts entries, and ignored
	// on a strict runner, before they join the step's allowlist.
	AllowHosts []string `json:"allow_hosts"`
	// publish
	ResolveConflicts bool   `json:"resolve_conflicts"`
	Merge            bool   `json:"merge"`
	Reason           string `json:"reason"` // only logged
}

// StepIssue is the issue or sub-issue of a step.
type StepIssue struct {
	Ref   string `json:"ref"`
	Title string `json:"title"`
}

// StepAttachment is a file a run step's agent gets under /attachments.
type StepAttachment struct {
	Issue string `json:"issue"`
	ID    int    `json:"id"`
	Name  string `json:"name"`
}
