package web

import (
	"encoding/json"
	"strings"
)

// parseClaude is the events of one line of Claude Code's stream-json
// output: one per content block of an assistant or user message, usage for
// the result line, and raw for anything else. It never fails.
func parseClaude(line string) []Event {
	var m struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				Thinking  string          `json:"thinking"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"` // a string or text blocks
				IsError   bool            `json:"is_error"`
			} `json:"content"` // the prompt's content, a string, fails and gives raw
		} `json:"message"`
		Cost  *float64 `json:"total_cost_usd"`
		Usage *struct {
			Input  *int `json:"input_tokens"`
			Output *int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(line), &m) != nil {
		return raw
	}
	var out []Event
	switch m.Type {
	case "assistant", "user":
		for _, b := range m.Message.Content {
			e := raw[0]
			switch {
			case m.Type == "assistant" && b.Type == "text":
				e = Event{Kind: "text", Text: b.Text}
			case m.Type == "assistant" && b.Type == "thinking":
				e = Event{Kind: "thinking", Text: b.Thinking}
			case m.Type == "assistant" && b.Type == "tool_use":
				e = Event{Kind: "tool_call", ID: b.ID, Name: b.Name, Input: cut(string(b.Input))}
			case m.Type == "user" && b.Type == "tool_result":
				e = Event{Kind: "tool_result", ID: b.ToolUseID, Text: cut(resultText(b.Content)), Error: b.IsError}
			}
			out = append(out, e)
		}
	case "result":
		// The result's usage is the turn's total; a message's own usage is
		// left out, as Claude repeats it on each line of the message.
		if m.Usage != nil || m.Cost != nil {
			e := Event{Kind: "usage", CostUSD: m.Cost}
			if m.Usage != nil {
				e.InputTokens, e.OutputTokens = m.Usage.Input, m.Usage.Output
			}
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return raw
	}
	return out
}

// resultText is a tool result's content: a string, or the text of its text
// blocks, one per line.
func resultText(c json.RawMessage) string {
	var s string
	if json.Unmarshal(c, &s) == nil {
		return s
	}
	var blocks []struct{ Type, Text string }
	_ = json.Unmarshal(c, &blocks) // anything else gives no text
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
