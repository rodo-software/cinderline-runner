package cinderstack

import "time"

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
}

// TeamAgent is one agent of a team. Instructions is untrusted text.
type TeamAgent struct {
	Name         string  `json:"name"`
	Role         string  `json:"role"`
	Client       string  `json:"client"`
	Model        *string `json:"model"`
	Instructions *string `json:"instructions"`
}

// Team is a team of the project, with its agents.
type Team struct {
	ID         int         `json:"id"`
	Name       string      `json:"name"`
	MaxMinutes int         `json:"max_minutes"`
	Trusted    bool        `json:"trusted"`
	Agents     []TeamAgent `json:"agents"`
}

// ReportClient is the state of one agent CLI on this machine.
type ReportClient struct {
	Name    string  `json:"name"`
	Version *string `json:"version"`
	State   string  `json:"state"`
}

// Report is what the runner tells the app about itself.
type Report struct {
	RunnerVersion string         `json:"runner_version"`
	Clients       []ReportClient `json:"clients"`
	Environments  []string       `json:"environments"`
}

// UpdateIssueRequest changes an issue. Status needs ExpectedStatus; empty fields are not sent.
type UpdateIssueRequest struct {
	Status         string `json:"status,omitempty"`
	ExpectedStatus string `json:"expected_status,omitempty"`
	MergeStatus    string `json:"merge_status,omitempty"`
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
