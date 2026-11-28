package web

import (
	"reflect"
	"testing"
)

// Lines in the shape of internal/client's Codex testdata (0.167.0).
func TestParseCodex(t *testing.T) {
	in, out := 200, 300
	for _, tc := range []struct {
		name, line string
		want       []Event
	}{
		{"message", `{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"Looking."}}`,
			[]Event{{Kind: "text", Text: "Looking."}}},
		{"reasoning", `{"type":"item.completed","item":{"id":"item_0","type":"reasoning","text":"Thinking."}}`,
			[]Event{{Kind: "thinking", Text: "Thinking."}}},
		{"command start", `{"type":"item.started","item":{"id":"item_2","type":"command_execution","command":"go test ./...","aggregated_output":"","exit_code":null,"status":"in_progress"}}`,
			[]Event{{Kind: "tool_call", ID: "item_2", Name: "command_execution", Input: `{"command":"go test ./..."}`}}},
		{"command completed", `{"type":"item.completed","item":{"id":"item_2","type":"command_execution","command":"go test ./...","aggregated_output":"ok","exit_code":0,"status":"completed"}}`,
			[]Event{{Kind: "tool_result", ID: "item_2", Text: "ok"}}},
		{"command failed", `{"type":"item.completed","item":{"id":"item_7","type":"command_execution","command":"go vet ./...","aggregated_output":"vet: x.go:1: bad","exit_code":1,"status":"failed"}}`,
			[]Event{{Kind: "tool_result", ID: "item_7", Text: "vet: x.go:1: bad", Error: true}}},
		{"command declined", `{"type":"item.completed","item":{"id":"item_8","type":"command_execution","command":"rm -rf /","aggregated_output":"","exit_code":null,"status":"declined"}}`,
			[]Event{{Kind: "tool_result", ID: "item_8", Error: true}}},
		{"mcp start", `{"type":"item.started","item":{"id":"item_5","type":"mcp_tool_call","server":"cinderline","tool":"get_issue","arguments":{"key":"CLR-1"},"result":null,"error":null,"status":"in_progress"}}`,
			[]Event{{Kind: "tool_call", ID: "item_5", Name: "mcp__cinderline__get_issue", Input: `{"key":"CLR-1"}`}}},
		{"mcp completed", `{"type":"item.completed","item":{"id":"item_5","type":"mcp_tool_call","server":"cinderline","tool":"get_issue","arguments":{},"result":{"content":[{"type":"text","text":"CLR-1: x"}],"structured_content":null},"error":null,"status":"completed"}}`,
			[]Event{{Kind: "tool_result", ID: "item_5", Text: "CLR-1: x"}}},
		{"mcp failed", `{"type":"item.completed","item":{"id":"item_9","type":"mcp_tool_call","server":"cinderline","tool":"get_issue","arguments":{},"result":null,"error":{"message":"no access"},"status":"failed"}}`,
			[]Event{{Kind: "tool_result", ID: "item_9", Text: "no access", Error: true}}},
		{"mcp without server", `{"type":"item.started","item":{"id":"item_9","type":"mcp_tool_call","status":"in_progress"}}`,
			[]Event{{Kind: "tool_call", ID: "item_9", Name: "mcp_tool_call"}}},
		{"file change, only completed", `{"type":"item.completed","item":{"id":"item_4","type":"file_change","changes":[{"path":"a.go","kind":"update"}],"status":"completed"}}`,
			[]Event{{Kind: "tool_call", ID: "item_4", Name: "file_change", Input: `{"changes":[{"path":"a.go","kind":"update"}]}`}, {Kind: "tool_result", ID: "item_4"}}},
		{"web search, only completed", `{"type":"item.completed","item":{"id":"item_6","type":"web_search","query":"go vet"}}`,
			[]Event{{Kind: "tool_call", ID: "item_6", Name: "web_search", Input: `{"query":"go vet"}`}, {Kind: "tool_result", ID: "item_6"}}},
		{"usage", `{"type":"turn.completed","usage":{"input_tokens":1200,"cached_input_tokens":1000,"cache_write_input_tokens":0,"output_tokens":300,"reasoning_output_tokens":120}}`,
			[]Event{{Kind: "usage", InputTokens: &in, OutputTokens: &out}}},
		{"turn without usage", `{"type":"turn.completed"}`, []Event{{Kind: "raw"}}},
		{"unknown item", `{"type":"item.completed","item":{"id":"item_8","type":"todo_list","items":[]}}`, []Event{{Kind: "raw"}}},
		{"message started", `{"type":"item.started","item":{"id":"item_1","type":"agent_message","text":""}}`, []Event{{Kind: "raw"}}},
		{"thread started", `{"type":"thread.started","thread_id":"019a5c3e-0000-7000-8000-000000000001"}`, []Event{{Kind: "raw"}}},
		{"turn failed", `{"type":"turn.failed","error":{"message":"unexpected status 401 Unauthorized"}}`, []Event{{Kind: "raw"}}},
		{"item.completed without item", `{"type":"item.completed"}`, []Event{{Kind: "raw"}}},
		{"null item", `{"type":"item.started","item":null}`, []Event{{Kind: "raw"}}},
		{"item not an object", `{"type":"item.started","item":5}`, []Event{{Kind: "raw"}}},
		{"not JSON", "2026-11-08T16:00:00Z WARN some log line on stderr", []Event{{Kind: "raw"}}},
		{"cut-off line", `{"type":"item.completed","item":{"id":"item_1","ty`, []Event{{Kind: "raw"}}},
	} {
		if got := parseCodex(tc.line); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
	msg := `{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"Looking."}}`
	if got, want := parseOut("codex", msg), []Event{{Kind: "text", Text: "Looking."}}; !reflect.DeepEqual(got, want) {
		t.Errorf("parseOut(codex) = %+v, want %+v", got, want)
	}
}
