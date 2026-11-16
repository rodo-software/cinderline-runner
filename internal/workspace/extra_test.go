package workspace

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// Each repository is fetched in its own container that gets only its token,
// by name, never in argv; a failure is reported for that repository alone.
func TestFetchExtras(t *testing.T) {
	var calls []execCall
	x := func(_ context.Context, env []string, argv ...string) (string, error) {
		calls = append(calls, execCall{env, argv})
		if slices.Contains(argv, "https://git.example.com/o/bad") {
			return "", errors.New("fetch failed")
		}
		return "", nil
	}
	failed := FetchExtras(context.Background(), x, "CSR", ExtraPath, []Extra{
		{Name: "lib", URL: "https://git.example.com/o/lib", Ref: "v1.0", Token: "tok-lib"},
		{Name: "bad", URL: "https://git.example.com/o/bad", Token: "tok-bad"},
	})
	if len(failed) != 1 || failed["bad"] == nil {
		t.Fatalf("failed = %v, want only bad", failed)
	}
	if len(calls) != 4 {
		t.Fatalf("%d calls, want volume create, prune and two fetches: %q", len(calls), calls)
	}
	if got := strings.Join(calls[0].Argv, " "); got != "docker volume create --label cinderline.project=CSR cinderline-CSR-extra" {
		t.Errorf("volume: %s", got)
	}
	prune := calls[1]
	if !slices.Contains(prune.Argv, "none") || len(prune.Env) != 0 || !slices.Equal(prune.Argv[len(prune.Argv)-2:], []string{"lib", "bad"}) {
		t.Errorf("prune: %q %q", prune.Env, prune.Argv)
	}
	for i, want := range []struct{ token, url, ref string }{{"tok-lib", "https://git.example.com/o/lib", "v1.0"}, {"tok-bad", "https://git.example.com/o/bad", "HEAD"}} {
		c := calls[2+i]
		if !slices.Equal(c.Env, []string{"GIT_TOKEN=" + want.token}) {
			t.Errorf("fetch %d env %q", i, c.Env)
		}
		if strings.Contains(strings.Join(c.Argv, " "), "tok-") {
			t.Errorf("fetch %d has a token in argv: %q", i, c.Argv)
		}
		if !slices.Equal(c.Argv[len(c.Argv)-2:], []string{want.url, want.ref}) || !slices.Contains(c.Argv, "cinderline-CSR-extra:"+ExtraPath) {
			t.Errorf("fetch %d argv %q", i, c.Argv)
		}
	}
}

func TestFetchExtrasVolumeFailureFailsAll(t *testing.T) {
	x := func(context.Context, []string, ...string) (string, error) { return "", errors.New("no docker") }
	failed := FetchExtras(context.Background(), x, "CSR", ExtraPath, []Extra{{Name: "a"}, {Name: "b"}})
	if len(failed) != 2 {
		t.Fatalf("failed = %v", failed)
	}
}

func TestLsRemote(t *testing.T) {
	for _, tt := range []struct {
		ref  string
		want []string
	}{
		{"", []string{"HEAD"}},
		{"main", []string{"refs/heads/main", "refs/tags/main"}},
	} {
		var call execCall
		x := func(_ context.Context, env []string, argv ...string) (string, error) {
			call = execCall{env, argv}
			return "", nil
		}
		if err := LsRemote(context.Background(), x, "CSR", "https://h/o/r", tt.ref, "tok"); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(call.Env, []string{"GIT_TOKEN=tok"}) || !slices.Equal(call.Argv[len(call.Argv)-len(tt.want)-1:], append([]string{"https://h/o/r"}, tt.want...)) {
			t.Errorf("ref %q: %q %q", tt.ref, call.Env, call.Argv)
		}
	}
}

func TestExtraMountIsReadOnly(t *testing.T) {
	if got := strings.Join(ExtraMount("CSR", ExtraPath), " "); got != "-v cinderline-CSR-extra:/extra:ro" {
		t.Errorf("ExtraMount = %q", got)
	}
	if got := strings.Join(ExtraMount("CSR", LinkedPath), " "); got != "-v cinderline-CSR-linked:/linked:ro" {
		t.Errorf("ExtraMount = %q", got)
	}
}
