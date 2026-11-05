package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rodo-software/cinderstack-runner/internal/name"
)

func TestRun(t *testing.T) {
	tests := []struct {
		args    []string
		code    int
		stdout  string
		usageOn bool
	}{
		{[]string{"version"}, 0, name.Name + " dev\n", false},
		{[]string{}, 2, "", true},
		{[]string{"nope"}, 2, "", true},
		{[]string{"version", "x"}, 2, "", true},
	}
	for _, tt := range tests {
		var out, errb bytes.Buffer
		if code := run(tt.args, &out, &errb); code != tt.code {
			t.Errorf("%v: code %d, want %d", tt.args, code, tt.code)
		}
		if out.String() != tt.stdout {
			t.Errorf("%v: stdout %q, want %q", tt.args, out.String(), tt.stdout)
		}
		if tt.usageOn && !strings.Contains(errb.String(), "usage:") {
			t.Errorf("%v: stderr %q lacks usage:", tt.args, errb.String())
		}
	}
}
