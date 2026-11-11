package run

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// pruneSetup writes a note per ref and answers each issue's status.
func pruneSetup(t *testing.T, p Project, notes map[string]Paused, status map[string]string) *fakeAPI {
	t.Helper()
	routes := map[string]reply{}
	for ref, n := range notes {
		if err := p.SavePaused(ref, n); err != nil {
			t.Fatal(err)
		}
		routes["GET /issues/"+ref] = reply{200, issueJSON(ref, status[ref])}
	}
	return &fakeAPI{routes: routes}
}

func kept(ref string, age time.Duration) Paused {
	return Paused{Dir: "/runs/" + ref, Session: "ses-" + ref, Client: "claude", Kept: time.Now().Add(-age)}
}

// Age pruning, then disk-space pruning oldest first, delete only sessions
// kept after a finished run whose issue is not To do, In progress or
// Blocked; a closed issue loses everything; a clone with unpushed commits
// stays (CSR-145).
func TestPruneDeletesOnlyInactiveKeptSessions(t *testing.T) {
	const day = 24 * time.Hour
	p := project(t)
	p.KeepFor, p.MinFree = 7*day, 50
	var deleted []string
	p.deleteSession = func(_ context.Context, ref, session string) error {
		deleted = append(deleted, ref+" "+session)
		return nil
	}
	paused := Paused{Dir: "/runs/CSR-4", Session: "ses-CSR-4", Client: "claude"}
	notes := map[string]Paused{
		"CSR-1": kept("CSR-1", 10*day),                                        // over the age limit
		"CSR-2": kept("CSR-2", 3*day),                                         // the oldest within it: pruned for disk space
		"CSR-3": kept("CSR-3", 1*day),                                         // survives: space is back after CSR-2
		"CSR-4": paused,                                                       // waits for an answer
		"CSR-5": kept("CSR-5", 30*day),                                        // its follow-up runs now
		"CSR-6": kept("CSR-6", 30*day),                                        // its follow-up is about to resume
		"CSR-8": {Dir: "/runs/CSR-8", Session: "ses-CSR-8", Client: "claude"}, // a person closed it
		"CSR-9": kept("CSR-9", 20*day),                                        // has unpushed commits
	}
	status := map[string]string{"CSR-1": "review", "CSR-2": "review", "CSR-3": "backlog", "CSR-4": "blocked", "CSR-5": "in_progress", "CSR-6": "todo", "CSR-8": "done", "CSR-9": "review"}
	api := pruneSetup(t, p, notes, status)
	ws := &workspace.Fake{
		Strandeds: []workspace.StrandedReply{{Clones: []workspace.Clone{{Dir: "/runs/CSR-9", Branch: "cinderline/CSR-9"}}}},
		Removes:   []error{nil, nil, nil},
		// Before and after each removal (CSR-1, CSR-8, CSR-2), and the checks of the disk loop.
		Frees: []workspace.FreeReply{{Bytes: 1}, {Bytes: 1}, {Bytes: 1}, {Bytes: 1}, {Bytes: 1}, {Bytes: 1}, {Bytes: 100}, {Bytes: 100}},
	}
	p.Workspace = ws

	if err := p.Prune(context.Background(), api.client(t)); err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, c := range ws.Calls() {
		if c.Method == "Remove" {
			removed = append(removed, c.Args[0].(string))
		}
	}
	if want := []string{"/runs/CSR-1", "/runs/CSR-8", "/runs/CSR-2"}; !slices.Equal(removed, want) {
		t.Errorf("removed %q, want %q", removed, want)
	}
	if want := []string{"CSR-1 ses-CSR-1", "CSR-8 ses-CSR-8", "CSR-2 ses-CSR-2"}; !slices.Equal(deleted, want) {
		t.Errorf("sessions deleted %q, want %q", deleted, want)
	}
	for ref, want := range map[string]Paused{
		"CSR-1": {Pruned: prunedAge}, "CSR-2": {Pruned: prunedDisk},
		"CSR-3": notes["CSR-3"], "CSR-4": paused, "CSR-5": notes["CSR-5"], "CSR-6": notes["CSR-6"], "CSR-9": notes["CSR-9"],
	} {
		got, _ := p.readPaused(ref)
		if got.Dir != want.Dir || got.Session != want.Session || got.Pruned != want.Pruned || !got.Kept.Equal(want.Kept) {
			t.Errorf("%s note = %+v, want %+v", ref, got, want)
		}
	}
	if _, ok := p.readPaused("CSR-8"); ok {
		t.Error("the closed issue's note is still there")
	}
	if len(ws.Frees) != 0 {
		t.Errorf("%d free checks left, want none", len(ws.Frees))
	}
}

// With nothing left to delete and free space still below the minimum, the
// log says so and the run still starts; with min_free_disk off, free space
// is never checked.
func TestPruneLogsWhenSpaceStaysLow(t *testing.T) {
	var logs bytes.Buffer
	p := project(t)
	p.Log = slog.New(slog.NewTextHandler(&logs, nil))
	p.MinFree = 50
	ws := &workspace.Fake{Frees: []workspace.FreeReply{{Bytes: 10}}}
	p.Workspace = ws
	if err := p.Prune(context.Background(), (&fakeAPI{}).client(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "free disk space below min_free_disk") {
		t.Errorf("log = %q, want the low space warning", logs.String())
	}

	p.MinFree = 0
	ws = &workspace.Fake{}
	p.Workspace = ws
	if err := p.Prune(context.Background(), (&fakeAPI{}).client(t)); err != nil {
		t.Fatal(err)
	}
	if calls := ws.Calls(); len(calls) != 0 {
		t.Errorf("workspace calls %+v, want none with min_free_disk off", calls)
	}
}

// A run prunes before it starts.
func TestARunPrunesFirst(t *testing.T) {
	p := project(t)
	p.KeepFor = time.Hour
	api := pruneSetup(t, p, map[string]Paused{"CSR-1": {Kept: time.Now().Add(-2 * time.Hour)}}, map[string]string{"CSR-1": "review"})
	api.routes["GET /teams/1"] = reply{500, `{}`} // stop right after the prune
	h := p.Hooks()
	if err := h.Run(context.Background(), api.client(t), issueWithTeam("CSR-7")); err == nil {
		t.Fatal("Run succeeded without its team")
	}
	if n, _ := p.readPaused("CSR-1"); n.Pruned != prunedAge {
		t.Fatalf("note = %+v, want pruned for age", n)
	}
}

func issueWithTeam(ref string) cinderline.Issue {
	return cinderline.Issue{Ref: ref, Team: &cinderline.IssueTeam{ID: 1}}
}
