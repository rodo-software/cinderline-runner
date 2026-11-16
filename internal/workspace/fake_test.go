package workspace

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestFake(t *testing.T) {
	boom := errors.New("boom")
	f := &Fake{
		Prepares:   []PrepareReply{{Dir: "/runs/WEB-1"}},
		Commits:    []CommitReply{{Committed: true}, {Committed: false}},
		MergeBases: []MergeBaseReply{{Conflicts: []string{"a.go", "b.go"}}},
		Ancestors:  []IsAncestorReply{{Yes: true}},
		Pushes:     []error{boom},
		Heads:      []HeadReply{{Commit: "abc123"}},
		Cleans:     []BoolReply{{Yes: true}},
		Markers:    []BoolReply{{Yes: true}},
		Resets:     []error{boom},
		Logs:       []LogReply{{Log: "abc T"}},
	}
	ctx := context.Background()
	dir := "/runs/WEB-1"

	if d, err := f.Prepare(ctx, "x/WEB-1", "main"); d != dir || err != nil {
		t.Errorf("Prepare = %q, %v", d, err)
	}
	for _, want := range []bool{true, false} {
		if c, err := f.Commit(ctx, dir, "T / A", "msg"); c != want || err != nil {
			t.Errorf("Commit = %v, %v; want %v", c, err, want)
		}
	}
	if c, err := f.MergeBase(ctx, dir, "main", "T / A"); !reflect.DeepEqual(c, []string{"a.go", "b.go"}) || err != nil {
		t.Errorf("MergeBase = %v, %v", c, err)
	}
	if yes, err := f.IsAncestor(ctx, dir, "main", "HEAD"); !yes || err != nil {
		t.Errorf("IsAncestor = %v, %v", yes, err)
	}
	if err := f.Push(ctx, dir, "x/WEB-1"); !errors.Is(err, boom) {
		t.Errorf("Push = %v, want boom", err)
	}
	if c, err := f.Head(ctx, dir); c != "abc123" || err != nil {
		t.Errorf("Head = %q, %v", c, err)
	}
	if yes, err := f.Clean(ctx, dir); !yes || err != nil {
		t.Errorf("Clean = %v, %v", yes, err)
	}
	if yes, err := f.ConflictMarkers(ctx, dir, []string{"a.go"}); !yes || err != nil {
		t.Errorf("ConflictMarkers = %v, %v", yes, err)
	}
	if err := f.Reset(ctx, dir, "abc123"); !errors.Is(err, boom) {
		t.Errorf("Reset = %v, want boom", err)
	}
	if l, err := f.Log(ctx, dir, "main"); l != "abc T" || err != nil {
		t.Errorf("Log = %q, %v", l, err)
	}

	want := []Call{
		{"Prepare", []any{"x/WEB-1", "main"}},
		{"Commit", []any{dir, "T / A", "msg"}},
		{"Commit", []any{dir, "T / A", "msg"}},
		{"MergeBase", []any{dir, "main", "T / A"}},
		{"IsAncestor", []any{dir, "main", "HEAD"}},
		{"Push", []any{dir, "x/WEB-1"}},
		{"Head", []any{dir}},
		{"Clean", []any{dir}},
		{"ConflictMarkers", []any{dir, []string{"a.go"}}},
		{"Reset", []any{dir, "abc123"}},
		{"Log", []any{dir, "main"}},
	}
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
		{"Prepare", func() (bool, error) { d, err := f.Prepare(ctx, "b", "main"); return d == "", err }},
		{"Commit", func() (bool, error) { c, err := f.Commit(ctx, "/d", "T / A", "m"); return !c, err }},
		{"MergeBase", func() (bool, error) { c, err := f.MergeBase(ctx, "/d", "main", "T / A"); return c == nil, err }},
		{"IsAncestor", func() (bool, error) { y, err := f.IsAncestor(ctx, "/d", "a", "b"); return !y, err }},
		{"Push", func() (bool, error) { return true, f.Push(ctx, "/d", "b") }},
		{"Head", func() (bool, error) { c, err := f.Head(ctx, "/d"); return c == "", err }},
		{"Clean", func() (bool, error) { y, err := f.Clean(ctx, "/d"); return !y, err }},
		{"ConflictMarkers", func() (bool, error) { y, err := f.ConflictMarkers(ctx, "/d", nil); return !y, err }},
		{"Reset", func() (bool, error) { return true, f.Reset(ctx, "/d", "c") }},
		{"Log", func() (bool, error) { l, err := f.Log(ctx, "/d", "main"); return l == "", err }},
		{"FastForward", func() (bool, error) { return true, f.FastForward(ctx, "/d", "b") }},
		{"Remove", func() (bool, error) { return true, f.Remove(ctx, "/d") }},
		{"Free", func() (bool, error) { n, err := f.Free(ctx); return n == 0, err }},
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
