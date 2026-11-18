package daemon

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/redact"
)

// logger is the daemon's log: JSON to file, and with view (run -v) also text
// to view, both at Debug. Container output is untrusted and full of terminal
// escapes, so its lines are cleaned: the file keeps the plain text, the view
// prints each as `15:04:05 ISSUE │ line` with colours when color is set
// (decision 2026-11-18-verbose-agent-output).
func logger(file, view io.Writer, color bool) *slog.Logger {
	if view == nil {
		return slog.New(redact.Handler(slog.NewJSONHandler(file, nil)))
	}
	debug := &slog.HandlerOptions{Level: slog.LevelDebug}
	return slog.New(redact.Handler(slog.NewMultiHandler(
		output{Handler: slog.NewJSONHandler(file, debug)},
		output{Handler: slog.NewTextHandler(view, debug), view: view, color: color, mu: &sync.Mutex{}},
	)))
}

// output passes every record on to its Handler except container output: it
// cleans that line and, with a view, prints it there itself.
type output struct {
	slog.Handler
	view  io.Writer // nil: pass the cleaned line on in the record
	color bool
	mu    *sync.Mutex // serializes writes to view
	issue string
}

func (h output) Handle(ctx context.Context, r slog.Record) error {
	if r.Message != environment.OutputMsg {
		return h.Handler.Handle(ctx, r)
	}
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	line := ""
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "line" {
			// Redacted again: removing escapes may join a value they split.
			line = redact.String(clean(a.Value.String(), h.color))
			a = slog.String("line", line)
		}
		out.AddAttrs(a)
		return true
	})
	if h.view == nil {
		return h.Handler.Handle(ctx, out)
	}
	prefix := r.Time.Format("15:04:05") + " "
	if h.issue != "" {
		prefix += h.issue + " "
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.view, prefix+"│ "+line+"\n")
	return err
}

func (h output) WithAttrs(as []slog.Attr) slog.Handler {
	for _, a := range as {
		if a.Key == "issue" {
			h.issue = a.Value.String()
		}
	}
	h.Handler = h.Handler.WithAttrs(as)
	return h
}

func (h output) WithGroup(name string) slog.Handler {
	h.Handler = h.Handler.WithGroup(name)
	return h
}

// clean returns line as a terminal would leave it, safe to print: only
// printable text, tabs and, when color is set, colour codes (SGR) survive.
// Every other escape (cursor moves, OSC titles and clipboard, modes) and
// control character is dropped. A carriage return or a move to column 1
// starts the line over, so a progress bar keeps its last frame; a last frame
// with nothing visible gives the one before it. With colour, the line ends
// with a reset so it does not bleed into the next.
func clean(line string, color bool) string {
	var frame, last strings.Builder
	visible, lastVisible := false, false
	restart := func() {
		if visible {
			last.Reset()
			last.WriteString(frame.String())
			lastVisible = true
		}
		frame.Reset()
		visible = false
	}
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c == 0x1b:
			n, sgr, col1 := escape(line[i:])
			if sgr && color {
				frame.WriteString(line[i : i+n])
			} else if col1 {
				restart()
			}
			i += n
		case c == '\r':
			restart()
			i++
		case c == '\t':
			frame.WriteByte(c)
			i++
		case c < 0x20 || c == 0x7f:
			i++
		default:
			r, size := utf8.DecodeRuneInString(line[i:])
			i += size
			switch {
			case r >= 0x80 && r <= 0x9f: // C1 controls, such as an 8-bit CSI
				continue
			case r == utf8.RuneError && size == 1:
				frame.WriteRune(utf8.RuneError)
			default:
				frame.WriteRune(r)
			}
			if r != ' ' {
				visible = true
			}
		}
	}
	out := frame.String()
	if !visible && lastVisible {
		out = last.String()
	}
	if strings.Contains(out, "\x1b") {
		out += "\x1b[0m"
	}
	return out
}

// escape measures the escape sequence at the start of s (s[0] is ESC) and
// says whether it is a plain SGR, `ESC [ digits ; : m`, or a move to column 1,
// `ESC [ G` or `ESC [ 1 G`. A string sequence (OSC, DCS, SOS, PM, APC) runs
// to its terminator, or to the end of s when it has none.
func escape(s string) (n int, sgr, col1 bool) {
	if len(s) < 2 {
		return len(s), false, false
	}
	switch s[1] {
	case '[':
		j := 2
		for j < len(s) && s[j] >= 0x30 && s[j] <= 0x3f {
			j++
		}
		params := s[2:j]
		k := j
		for k < len(s) && s[k] >= 0x20 && s[k] <= 0x2f {
			k++
		}
		if k == len(s) || s[k] < 0x40 || s[k] > 0x7e {
			return k, false, false // malformed: drop what was read
		}
		plain := k == j
		sgr = plain && s[k] == 'm' && strings.Trim(params, "0123456789;:") == ""
		col1 = plain && s[k] == 'G' && (params == "" || params == "1")
		return k + 1, sgr, col1
	case ']', 'P', 'X', '^', '_':
		for j := 2; j < len(s); j++ {
			if s[j] == 0x07 {
				return j + 1, false, false
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2, false, false
			}
		}
		return len(s), false, false
	default:
		j := 1
		for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
			j++
		}
		if j < len(s) && s[j] >= 0x30 && s[j] <= 0x7e {
			j++
		}
		return j, false, false
	}
}
