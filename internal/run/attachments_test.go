package run

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
)

// attachedRun is a solo run of CSR-7 whose first step gives the agent files.
func attachedRun(t *testing.T, files string) *stepRun {
	t.Helper()
	r := newStepRun(t,
		`{"kind":"run","agent":"Coder","session":"new","token":"none","prompt":"Do CSR-7.","progress":null,"issue":{"ref":"CSR-7","title":"Add hello"},"attachments":`+files+`}`,
		`{"kind":"fail","reason":"attempts_failed","comment":"Stopped."}`,
	)
	r.cl.Runs = []client.RunReply{{Result: client.Result{SessionID: "s1"}}}
	return r
}

// A run step downloads the files it names into the emptied attachments
// folder before its turn (CSR-173, decision 2026-11-15-server-decides-steps
// point 4). Prune empties the folder for the next run.
func TestARunGetsItsStepsAttachments(t *testing.T) {
	r := attachedRun(t, `[{"issue":"CSR-7","id":5,"name":"spec.pdf"},{"issue":"CSR-7","id":6,"name":"6-SPEC.pdf"}]`)
	r.api.routes["GET /issues/CSR-7/attachments/6"] = reply{200, "two"}
	dir := attachmentsDir(r.p.State, cfg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.run(t); err == nil || err.Error() != "Stopped." {
		t.Fatalf("run = %v, want the fail step's error", err)
	}
	want := map[string]string{"spec.pdf": "PDF", "6-SPEC.pdf": "two"}
	entries, _ := os.ReadDir(dir)
	if len(entries) != len(want) {
		t.Errorf("folder holds %d files, want %d", len(entries), len(want))
	}
	for name, body := range want {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != body {
			t.Errorf("%s = %q, %v; want %q", name, got, err, body)
		}
	}
	if err := r.p.Prune(context.Background(), r.api.client(t)); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("after Prune the folder holds %d files, %v", len(entries), err)
	}
}

// A run starts with an empty folder: nothing of another issue's run is left
// for a step that leaves the folder as it is (attachments null).
func TestARunWithoutAttachments(t *testing.T) {
	r := attachedRun(t, `null`)
	dir := attachmentsDir(r.p.State, cfg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = r.run(t)
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("folder holds %d files, %v", len(entries), err)
	}
	if n := len(r.cl.Calls()); n != 1 {
		t.Errorf("turns = %d, want 1", n)
	}
}

// A file that cannot be downloaded, a name that is not one safe file name,
// two names that differ only in case, or a file of another issue: the turn
// does not start, and next hears why.
func TestAStepsAttachmentThatFailsDoesNotStartTheTurn(t *testing.T) {
	for name, tt := range map[string]struct{ files, err string }{
		"no download":   {`[{"issue":"CSR-7","id":9,"name":"gone.pdf"}]`, "attachment gone.pdf of CSR-7"},
		"a path":        {`[{"issue":"CSR-7","id":5,"name":"../../etc/passwd"}]`, "not one safe, unique file name"},
		"a dot name":    {`[{"issue":"CSR-7","id":5,"name":".."}]`, "not one safe, unique file name"},
		"case twins":    {`[{"issue":"CSR-7","id":5,"name":"a.pdf"},{"issue":"CSR-7","id":6,"name":"A.pdf"}]`, "not one safe, unique file name"},
		"another issue": {`[{"issue":"CSR-8","id":5,"name":"a.pdf"}]`, "outside CSR-7"},
	} {
		t.Run(name, func(t *testing.T) {
			r := attachedRun(t, tt.files)
			_ = r.run(t)
			if len(r.cl.Calls()) != 0 {
				t.Error("an agent ran without its files")
			}
			reqs := nextRequests(t, r.api)
			if len(reqs) != 2 || reqs[1].Last.Outcome != "not_started" || !strings.Contains(deref(reqs[1].Last.Error), tt.err) {
				t.Fatalf("requests = %+v, want the second with not_started and %q", reqs, tt.err)
			}
			if entries, _ := os.ReadDir(filepath.Dir(attachmentsDir(r.p.State, cfg))); len(entries) > 1 {
				t.Errorf("something was written beside the attachments folder: %v", entries)
			}
		})
	}
}

// A sub-issue's files come after its parent's, which keep their names.
func TestASubIssueGetsItsParentsAttachmentsToo(t *testing.T) {
	p := project(t)
	api := &fakeAPI{routes: map[string]reply{
		"GET /issues/CSR-1":                 {200, `{"ref":"CSR-1","title":"P","status":{"value":"in_progress","label":"x"},"attachments":[{"id":1,"name":"a.txt","size":1}]}`},
		"GET /issues/CSR-1/attachments/1":   {200, "p"},
		"GET /issues/CSR-1.1/attachments/2": {200, "s"},
	}}
	sub := cinderline.IssueWithComments{Issue: cinderline.Issue{Ref: "CSR-1.1", Parent: &cinderline.IssueSummary{Ref: "CSR-1"},
		Attachments: []cinderline.Attachment{{ID: 2, Name: "a.txt", Size: 1}}}}
	files, err := p.attachments(context.Background(), api.client(t), sub)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Attached{{"a.txt", 1}, {"2-a.txt", 1}}; !slices.Equal(files, want) {
		t.Errorf("files = %v, want %v", files, want)
	}
	if got, _ := os.ReadFile(filepath.Join(attachmentsDir(p.State, cfg), "2-a.txt")); string(got) != "s" {
		t.Errorf("2-a.txt = %q", got)
	}
	if s := WorkerPrompt("Code.", "CSR-1.1", "CSR-1", "Part", "", "b", "", false, files); !strings.HasSuffix(s, "- `/attachments/2-a.txt` (1 bytes)\n") {
		t.Errorf("WorkerPrompt does not end with the files:\n%s", s)
	}
}

func TestFileName(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf": "report.pdf",
		"a/b\\c":     "a_b_c",
		"..":         "attachment-9",
		" . ":        "attachment-9",
		"x`\ny:z":    "x__y_z",
		"":           "attachment-9",
	} {
		if got := fileName(cinderline.Attachment{ID: 9, Name: in}); got != want {
			t.Errorf("fileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestByteSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 bytes", 1023: "1023 bytes", 1024: "1 KB", 1536: "2 KB", 1 << 20: "1.0 MB", 10_400_000: "9.9 MB"} {
		if got := byteSize(n); got != want {
			t.Errorf("byteSize(%d) = %q, want %q", n, got, want)
		}
	}
}
