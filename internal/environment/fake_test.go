package environment

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestFake(t *testing.T) {
	boom := errors.New("boom")
	spec := Spec{Project: "WEB", Issue: "WEB-1", Image: "default", Dir: "/runs/WEB-1", Env: []string{"DB_HOST=db"}}
	f := &Fake{
		Starts: []StartReply{{ID: "c1"}},
		Stops:  []error{boom},
		Tails:  []TailReply{{Tail: "last lines"}},
	}
	ctx := context.Background()

	if id, err := f.Start(ctx, spec); id != "c1" || err != nil {
		t.Errorf("Start = %q, %v", id, err)
	}
	if tail, err := f.Tail(ctx, "c1"); tail != "last lines" || err != nil {
		t.Errorf("Tail = %q, %v", tail, err)
	}
	if err := f.Stop(ctx, "c1"); !errors.Is(err, boom) {
		t.Errorf("Stop = %v, want boom", err)
	}

	want := []Call{{"Start", []any{spec}}, {"Tail", []any{"c1"}}, {"Stop", []any{"c1"}}}
	if calls := f.Calls(); !reflect.DeepEqual(calls, want) {
		t.Errorf("Calls() = %+v, want %+v", calls, want)
	}
}

func TestFakeEmptyQueues(t *testing.T) {
	f := &Fake{}
	ctx := context.Background()
	tests := []struct {
		method string
		call   func() (zero bool, err error)
	}{
		{"Start", func() (bool, error) { id, err := f.Start(ctx, Spec{}); return id == "", err }},
		{"Stop", func() (bool, error) { return true, f.Stop(ctx, "c1") }},
		{"Tail", func() (bool, error) { s, err := f.Tail(ctx, "c1"); return s == "", err }},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			zero, err := tt.call()
			if !zero || !errors.Is(err, ErrNotScripted) {
				t.Errorf("zero = %v, err = %v; want zero value and ErrNotScripted", zero, err)
			}
		})
	}
}
