package cinderline

import (
	"encoding/json"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/config"
)

// Labeled is a value with its display label, such as a status.
type Labeled struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// IssueSummary identifies a related issue.
type IssueSummary struct {
	Ref   string `json:"ref"`
	Title string `json:"title"`
}

// SubIssue is a sub-issue as listed on its parent.
type SubIssue struct {
	IssueSummary
	Status Labeled `json:"status"`
}

// IssueTeam is the team an issue is assigned to.
type IssueTeam struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	// Trusted is the team's trusted, on the Review list of a sync answer;
	// nil where the app does not send it: GetTeam knows
	// (decision 2026-11-16-sync-request).
	Trusted *bool `json:"trusted,omitempty"`
}

// IssueAgent is the agent that created an issue.
type IssueAgent struct {
	Name string `json:"name"`
}

// Issue is an issue without its comments. Body is untrusted text.
type Issue struct {
	ID          int           `json:"id"`
	Number      int           `json:"number"`
	Ref         string        `json:"ref"`
	Title       string        `json:"title"`
	Body        *string       `json:"body"`
	Status      Labeled       `json:"status"`
	Priority    Labeled       `json:"priority"`
	MergeStatus *string       `json:"merge_status"`
	Position    int           `json:"position"`
	Team        *IssueTeam    `json:"team"`
	Parent      *IssueSummary `json:"parent"`
	Agent       *IssueAgent   `json:"agent"`
	SubIssues   []SubIssue    `json:"sub_issues"`
	Attachments []Attachment  `json:"attachments"` // nil from an app before EMBER-208: none
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	CompletedAt *time.Time    `json:"completed_at"`
	ArchivedAt  *time.Time    `json:"archived_at"`
}

// Attachment is a file attached to an issue (EMBER-208). Name is untrusted
// text from the uploader.
type Attachment struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	MimeType string `json:"mime_type"`
	URL      string `json:"url"`
}

// IssueWithComments is an issue with its comments, oldest first, and its
// cost (EMBER-112).
type IssueWithComments struct {
	Issue
	Comments   []Comment  `json:"comments"`
	Usage      IssueUsage `json:"usage"`
	BudgetUSD  *float64   `json:"budget_usd"`  // nil: no budget; a sub-issue's is its parent's
	OverBudget bool       `json:"over_budget"` // for a sub-issue, its parent's total reached the budget
}

// IssueUsage totals the usage entries of an issue and its sub-issues.
type IssueUsage struct {
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	CacheReadTokens  int     `json:"cache_read_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"` // only the known cost
	CostIncomplete   bool    `json:"cost_incomplete"`
}

// UsageEntry is the tokens and cost of one agent CLI run, for ReportUsage.
type UsageEntry struct {
	Issue            string   `json:"issue"` // the run's issue or one of its sub-issues
	Agent            string   `json:"agent"` // the team's agent; the app labels it "<team> / <agent>"
	Model            *string  `json:"model"` // nil: the client's default
	InputTokens      int      `json:"input_tokens"`
	OutputTokens     int      `json:"output_tokens"`
	CacheReadTokens  int      `json:"cache_read_tokens"`
	CacheWriteTokens int      `json:"cache_write_tokens"`
	CostUSD          *float64 `json:"cost_usd"` // nil: the app estimates it
}

// CommentAuthor wrote a comment; Agent is set when an agent wrote it through its token.
type CommentAuthor struct {
	Name  string  `json:"name"`
	Agent *string `json:"agent"`
}

// Comment is a comment on an issue. Body is untrusted text.
type Comment struct {
	ID        int           `json:"id"`
	Body      string        `json:"body"`
	Author    CommentAuthor `json:"author"`
	CreatedAt time.Time     `json:"created_at"`
}

// Project is the project the runner token belongs to.
type Project struct {
	Key           string `json:"key"`
	Name          string `json:"name"`
	RepositoryURL string `json:"repository_url"`
	Forge         string `json:"forge"`
	BaseBranch    string `json:"base_branch"`
	Environment   string `json:"environment"`
	// ComposeFile and AgentService are the compose file of environment
	// docker and its agent service; empty means the runner's defaults
	// (decision 2026-11-08-m6-environments point 1).
	ComposeFile  string `json:"compose_file,omitempty"`
	AgentService string `json:"agent_service,omitempty"`
	// CheckCommand is run through sh -c in the clone after each agent run
	// that changes code (CSR-80); set by admins. Nil: no check.
	CheckCommand *string `json:"check_command,omitempty"`
	// LinkedProjects are the projects this project's agents may reach, one
	// direction per link (decision 2026-11-11-project-links). An app that
	// does not send it yet means none.
	LinkedProjects []LinkedProject `json:"linked_projects"`
	// IdleMinutes stops an agent run that printed nothing this long; set
	// per project in the app (CSR-159). Nil: the runner's default.
	IdleMinutes *int `json:"idle_minutes,omitempty"`
	// Workspace is the project's workspace (EMBER-159); nil from a
	// Cinderline from before workspaces.
	Workspace *Workspace `json:"workspace,omitempty"`
	// LongPoll says the app can hold GET /work open (FrankenPHP or
	// Octane); false, also from an older app, means short polls (CSR-93).
	LongPoll bool `json:"long_poll,omitempty"`
	// Sync says the app offers POST /sync, one request in place of the
	// timed ones; false, also from an older app, means the separate
	// requests (decision 2026-11-16-sync-request).
	Sync bool `json:"sync,omitempty"`
}

// SyncAnswer is the app's answer to POST /sync: what the runner otherwise
// asks for with separate requests on timers.
type SyncAnswer struct {
	Work    *Issue    `json:"work"`    // the first To do issue with a team, as GET /work's issue
	Watched []Watched `json:"watched"` // one per watched ref the project has
	Review  []Issue   `json:"review"`  // the Review issues, at most 200, their teams with Trusted
}

// Watched is what a running turn's watcher needs of its issue.
type Watched struct {
	Ref                string  `json:"ref"`
	Status             Labeled `json:"status"`
	LastCommentByAgent bool    `json:"last_comment_by_agent"` // the latest comment was written by an agent
}

// Workspace is the workspace a project belongs to.
type Workspace struct {
	ID   int    `json:"id"`
	Slug string `json:"slug"`
}

// DefaultWorkspace is the slug of a project whose Cinderline sends no workspace.
const DefaultWorkspace = config.DefaultWorkspace

// WorkspaceSlug is the project's workspace slug, DefaultWorkspace when the
// Cinderline sends none (CSR-97).
func (p Project) WorkspaceSlug() string {
	if p.Workspace == nil || p.Workspace.Slug == "" {
		return DefaultWorkspace
	}
	return p.Workspace.Slug
}

// LinkedProject is a project another project links to.
type LinkedProject struct {
	Key           string `json:"key"`
	Name          string `json:"name"`
	RepositoryURL string `json:"repository_url"`
	BaseBranch    string `json:"base_branch"`
}

// TeamAgent is one agent of a team. Instructions is untrusted text.
type TeamAgent struct {
	Name         string  `json:"name"`
	Role         string  `json:"role"`
	Client       string  `json:"client"`
	Model        *string `json:"model"`
	Effort       *string `json:"effort"` // nil: the client's default (app decision 2026-11-06-effort-per-team-slot)
	Instructions *string `json:"instructions"`
	Description  *string `json:"description"` // what the agent is good at, for the orchestrator's prompt
	Price        *Price  `json:"price"`       // nil when unknown
}

// Price is a model's price in USD per million tokens.
type Price struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

// Team is a team of the project, with its agents.
type Team struct {
	ID           int         `json:"id"`
	Name         string      `json:"name"`
	MaxMinutes   int         `json:"max_minutes"`
	MaxTurns     int         `json:"max_turns"` // 0 when the app does not send it; the runner then uses 50 (decision 2026-11-07-team-max-turns)
	Trusted      bool        `json:"trusted"`
	MaxSubIssues int         `json:"max_sub_issues"` // Cinderline refuses more sub-issues on one issue
	Ponytail     bool        `json:"ponytail"`       // load the ponytail plugin in Claude Code runs (2026-11-03-per-team-ponytail-plugin)
	GenericRuns  bool        `json:"generic_runs"`   // the orchestrator acts through MCP alone: no files, no pull request (CSR-75)
	Agents       []TeamAgent `json:"agents"`
}

// ReportClient is the state of one agent CLI on this machine.
type ReportClient struct {
	Name    string  `json:"name"`
	Version *string `json:"version"`
	State   string  `json:"state"`
}

// ReportError is one error the runner holds: a kind, optionally an issue ref
// (for issue-specific errors such as a missing key), and a message. Its
// Error() returns the message.
type ReportError struct {
	Kind    string `json:"kind"`
	Issue   string `json:"issue,omitempty"`
	Message string `json:"message"`
}

func (e *ReportError) Error() string { return e.Message }

// Error kinds for the report's errors list.
const (
	KindRepositoryMismatch = "repository_mismatch"
	KindMissingKey         = "missing_key"
	KindUnsupportedClient  = "unsupported_client"
	KindPollFailed         = "poll_failed"
)

// Report is what the runner tells the app about itself.
type Report struct {
	RunnerVersion string         `json:"runner_version"`
	Clients       []ReportClient `json:"clients"`
	Environments  []string       `json:"environments"`
	Errors        []ReportError  `json:"errors"`
	// RefusedConnections counts the hosts the egress proxy refused for this
	// project since the runner started (CSR-63); 0 is left out.
	RefusedConnections int `json:"refused_connections,omitempty"`
}

// UpdateIssueRequest changes an issue. Status needs ExpectedStatus; empty fields are not sent.
type UpdateIssueRequest struct {
	Status           string `json:"status,omitempty"`
	ExpectedStatus   string `json:"expected_status,omitempty"`
	MergeStatus      string `json:"merge_status,omitempty"`
	ClearMergeStatus bool   `json:"-"` // sends merge_status null instead
}

// MarshalJSON sends merge_status null when ClearMergeStatus is set.
func (r UpdateIssueRequest) MarshalJSON() ([]byte, error) {
	type plain UpdateIssueRequest
	if !r.ClearMergeStatus {
		return json.Marshal(plain(r))
	}
	return json.Marshal(struct {
		plain
		MergeStatus *string `json:"merge_status"` // shadows plain's
	}{plain: plain(r)})
}

// TokenKind is what an agent token may do (decision 2026-11-05-runner-rewrite-details point 6).
type TokenKind string

// The agent token kinds.
const (
	KindOrchestrator TokenKind = "orchestrator"
	KindReader       TokenKind = "reader"
)

// AgentToken is a short-lived token for one agent run. Never log it.
type AgentToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// NextRequest is what the runner just did and the facts only it knows, for
// Next (decision 2026-11-15-server-decides-steps).
type NextRequest struct {
	Context   string             `json:"context"` // "run" or "review"
	State     *string            `json:"state"`   // the last answer's, unchanged; nil on the first call
	Last      *LastStep          `json:"last"`    // nil on the first call
	Sessions  []ResumableSession `json:"sessions"`
	Workspace RunWorkspace       `json:"workspace"`
}

// ResumableSession is a client session the runner can resume now. Origin is
// "paused", "kept" or "turn".
type ResumableSession struct {
	Issue  string `json:"issue"`
	Agent  string `json:"agent"`
	Origin string `json:"origin"`
}

// RunWorkspace is the run's clone and mounts, for the app's prompts.
type RunWorkspace struct {
	Branch            string                `json:"branch"`
	Base              string                `json:"base"`
	ExtraRepositories []WorkspaceRepository `json:"extra_repositories"`
	LinkedProjects    []WorkspaceLink       `json:"linked_projects"`
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
// Conflicts and Commits are untrusted.
type LastStep struct {
	ID        string      `json:"id"`
	Step      string      `json:"step"` // run, resolve, check or merge
	Issue     string      `json:"issue"`
	Agent     *string     `json:"agent"`
	Outcome   string      `json:"outcome"`
	Error     *string     `json:"error"`
	Report    *string     `json:"report"`
	Tail      *string     `json:"tail"`
	Usage     *UsageEntry `json:"usage"`
	Conflicts []string    `json:"conflicts"`
	Commits   *string     `json:"commits"`
}

// NextResponse is the next step, and the state to send back with the next call.
type NextResponse struct {
	State string `json:"state"`
	Step  Step   `json:"step"`
}

// Step is one step of the closed set in nextStep, flat: Kind says which
// fields it has. Nothing in it is ever run, or used as a path or argument,
// except Prompt as the agent's prompt and an attachment's Name once it is a
// safe file name.
type Step struct {
	Kind string `json:"kind"`
	// run and resolve
	Agent    string  `json:"agent"`
	Session  string  `json:"session"` // new or resume
	Token    string  `json:"token"`   // orchestrator, reader or none
	Prompt   string  `json:"prompt"`
	Progress *string `json:"progress"` // also check's
	// run and check
	Issue *StepIssue `json:"issue"`
	// run: nil leaves the attachments folder as it is
	Attachments []StepAttachment `json:"attachments"`
	// publish
	ResolveConflicts bool    `json:"resolve_conflicts"`
	ReportHead       *string `json:"report_head"`
	ReportFoot       *string `json:"report_foot"`
	// finish, pause and fail
	Comment *string `json:"comment"`
	Reason  string  `json:"reason"` // pause and fail, for the log
	Block   bool    `json:"block"`  // pause
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
