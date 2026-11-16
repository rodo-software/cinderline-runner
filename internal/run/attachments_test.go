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

// attachedIssue is CSR-7 with three files: two share a name, one tries to
// leave the folder.
const attachedIssue = `{"ref":"CSR-7","title":"Add hello","body":"Add a file.","status":{"value":"in_progress","label":"x"},"team":{"id":1,"name":"Dev"},"sub_issues":[],
	"attachments":[{"id":1,"name":"spec.pdf","size":2048},{"id":2,"name":"SPEC.pdf","size":10},{"id":3,"name":"../../etc/passwd","size":5}]}`

// A run downloads its issue's files into the attachments folder before the
// first turn, each under a safe, unique name, and the prompt lists them
// (CSR-173). Prune empties the folder for the next run.
func TestARunGetsItsIssuesAttachments(t *testing.T) {
	p, cl, api, _ := linkedRun(t, appLink, 1)
	api.routes["GET /issues/CSR-7"] = reply{200, attachedIssue}
	for id, body := range map[string]string{"1": "one", "2": "two", "3": "three"} {
		api.routes["GET /issues/CSR-7/attachments/"+id] = reply{200, body}
	}
	runCSR7(t, p, api)

	dir := attachmentsDir(p.State, cfg)
	want := map[string]string{"spec.pdf": "one", "2-SPEC.pdf": "two", ".._.._etc_passwd": "three"}
	entries, _ := os.ReadDir(dir)
	if len(entries) != len(want) {
		t.Errorf("folder holds %d files, want %d", len(entries), len(want))
	}
	for name, body := range want {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != body {
			t.Errorf("%s = %q, %v; want %q", name, got, err, body)
		}
	}
	prompt := cl.Calls()[0].Args[0].(client.Turn).Prompt
	for _, line := range []string{"- `/attachments/spec.pdf` (2 KB)", "- `/attachments/2-SPEC.pdf` (10 bytes)", "- `/attachments/.._.._etc_passwd` (5 bytes)"} {
		if !strings.HasSuffix(prompt, "\n") || !strings.Contains(prompt[strings.Index(prompt, "## Issue CSR-7"):], line) {
			t.Errorf("prompt lacks %q after the issue text:\n%s", line, prompt)
		}
	}

	if err := p.Prune(context.Background(), api.client(t)); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("after Prune the folder holds %d files, %v", len(entries), err)
	}
}

// An issue without attachments (or from an app before EMBER-208) gets no
// section and an empty folder: nothing of the last run is left.
func TestARunWithoutAttachments(t *testing.T) {
	p, cl, api, _ := linkedRun(t, appLink, 1)
	dir := attachmentsDir(p.State, cfg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	runCSR7(t, p, api)
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("folder holds %d files, %v", len(entries), err)
	}
	if prompt := cl.Calls()[0].Args[0].(client.Turn).Prompt; strings.Contains(prompt, "/attachments") {
		t.Errorf("prompt names attachments:\n%s", prompt)
	}
}

// A file that cannot be downloaded stops the run before its first turn, with
// a progress line, as a failed environment does.
func TestAnAttachmentThatFailsStopsTheRun(t *testing.T) {
	p, cl, api, _ := linkedRun(t, appLink, 1)
	api.routes["GET /issues/CSR-7"] = reply{200, attachedIssue} // no download routes: 404
	h := p.Hooks()
	err := h.Run(context.Background(), api.client(t), cinderline.Issue{Ref: "CSR-7", Team: &cinderline.IssueTeam{ID: 1}})
	if err == nil {
		t.Fatal("no error")
	}
	if len(cl.Calls()) != 0 {
		t.Error("an agent ran without its files")
	}
	if got := progressLines(api); !strings.Contains(got, "attachments could not be downloaded") {
		t.Errorf("progress = %s", got)
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
