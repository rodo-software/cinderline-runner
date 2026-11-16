package client

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

func TestFakeRun(t *testing.T) {
	boom := errors.New("boom")
	f := &Fake{Runs: []RunReply{
		{Result: Result{SessionID: "s1", HitTurnLimit: true, Tail: "…limit"}},
		{Result: Result{SessionID: "s1", Report: "done", Tail: "out"}, Err: boom},
	}}
	ctx := context.Background()
	first := Turn{Dir: "/w", Prompt: "do it", MaxTurns: 50}
	second := Turn{Dir: "/w", Prompt: "go on", SessionID: "s1", MaxTurns: 50}

	got, err := f.Run(ctx, first)
	if err != nil || got != (Result{SessionID: "s1", HitTurnLimit: true, Tail: "…limit"}) {
		t.Errorf("first Run = %+v, %v", got, err)
	}
	got, err = f.Run(ctx, second)
	if !errors.Is(err, boom) || got.Tail != "out" || got.Report != "done" {
		t.Errorf("second Run = %+v, %v; want result and boom", got, err)
	}
	got, err = f.Run(ctx, first)
	if !errors.Is(err, ErrNotScripted) || got != (Result{}) {
		t.Errorf("Run on empty queue = %+v, %v; want zero and ErrNotScripted", got, err)
	}

	want := []Call{{"Run", []any{first}}, {"Run", []any{second}}, {"Run", []any{first}}}
	if calls := f.Calls(); !reflect.DeepEqual(calls, want) {
		t.Errorf("Calls() = %+v, want %+v", calls, want)
	}
}

func TestFakeConcurrent(t *testing.T) {
	const n = 20
	f := &Fake{Runs: make([]RunReply, n)}
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if _, err := f.Run(context.Background(), Turn{}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := len(f.Calls()); got != n {
		t.Errorf("recorded %d calls, want %d", got, n)
	}
}
