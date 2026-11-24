package run

import (
	"os"
	"testing"
)

// A note is written whole, replacing the one before, readable by this user
// only, and leaves nothing else in its folder (CLR-234).
func TestSaveNoteReplacesItAtomically(t *testing.T) {
	p := newFixture(t).p
	p.saveNote("CLR-1", Note{Session: "old"})
	p.saveNote("CLR-1", Note{Session: "new"})
	if n, ok := p.readNote("CLR-1"); !ok || n.Session != "new" {
		t.Fatalf("note = %+v, %v; want session new", n, ok)
	}
	entries, err := os.ReadDir(PausedDir(p.State, p.Config))
	if err != nil || len(entries) != 1 {
		t.Fatalf("paused folder = %v, %v; want the note only", entries, err)
	}
	if fi, err := os.Stat(p.noteFile("CLR-1")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("note mode = %v, %v; want 0600", fi, err)
	}
}
