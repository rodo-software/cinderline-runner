package environment

import (
	"context"
	"slices"
	"testing"
)

// The fake hands its canned output to OnLine a line at a time, as Docker does.
func TestFakeCallsOnLine(t *testing.T) {
	f := &Fake{Runs: []RunReply{{Output: "one\ntwo\nlast"}, {Output: ""}}}
	var got []string
	s := Spec{OnLine: func(l string) { got = append(got, l) }}
	for range 2 {
		if _, err := f.Run(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(got, []string{"one", "two", "last"}) {
		t.Errorf("lines = %q", got)
	}
}
