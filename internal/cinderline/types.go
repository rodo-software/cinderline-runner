package cinderline

import (
	"encoding/json"
	"time"
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
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	CompletedAt *time.Time    `json:"completed_at"`
	ArchivedAt  *time.Time    `json:"archived_at"`
}

// IssueWithComments is an issue with its comments, oldest first.
type IssueWithComments struct {
	Issue
	Comments []Comment `json:"comments"`
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
	// LinkedProjects are the projects this project's agents may reach, one
	// direction per link (decision 2026-11-11-project-links). An app that
	// does not send it yet means none.
	LinkedProjects []LinkedProject `json:"linked_projects"`
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
