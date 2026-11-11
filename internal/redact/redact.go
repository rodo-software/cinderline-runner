// Package redact replaces the secrets the runner knows with a marker in text
// that leaves it: the log, comments, progress lines, report errors and pull
// requests (CSR-157). secrets.Get and the agent tokens Cinderline issues
// register their values here, so no caller has to remember to.
//
// A second line of defence against accidents and lazy prompt injection, not
// against a determined agent: decision 2026-11-11-secret-redaction.
package redact

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// MinLen is the shortest value redacted: a shorter one could match ordinary text.
const MinLen = 16

var (
	mu       sync.Mutex
	patterns = map[string]string{} // encoded form → marker
	replacer atomic.Pointer[strings.Replacer]
)

// Add registers value under label, such as "anthropic key", with its base64,
// hex and URL-encoded forms. Values shorter than MinLen are ignored.
// shortcut: values are never dropped, so a daemon grows by a few agent tokens
// per run; fine for months of runs, add expiry if memory ever shows it.
func Add(label, value string) {
	if len(value) < MinLen {
		return
	}
	marker := "[redacted " + label + "]"
	mu.Lock()
	defer mu.Unlock()
	if _, ok := patterns[value]; ok {
		return
	}
	for _, f := range forms(value) {
		patterns[f] = marker
	}
	keys := make([]string, 0, len(patterns))
	for k := range patterns {
		keys = append(keys, k)
	}
	// strings.Replacer tries patterns in argument order: longest first, so a
	// value that contains another is replaced whole.
	slices.SortFunc(keys, func(a, b string) int { return len(b) - len(a) })
	pairs := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		pairs = append(pairs, k, patterns[k])
	}
	replacer.Store(strings.NewReplacer(pairs...))
}

// forms is value as it may appear in text: as is, URL-encoded, hex and base64.
// Base64 is taken at the three byte alignments, so the value is also found
// inside a longer encoded string such as a Basic auth header.
func forms(value string) []string {
	b := []byte(value)
	h := hex.EncodeToString(b)
	out := []string{value, url.QueryEscape(value), url.PathEscape(value), h, strings.ToUpper(h)}
	for k := range 3 {
		s := base64.RawStdEncoding.EncodeToString(append(make([]byte, k), b...))
		// Drop the characters that hold bits of the k prefix bytes or of
		// whatever follows the value.
		s = s[[]int{0, 2, 3}[k]:]
		if (k+len(b))%3 != 0 {
			s = s[:len(s)-1]
		}
		out = append(out, s, strings.NewReplacer("+", "-", "/", "_").Replace(s))
	}
	return out
}

// String returns s with every registered value replaced by its marker.
func String(s string) string {
	if r := replacer.Load(); r != nil {
		return r.Replace(s)
	}
	return s
}

// Handler wraps h so the message and every string, error and Stringer
// attribute of each record are redacted. The daemon's log goes through it;
// agent output reaches it one complete line at a time, so a value split over
// two writes of the stream is still caught.
func Handler(h slog.Handler) slog.Handler { return handler{h} }

type handler struct{ slog.Handler }

func (h handler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, String(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(attr(a))
		return true
	})
	return h.Handler.Handle(ctx, out)
}

func (h handler) WithAttrs(as []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(as))
	for i, a := range as {
		red[i] = attr(a)
	}
	return handler{h.Handler.WithAttrs(red)}
}

func (h handler) WithGroup(name string) slog.Handler { return handler{h.Handler.WithGroup(name)} }

func attr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, String(v.String()))
	case slog.KindGroup:
		g := v.Group()
		red := make([]any, len(g))
		for i, x := range g {
			red[i] = attr(x)
		}
		return slog.Group(a.Key, red...)
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, String(x.Error()))
		case interface{ String() string }:
			return slog.String(a.Key, String(x.String()))
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}
