package web

import (
	"fmt"
	"unicode/utf8"
)

// maxEventText is the most characters of a tool call's input or a tool
// result's text an event carries; the raw line keeps all of it.
const maxEventText = 20_000

// Event is one thing an out line of an agent turn says, for the page's
// timeline. Kind is text, thinking, tool_call, tool_result, usage or raw; a
// field of its kind that is empty or false is left out, and raw has no
// fields: the page shows the line itself.
type Event struct {
	Kind         string   `json:"kind"`
	ID           string   `json:"id,omitempty"`           // tool_call, and the call of a tool_result
	Name         string   `json:"name,omitempty"`         // tool_call
	Text         string   `json:"text,omitempty"`         // text, thinking, tool_result
	Input        string   `json:"input,omitempty"`        // tool_call: its JSON input as text
	Error        bool     `json:"error,omitempty"`        // tool_result
	InputTokens  *int     `json:"input_tokens,omitempty"` // usage, when the line reports them
	OutputTokens *int     `json:"output_tokens,omitempty"`
	CostUSD      *float64 `json:"cost_usd,omitempty"`
}

var raw = []Event{{Kind: "raw"}}

// parseOut is the events of an out line of a step of client. A client
// without a parser gets raw for every line.
func parseOut(client, line string) []Event {
	if client == "claude" {
		return parseClaude(line)
	}
	return raw
}

// cut is s at most maxEventText characters, with a marker when cut.
func cut(s string) string {
	n := utf8.RuneCountInString(s)
	if n <= maxEventText {
		return s
	}
	return string([]rune(s)[:maxEventText]) + fmt.Sprintf("… [cut: %d characters in all]", n)
}
