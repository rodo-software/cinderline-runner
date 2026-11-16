package redact

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"
)

const secret = "sk-test-0123456789abcdef/+=XYZ"

func TestStringEveryForm(t *testing.T) {
	Add("anthropic key", secret)
	forms := map[string]string{
		"plain":       secret,
		"base64":      base64.StdEncoding.EncodeToString([]byte(secret)),
		"base64 url":  base64.URLEncoding.EncodeToString([]byte(secret)),
		"hex":         hex.EncodeToString([]byte(secret)),
		"hex upper":   strings.ToUpper(hex.EncodeToString([]byte(secret))),
		"query":       url.QueryEscape(secret),
		"path":        url.PathEscape(secret),
		"basic auth":  base64.StdEncoding.EncodeToString([]byte("x:" + secret)),
		"basic auth2": base64.StdEncoding.EncodeToString([]byte("ab:" + secret)),
		"basic auth3": base64.StdEncoding.EncodeToString([]byte("abcd:" + secret)),
	}
	for name, f := range forms {
		got := String("before " + f + " after")
		if !strings.Contains(got, "[redacted anthropic key]") {
			t.Errorf("%s: %q not redacted: %q", name, f, got)
		}
		if strings.Contains(got, secret) || strings.Contains(got, f) {
			t.Errorf("%s: value left in %q", name, got)
		}
		if !strings.HasPrefix(got, "before ") {
			t.Errorf("%s: surrounding text lost: %q", name, got)
		}
	}
}

func TestShortValuesSkipped(t *testing.T) {
	Add("git token", "short")
	if got := String("a short word"); got != "a short word" {
		t.Errorf("got %q", got)
	}
	Add("git token", "")
	if got := String("x"); got != "x" {
		t.Errorf("got %q", got)
	}
}

func TestLongestValueWins(t *testing.T) {
	Add("openai key", "prefix-abcdefghijklmnop")
	Add("gemini key", "prefix-abcdefghijklmnop-and-more")
	if got := String("k=prefix-abcdefghijklmnop-and-more"); got != "k=[redacted gemini key]" {
		t.Errorf("got %q", got)
	}
}

func TestHandlerRedactsMessageAttrsErrorsAndGroups(t *testing.T) {
	const v = "ghp_handlertest0123456789"
	Add("git token", v)
	var buf bytes.Buffer
	log := slog.New(Handler(slog.NewJSONHandler(&buf, nil))).With("pre", v)
	log.WithGroup("g").Info("msg "+v, "line", "x"+v, "err", errors.New("failed: "+v), slog.Group("in", "deep", v))
	if strings.Contains(buf.String(), v) || strings.Count(buf.String(), "[redacted git token]") != 5 {
		t.Errorf("log line %s", buf.String())
	}
}
