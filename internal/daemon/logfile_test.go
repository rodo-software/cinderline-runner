package daemon

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestRotation(t *testing.T) {
	path := t.TempDir() + "/sub/log"
	l := &logFile{path: path, max: 100}
	defer l.Close()
	for i := range 51 {
		if _, err := fmt.Fprintf(l, "line %04d %s\n", i, strings.Repeat("x", 20)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Errorf("more than 3 old files: %v", err)
	}
	seen := 0
	for _, f := range []string{path + ".3", path + ".2", path + ".1", path} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > 100 {
			t.Errorf("%s is %d bytes, over the limit", f, len(raw))
		}
		for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
			if len(line) != 30 || !strings.HasPrefix(line, "line ") {
				t.Errorf("%s has a split line %q", f, line)
			}
			seen++
		}
	}
	if seen != 4*3 { // three 30-byte lines fit in 100 bytes
		t.Errorf("the four files hold %d lines, want 12", seen)
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Errorf("log mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestTailAndFollow(t *testing.T) {
	l := &logFile{path: t.TempDir() + "/log", max: 1 << 20}
	defer l.Close()
	for i := range 5 {
		fmt.Fprintf(l, "%d\n", i)
	}
	tail, ch, cancel, err := l.follow(2)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if string(tail) != "3\n4\n" {
		t.Errorf("tail = %q", tail)
	}
	fmt.Fprintf(l, "5\n")
	if got := string(<-ch); got != "5\n" {
		t.Errorf("followed %q", got)
	}
	// A subscriber that does not read is dropped, and logging goes on.
	for i := range 2 * subscriberBuffer {
		if _, err := fmt.Fprintf(l, "%d\n", i); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for range ch {
		n++
	}
	if n != subscriberBuffer {
		t.Errorf("a slow subscriber got %d lines before it was dropped, want %d", n, subscriberBuffer)
	}
}
