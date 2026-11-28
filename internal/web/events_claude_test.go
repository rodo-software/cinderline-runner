package web

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseClaude(t *testing.T) {
	in, out, cost := 10, 20, 0.25
	long := `{"command":"` + strings.Repeat("é", maxEventText) + `"}`
	for _, tc := range []struct {
		name, line string
		want       []Event
	}{
		{"text and tool call", `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"Looking."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]},"session_id":"s1"}`,
			[]Event{{Kind: "text", Text: "Looking."}, {Kind: "tool_call", ID: "t1", Name: "Bash", Input: `{"command":"ls"}`}}},
		{"thinking", `{"type":"assistant","message":{"id":"m0","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"Let me look.","signature":"EqQB"}]},"parent_tool_use_id":null,"session_id":"s1"}`,
			[]Event{{Kind: "thinking", Text: "Let me look."}}},
		{"tool result", `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a.go"}]},"session_id":"s1"}`,
			[]Event{{Kind: "tool_result", ID: "t1", Text: "a.go"}}},
		{"tool result as blocks, failed", `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":[{"type":"text","text":"Exit code 1"},{"type":"text","text":"FAIL"}]}]}}`,
			[]Event{{Kind: "tool_result", ID: "t2", Text: "Exit code 1\nFAIL", Error: true}}},
		{"final result with usage", `{"type":"result","subtype":"success","total_cost_usd":0.25,"usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":30,"cache_read_input_tokens":40}}`,
			[]Event{{Kind: "usage", InputTokens: &in, OutputTokens: &out, CostUSD: &cost}}},
		{"result with output tokens only", `{"type":"result","subtype":"success","usage":{"output_tokens":20}}`,
			[]Event{{Kind: "usage", OutputTokens: &out}}},
		{"result without usage", `{"type":"result","subtype":"success"}`, []Event{{Kind: "raw"}}},
		{"system line", `{"type":"system","subtype":"init","session_id":"s1","tools":["Bash","Edit"]}`, []Event{{Kind: "raw"}}},
		{"the prompt", `{"type":"user","message":{"role":"user","content":"Do it."},"session_id":"s1"}`, []Event{{Kind: "raw"}}},
		{"a message without content", `{"type":"assistant","message":{"usage":{"output_tokens":3}},"session_id":"s1"}`, []Event{{Kind: "raw"}}},
		{"an unknown block", `{"type":"assistant","message":{"content":[{"type":"redacted_thinking","data":"x"},{"type":"text","text":"Hi."}]}}`,
			[]Event{{Kind: "raw"}, {Kind: "text", Text: "Hi."}}},
		{"text in a user message", `{"type":"user","message":{"content":[{"type":"text","text":"Go on."}]}}`, []Event{{Kind: "raw"}}},
		{"not JSON", "noise", []Event{{Kind: "raw"}}},
		{"cut-off line", `{"type":"assistant","message":{"id":"m1","content":[{"type":"te`, []Event{{Kind: "raw"}}},
		{"line the transcript cut", "[70000 bytes, too long to keep]", []Event{{Kind: "raw"}}},
		{"long input", `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t3","name":"Bash","input":` + long + `}]}}`,
			[]Event{{Kind: "tool_call", ID: "t3", Name: "Bash", Input: long[:len(`{"command":"`)+2*(maxEventText-len(`{"command":"`))] + "… [cut: 20014 characters in all]"}}},
	} {
		if got := parseClaude(tc.line); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
