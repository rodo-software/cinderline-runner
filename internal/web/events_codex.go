package web

import "encoding/json"

// codexInput is the field of each Codex tool item that is its input; an MCP
// call's input is its arguments themselves.
var codexInput = map[string]string{"command_execution": "command", "file_change": "changes", "web_search": "query", "mcp_tool_call": ""}

// parseCodex is the events of one line of Codex's `exec --json` output: text
// and thinking for a completed message and reasoning, a tool item's call
// when it starts and its result when it completes, usage for the turn's end,
// and raw for anything else. It never fails.
func parseCodex(line string) []Event {
	var m struct {
		Type  string          `json:"type"`
		Item  json.RawMessage `json:"item"`
		Usage *struct {
			Input  *int `json:"input_tokens"`
			Cached int  `json:"cached_input_tokens"`
			Output *int `json:"output_tokens"` // reasoning included
		} `json:"usage"`
	}
	var it *struct {
		ID, Type, Text, Server, Tool, Status string
		Output                               string `json:"aggregated_output"`
		ExitCode                             *int   `json:"exit_code"`
		Result                               *struct {
			Content json.RawMessage `json:"content"`
		} `json:"result"`
		Error *struct{ Message string } `json:"error"`
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &m) != nil || m.Item != nil && (json.Unmarshal(m.Item, &it) != nil || json.Unmarshal(m.Item, &fields) != nil) {
		return raw
	}
	switch {
	case m.Type == "turn.completed" && m.Usage != nil:
		// Codex's input tokens include the cached ones, Claude's do not;
		// the page shows them as Claude's, as internal/client counts them.
		// This is the thread's total so far, a resumed thread's earlier
		// turns included: a line alone cannot tell this turn's share.
		e := Event{Kind: "usage", OutputTokens: m.Usage.Output}
		if u := m.Usage.Input; u != nil {
			n := max(*u-m.Usage.Cached, 0)
			e.InputTokens = &n
		}
		return []Event{e}
	case it == nil:
		return raw
	case m.Type == "item.completed" && it.Type == "agent_message":
		return []Event{{Kind: "text", Text: it.Text}}
	case m.Type == "item.completed" && it.Type == "reasoning":
		return []Event{{Kind: "thinking", Text: it.Text}}
	}
	key, tool := codexInput[it.Type]
	if !tool {
		return raw
	}
	call := Event{Kind: "tool_call", ID: it.ID, Name: it.Type}
	if it.Type == "mcp_tool_call" && it.Server != "" && it.Tool != "" {
		call.Name = "mcp__" + it.Server + "__" + it.Tool // as Codex names MCP tools for the model
	}
	if v := fields[key]; key == "" {
		call.Input = cut(string(fields["arguments"]))
	} else if v != nil {
		in, _ := json.Marshal(map[string]json.RawMessage{key: v}) // cannot fail: v is valid JSON
		call.Input = cut(string(in))
	}
	switch m.Type {
	case "item.started":
		return []Event{call}
	case "item.completed":
		res := Event{Kind: "tool_result", ID: it.ID, Text: it.Output,
			Error: it.Status == "failed" || it.Status == "declined" || it.ExitCode != nil && *it.ExitCode != 0}
		switch {
		case it.Error != nil && it.Error.Message != "":
			res.Text = it.Error.Message
		case it.Result != nil:
			res.Text = resultText(it.Result.Content)
		}
		res.Text = cut(res.Text)
		// Codex 0.167.0 reports a file change and a web search only when
		// they complete (internal/client's testdata), so their line is
		// also their call.
		if it.Type == "file_change" || it.Type == "web_search" {
			return []Event{call, res}
		}
		return []Event{res}
	}
	return raw
}
