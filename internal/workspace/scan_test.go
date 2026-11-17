package workspace

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/redact"
)

const (
	knownToken = "find-me-scan-0123456789abcdef"
	knownKey   = "sk-ant-known-0123456789abcdefghij"
	commitA    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commitB    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// scanLog is git log -p as scanArgs asks for it (taken from git's output):
// a commit with a value in its message, a name with a space (ended by a tab)
// and a quoted name, then a merge shown against git's own merge.
var scanLog = "commit " + commitA + `
Author: A <a@example.com>
Date:   Tue Nov 17 14:17:10 2026 +0000

    add the token ` + knownToken + `
    diff --git a/not b/a-header

diff --git a/we ird.txt b/we ird.txt
new file mode 100644
index 0000000..94bf3c1
--- /dev/null
+++ b/we ird.txt` + "\t" + `
@@ -0,0 +1,2 @@
+a
+token = "` + knownToken + `"
diff --git "a/caf\303\251.pem" "b/caf\303\251.pem"
index 1111111..2222222 100644
--- "a/caf\303\251.pem"
+++ "b/caf\303\251.pem"
@@ -1,3 +1,3 @@
 ` + knownToken + `
--` + knownToken + `
+-----BEGIN OPENSSH PRIVATE KEY-----
diff --git a/gone.txt b/gone.txt
deleted file mode 100644
--- a/gone.txt
+++ /dev/null
@@ -1 +0,0 @@
-` + knownToken + `

commit ` + commitB + `
Merge: 13268b7 49d4646
Author: A <a@example.com>
Date:   Tue Nov 17 14:17:25 2026 +0000

    merge main

diff --git a/f b/f
remerge CONFLICT (add/add): Merge conflict in f
index a615c8e..e4854f9 100644
--- a/f
+++ b/f
@@ -1,5 +1,3 @@
-<<<<<<< 13268b7 (mine)
-glpat-ABCDEFGHIJKLMNOPQRSTUV
+resolved glpat-ABCDEFGHIJKLMNOPQRSTUVW
+again glpat-ABCDEFGHIJKLMNOPQRSTUVW
+k = sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAA
+known = ` + knownKey + `
`

func TestScan(t *testing.T) {
	redact.Add("git token", knownToken)
	redact.Add("anthropic key", knownKey)
	want := []Secret{
		{Kind: "git token", Commit: commitA, Path: "", Known: true},
		{Kind: "git token", Commit: commitA, Path: "we ird.txt", Known: true},
		{Kind: "anthropic key", Commit: commitB, Path: "f", Known: true},
		{Kind: "private key block", Commit: commitA, Path: "café.pem"},
		{Kind: "gitlab token", Commit: commitB, Path: "f"},
		{Kind: "anthropic key", Commit: commitB, Path: "f"},
	}
	if got := scan(scanLog); !reflect.DeepEqual(got, want) {
		t.Errorf("scan =\n%+v\nwant\n%+v", got, want)
	}
	if got := scan(""); got != nil {
		t.Errorf("scan of no commits = %+v", got)
	}
}

func TestScanShapes(t *testing.T) {
	for _, c := range []struct{ line, kind string }{
		{"-----BEGIN RSA PRIVATE KEY-----", "private key block"},
		{"sk-or-v1-" + strings.Repeat("0a", 32), "openrouter key"},
		{"OPENAI_API_KEY=sk-proj-abcdefghijklmnopqrstuvwxyz", "openai key"},
		{"ghp_" + strings.Repeat("A", 36), "github token"},
		{"AKIAABCDEFGHIJKLMNOP", "aws access key"},
		{"AIza" + strings.Repeat("b", 35), "google api key"},
		{"xoxb-1234567890-abcdef", "slack token"},
		{"a sentence about sk-ant- prefixes and -----BEGIN PUBLIC KEY----- blocks", ""},
	} {
		line, kind := c.line, c.kind
		got := scan("commit " + commitA + "\n\n    " + line + "\n")
		if kind == "" {
			if got != nil {
				t.Errorf("%q: found %+v", line, got)
			}
			continue
		}
		if len(got) != 1 || got[0].Kind != kind || got[0].Known {
			t.Errorf("%q: found %+v, want a %s warning", line, got, kind)
		}
	}
}

// logRecorder is recorder whose git log answers with log.
func logRecorder(log string) (*Docker, *[]execCall) {
	d, calls := recorder(nil)
	d.Exec = func(_ context.Context, env []string, argv ...string) (string, error) {
		*calls = append(*calls, execCall{env, argv})
		if slices.Contains(argv, "log") {
			return log, nil
		}
		return "", nil
	}
	return d, calls
}

func TestDockerPushStopsAtKnownSecret(t *testing.T) {
	redact.Add("git token", knownToken)
	d, calls := logRecorder(scanLog)
	found, err := d.Push(context.Background(), "/runs/CSR-29", "cinderline/CSR-29")
	if !errors.Is(err, ErrSecret) {
		t.Fatalf("Push = %v, want ErrSecret", err)
	}
	if len(*calls) != 2 {
		t.Errorf("ran %d commands, want the fetch and the scan only: nothing pushed, the clone untouched", len(*calls))
	}
	if !hasKnown(found) || strings.Contains(err.Error(), knownToken) {
		t.Errorf("found %+v, error %q: want the known secrets and no value", found, err)
	}
	if !strings.Contains(err.Error(), "git token in "+commitA[:12]+" we ird.txt") {
		t.Errorf("error %q does not name the kind and place", err)
	}
}

func TestDockerPushWarnsAndPushes(t *testing.T) {
	d, calls := logRecorder("commit " + commitA + "\n\n    -----BEGIN EC PRIVATE KEY-----\n")
	found, err := d.Push(context.Background(), "/runs/CSR-29", "cinderline/CSR-29")
	if err != nil {
		t.Fatal(err)
	}
	if want := []Secret{{Kind: "private key block", Commit: commitA}}; !reflect.DeepEqual(found, want) {
		t.Errorf("found %+v, want %+v", found, want)
	}
	if len(*calls) != 4 {
		t.Errorf("ran %d commands, want fetch, scan, push and update-ref", len(*calls))
	}
}
