package client

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/environment"
)

// CL-267's recorded fields, with unrelated fields omitted. The weekly case
// uses the same event shape; the duration is not part of detection.
const claudeUsageResult = `{"type":"result","is_error":true,"api_error_status":429,"api_error":"usage_limit_reached","result":"You've hit your 5-hour limit · resets 10:40am (UTC)","session_id":"s1"}`

func TestParseClaudeUsageLimit(t *testing.T) {
	event := func(status, limit, reset string) string {
		return `{"type":"rate_limit_event","rate_limit_info":{"status":"` + status + `","rateLimitType":"` + limit + `"` + reset + `}}`
	}
	for _, tc := range []struct {
		name, out string
		hit       bool
		reset     int64
	}{
		{"five hour", event("rejected", "five_hour", `,"resetsAt":1795084800`) + "\n" + claudeUsageResult, true, 1795084800},
		{"weekly event alone", event("rejected", "seven_day", `,"resetsAt":1795084800`), true, 1795084800},
		{"without reset", claudeUsageResult, true, 0},
		{"invalid reset", event("rejected", "five_hour", `,"resetsAt":9223372036854775807`), true, 0},
		{"event without reset", event("rejected", "five_hour", ""), true, 0},
		{"ordinary 429", `{"type":"result","is_error":true,"api_error_status":429,"api_error":"rate_limit_error"}`, false, 0},
		{"last event wins", event("rejected", "five_hour", `,"resetsAt":1795084800`) + "\n" + event("allowed", "five_hour", ""), false, 0},
		{"reset does not leak", event("allowed", "five_hour", `,"resetsAt":1795084800`) + "\n" + event("rejected", "seven_day", ""), true, 0},
		{"allowed reset with explicit error", event("allowed", "five_hour", `,"resetsAt":1795084800`) + "\n" + claudeUsageResult, true, 1795084800},
		{"text is not a signal", `{"type":"result","result":"usage_limit_reached"}`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseClaude(tc.out)
			if got.HitUsageLimit != tc.hit {
				t.Fatalf("result = %+v", got)
			}
			if tc.reset == 0 {
				if !got.ResetsAt.IsZero() {
					t.Errorf("unexpected reset %v", got.ResetsAt)
				}
			} else if !got.ResetsAt.Equal(time.Unix(tc.reset, 0)) || got.ResetsAt.Location() != time.UTC {
				t.Errorf("reset = %v", got.ResetsAt)
			}
		})
	}
}

func TestClaudeUsageLimitKeepsSession(t *testing.T) {
	for _, session := range []string{"", "s1"} {
		for _, exitErr := range []error{nil, errors.New("exit status 1")} {
			// A resumed CLI can reject before printing a session ID.
			out := claudeUsageResult
			if session != "" {
				out = strings.ReplaceAll(out, `,"session_id":"s1"`, "")
			}
			env := &environment.Fake{Runs: []environment.RunReply{{Output: out, Err: exitErr}}}
			c := &CLI{Name: "claude", Project: "CSR", Env: env, Exec: func(context.Context, []string, ...string) (string, error) { return "", nil }}
			got, err := c.Run(context.Background(), Turn{Dir: "/runs/CSR-1", SessionID: session})
			if err == nil || !got.HitUsageLimit || got.SessionID != "s1" || got.Tail != out {
				t.Errorf("Run = %+v, %v", got, err)
			}
			if exitErr != nil && !errors.Is(err, exitErr) {
				t.Errorf("lost exit error: %v", err)
			}
		}
	}
}

// Generic HTTP failures in each client's event shape must not pause a run
// as a subscription limit. These are synthetic boundary cases.
func TestGeneric429IsNotUsageLimit(t *testing.T) {
	for name, out := range map[string]string{
		"claude":   `{"type":"result","is_error":true,"api_error_status":429,"api_error":"rate_limit_error","session_id":"s1"}`,
		"codex":    `{"type":"thread.started","thread_id":"s1"}` + "\n" + `{"type":"turn.failed","error":{"message":"unexpected status 429 Too Many Requests"}}`,
		"gemini":   `{"type":"init","session_id":"s1"}` + "\n" + `{"type":"result","status":"error","error":{"type":"APIError","message":"429 Too Many Requests"}}`,
		"opencode": `{"type":"error","sessionID":"s1","error":{"name":"APIError","data":{"statusCode":429,"message":"Too Many Requests"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			boom := errors.New("exit 1")
			env := &environment.Fake{Runs: []environment.RunReply{{Output: out, Err: boom}}}
			c := &CLI{Name: name, Project: "CSR", Credential: "test-key", Env: env, Exec: func(context.Context, []string, ...string) (string, error) { return "", nil }}
			got, err := c.Run(context.Background(), Turn{Dir: "/runs/CSR-1"})
			if !errors.Is(err, boom) || got.HitUsageLimit || !got.ResetsAt.IsZero() || got.SessionID != "s1" {
				t.Errorf("Run = %+v, %v", got, err)
			}
		})
	}
}

func TestClaudeUsageLimitEventKeepsEarlierSession(t *testing.T) {
	out := `{"type":"system","subtype":"init","session_id":"s1"}` + "\n" + `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`
	got := parseClaude(out)
	if !got.HitUsageLimit || got.SessionID != "s1" {
		t.Fatalf("result = %+v", got)
	}
}
