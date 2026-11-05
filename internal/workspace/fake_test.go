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
	}
	ctx := context.Background()
	dir := "/runs/WEB-1"

	if d, err := f.Prepare(ctx, "x/WEB-1", "main"); d != dir || err != nil {
		t.Errorf("Prepare = %q, %v", d, err)
	}
	for _, want := range []bool{true, false} {
		if c, err := f.Commit(ctx, dir, "msg"); c != want || err != nil {
			t.Errorf("Commit = %v, %v; want %v", c, err, want)
		}
	}
	if c, err := f.MergeBase(ctx, dir, "main"); !reflect.DeepEqual(c, []string{"a.go", "b.go"}) || err != nil {
		t.Errorf("MergeBase = %v, %v", c, err)
	}
	if yes, err := f.IsAncestor(ctx, dir, "main", "HEAD"); !yes || err != nil {
		t.Errorf("IsAncestor = %v, %v", yes, err)
	}
	if err := f.Push(ctx, dir, "x/WEB-1"); !errors.Is(err, boom) {
		t.Errorf("Push = %v, want boom", err)
	}

	want := []Call{
		{"Prepare", []any{"x/WEB-1", "main"}},
		{"Commit", []any{dir, "msg"}},
		{"Commit", []any{dir, "msg"}},
		{"MergeBase", []any{dir, "main"}},
		{"IsAncestor", []any{dir, "main", "HEAD"}},
		{"Push", []any{dir, "x/WEB-1"}},
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
		{"Commit", func() (bool, error) { c, err := f.Commit(ctx, "/d", "m"); return !c, err }},
		{"MergeBase", func() (bool, error) { c, err := f.MergeBase(ctx, "/d", "main"); return c == nil, err }},
		{"IsAncestor", func() (bool, error) { y, err := f.IsAncestor(ctx, "/d", "a", "b"); return !y, err }},
		{"Push", func() (bool, error) { return true, f.Push(ctx, "/d", "b") }},
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
