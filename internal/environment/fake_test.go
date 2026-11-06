package environment

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestFake(t *testing.T) {
	boom := errors.New("boom")
	spec := Spec{Issue: "WEB-1", Dir: "/runs/WEB-1", Cmd: []string{"claude"}, Env: []string{"ANTHROPIC_API_KEY=k"}}
	f := &Fake{Runs: []RunReply{{Output: "out"}}, Cleanups: []error{boom}}
	ctx := context.Background()

	if out, err := f.Run(ctx, spec); out != "out" || err != nil {
		t.Errorf("Run = %q, %v", out, err)
	}
	if err := f.Cleanup(ctx); !errors.Is(err, boom) {
		t.Errorf("Cleanup = %v, want boom", err)
	}
	want := []Call{{"Run", []any{spec}}, {"Cleanup", nil}}
	if calls := f.Calls(); !reflect.DeepEqual(calls, want) {
		t.Errorf("Calls() = %+v, want %+v", calls, want)
	}
}

func TestFakeEmptyQueues(t *testing.T) {
	f := &Fake{}
	ctx := context.Background()
	if out, err := f.Run(ctx, Spec{}); out != "" || !errors.Is(err, ErrNotScripted) {
		t.Errorf("Run = %q, %v; want ErrNotScripted", out, err)
	}
	if err := f.Cleanup(ctx); !errors.Is(err, ErrNotScripted) {
		t.Errorf("Cleanup = %v, want ErrNotScripted", err)
	}
}
