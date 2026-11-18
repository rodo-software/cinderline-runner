package daemon

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/redact"
)

// Decision 2026-11-18-verbose-agent-output: container output keeps only
// printable text and, on a terminal, its colours; a progress bar its last frame.
func TestCleanKeepsTextAndColoursOnly(t *testing.T) {
	vitest := " \x1b[32m✓\x1b[39m lib/agents.test.ts \x1b[2m(\x1b[22m\x1b[2m11 tests\x1b[22m\x1b[2m)\x1b[22m"
	bar := "   0/360 [░░]   0%\x1b[1G\x1b[2K 180/360 [▓░]  50%\x1b[1G\x1b[2K 360/360 [▓▓] 100%"
	for _, c := range []struct {
		name, in string
		color    bool
		want     string
	}{
		{"plain", "  PASS  516 files", false, "  PASS  516 files"},
		{"colours dropped", vitest, false, " ✓ lib/agents.test.ts (11 tests)"},
		{"colours kept with a reset", vitest, true, vitest + "\x1b[0m"},
		{"combined SGR kept", "\x1b[37;44m INFO \x1b[39;49m ok", true, "\x1b[37;44m INFO \x1b[39;49m ok\x1b[0m"},
		{"progress bar", bar, false, " 360/360 [▓▓] 100%"},
		{"carriage return", "10%\r50%\r100%", false, "100%"},
		{"trailing carriage return", "done\r", false, "done"},
		{"trailing erase", "done\x1b[1G\x1b[2K", false, "done"},
		{"blank frame", "done\r   ", false, "done"},
		{"empty", "", true, ""},
		{"OSC 52 with BEL", "\x1b]52;c;aGVsbG8=\x07text", true, "text"},
		{"OSC title with ST", "\x1b]0;pwned\x1b\\text", true, "text"},
		{"unterminated OSC", "text\x1b]8;;http://x", true, "text"},
		{"DCS", "\x1bP+q544e\x1b\\text", true, "text"},
		{"private mode set as m", "\x1b[>4;1mtext", true, "text"},
		{"SGR with intermediate", "\x1b[1 mtext", true, "text"},
		{"clear screen", "\x1b[2J\x1b[Htext", true, "text"},
		{"move to column 5 is no restart", "ab\x1b[5Gcd", false, "abcd"},
		{"charset", "\x1b(Btext", true, "text"},
		{"keypad mode", "\x1b=text\x1b>", true, "text"},
		{"lone escape", "text\x1b", true, "text"},
		{"malformed CSI", "a\x1b[12", true, "a"},
		{"controls", "a\x07b\x08c\x00d\x7fe", false, "abcde"},
		{"tab kept", "a\tb", false, "a\tb"},
		{"C1 CSI", "a\u009b31mb", true, "a31mb"},
		{"invalid UTF-8", "a\xffb", false, "a�b"},
	} {
		if got := clean(c.in, c.color); got != c.want {
			t.Errorf("%s: clean(%q, %v) = %q, want %q", c.name, c.in, c.color, got, c.want)
		}
	}
}

// The -v view prints container output as `time ISSUE │ line`; the log file
// keeps the cleaned line in its record; other records pass as before.
func TestLoggerShowsContainerOutputAsPlainLines(t *testing.T) {
	var file, view bytes.Buffer
	log := logger(&file, &view, true).With("project", "EMBER", "issue", "CL-244")
	log.Debug(environment.OutputMsg, "line", "\x1b]0;title\x07\x1b[32m✓\x1b[39m ok")
	log.Info("run started")

	lines := strings.Split(strings.TrimSuffix(view.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("view %q, want 2 lines", view.String())
	}
	if got, want := lines[0][len("15:04:05"):], " CL-244 │ \x1b[32m✓\x1b[39m ok\x1b[0m"; got != want {
		t.Errorf("view line %q, want %q after the time", lines[0], want)
	}
	if !strings.Contains(lines[1], `msg="run started"`) || !strings.Contains(lines[1], "issue=CL-244") {
		t.Errorf("other record in view: %q", lines[1])
	}
	if !strings.Contains(file.String(), `"line":"✓ ok"`) || !strings.Contains(file.String(), `"issue":"CL-244"`) {
		t.Errorf("log file %q, want the cleaned line", file.String())
	}
	if strings.Contains(file.String(), `\u001b`) {
		t.Errorf("log file keeps escapes: %q", file.String())
	}
}

// A secret that escapes split is redacted once they are removed.
func TestLoggerRedactsTheCleanedLine(t *testing.T) {
	const secret = "sk-output-test-0123456789"
	redact.Add("test key", secret)
	var file, view bytes.Buffer
	log := logger(&file, &view, false)
	log.Debug(environment.OutputMsg, "line", secret[:8]+"\x1b[0m"+secret[8:])
	for name, out := range map[string]string{"file": file.String(), "view": view.String()} {
		if strings.Contains(out, secret) || !strings.Contains(out, "[redacted test key]") {
			t.Errorf("%s %q, want the key redacted", name, out)
		}
	}
	if !strings.Contains(view.String(), " │ [redacted test key]\n") {
		t.Errorf("view %q, want no issue in the prefix", view.String())
	}
}

// Without -v the log file is at Info and keeps no container output.
func TestLoggerWithoutViewKeepsNoContainerOutput(t *testing.T) {
	var file bytes.Buffer
	logger(&file, nil, false).Debug(environment.OutputMsg, "line", "x")
	if file.Len() != 0 {
		t.Errorf("log file %q", file.String())
	}
}
