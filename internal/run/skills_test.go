package run

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/tools"
)

var commit = strings.Repeat("a", 40)

func pkg(name, url, path string) cinderline.Skill {
	return cinderline.Skill{Name: name, URL: url, Commit: commit, Path: path}
}

// fills records the skill package fetches of a project and answers each
// from errs by URL (none: filled).
type fills struct {
	mu        sync.Mutex
	calls     []string // url@commit:path
	errs      map[string]error
	contents  map[string]tools.SkillContents // by URL; none: an empty package
	downloads int                            // the downloads passed, all fills
}

// withDefaults is d with a valid value for each field it leaves empty.
func withDefaults(d cinderline.SkillDownload) cinderline.SkillDownload {
	if d.Arch == "" {
		d.Arch = "amd64"
	}
	if d.Path == "" {
		d.Path = "bin/linux-x64/e"
	}
	if d.URL == "" {
		d.URL = "https://github.com/o/p/releases/download/v1/e-linux-x64"
	}
	if d.SHA256 == "" {
		d.SHA256 = strings.Repeat("a", 64)
	}
	return d
}

func (f *fills) fill(_ context.Context, url, commit, path string, downloads []cinderline.SkillDownload) (string, tools.SkillContents, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, url+"@"+commit+":"+path)
	f.downloads += len(downloads)
	if err := f.errs[url]; err != nil {
		return "", tools.SkillContents{}, err
	}
	return "vol-" + path, f.contents[url], nil
}

// Nothing of a package becomes a path or an argument before it passes the
// checks; one that fails is failed, and the others still load.
func TestSkillChecks(t *testing.T) {
	bad := []cinderline.Skill{
		pkg("Pony", "https://github.com/o/p", ""),
		pkg("a--b", "https://github.com/o/p", ""),
		pkg(strings.Repeat("a", 65), "https://github.com/o/p", ""),
		pkg("p", "http://github.com/o/p", ""),
		pkg("p", "https://u:tok@github.com/o/p", ""),
		pkg("p", "https://github.com/o/p?x", ""),
		pkg("p", "https://github.com/o/p#x", ""),
		pkg("p", "https://github.com/"+strings.Repeat("a", 500), ""),
		pkg("p", "--upload-pack=x", ""),
		{Name: "p", URL: "https://github.com/o/p", Commit: "main"},
		{Name: "p", URL: "https://github.com/o/p", Commit: strings.Repeat("A", 40)},
		{Name: "p", URL: "https://github.com/o/p", Commit: strings.Repeat("a", 39)},
		pkg("p", "https://github.com/o/p", "/etc"),
		pkg("p", "https://github.com/o/p", "../x"),
		pkg("p", "https://github.com/o/p", "a/../../x"),
		pkg("p", "https://github.com/o/p", "a/./b"),
		pkg("p", "https://github.com/o/p", "a/"),
		pkg("p", "https://github.com/o/p", "."),
		pkg("p", "https://github.com/o/p", ".."),
		pkg("p", "https://github.com/o/p", "a,dst=/etc"),
		pkg("p", "https://github.com/o/p", "a b"),
		pkg("p", "https://github.com/o/p", "a\nb"),
		pkg("p", "https://github.com/o/p", strings.Repeat("a", 201)),
	}
	for _, bad1 := range []func(*cinderline.SkillDownload){
		func(d *cinderline.SkillDownload) { d.Arch = "x64" }, func(d *cinderline.SkillDownload) { d.Arch = "" },
		func(d *cinderline.SkillDownload) { d.Path = "../x" }, func(d *cinderline.SkillDownload) { d.Path = "/etc/x" },
		func(d *cinderline.SkillDownload) { d.Path = "a/../../x" }, func(d *cinderline.SkillDownload) { d.Path = "" },
		func(d *cinderline.SkillDownload) { d.Path = "a,dst=/etc" },
		func(d *cinderline.SkillDownload) { d.URL = "http://example.com/e" }, func(d *cinderline.SkillDownload) { d.URL = "https://u:p@example.com/e" },
		func(d *cinderline.SkillDownload) { d.URL = "file:///etc/passwd" }, func(d *cinderline.SkillDownload) { d.URL = "" },
		func(d *cinderline.SkillDownload) { d.SHA256 = "" }, func(d *cinderline.SkillDownload) { d.SHA256 = strings.Repeat("A", 64) },
		func(d *cinderline.SkillDownload) { d.SHA256 = strings.Repeat("a", 63) },
	} {
		d := withDefaults(cinderline.SkillDownload{})
		bad1(&d)
		s := pkg("p", "https://github.com/o/p", "")
		s.Downloads = []cinderline.SkillDownload{d}
		bad = append(bad, s)
	}
	tooMany := pkg("p", "https://github.com/o/p", "")
	tooMany.Downloads = slices.Repeat([]cinderline.SkillDownload{withDefaults(cinderline.SkillDownload{})}, 11)
	bad = append(bad, tooMany)
	for _, s := range bad {
		if checkSkill(s) == nil {
			t.Errorf("%+v passed the check", s)
		}
	}
	good := []cinderline.Skill{
		pkg("impeccable", "https://github.com/o/impeccable", "plugin"),
		pkg("pony-tail-2", "https://git.example.com:8443/o/p.git", ""),
		pkg("p", "https://github.com/o/p", ".claude-plugin/x_y"),
		{Name: "engine", URL: "https://github.com/o/p", Commit: commit, Downloads: slices.Repeat([]cinderline.SkillDownload{withDefaults(cinderline.SkillDownload{})}, 10)},
	}
	for _, s := range good {
		if err := checkSkill(s); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}

	f := newFixture(t)
	fl := &fills{}
	f.p.fillSkill = fl.fill
	bad = []cinderline.Skill{bad[0], bad[4], bad[9], bad[13], bad[19]} // within the job's ten
	loaded, states := f.p.skillPackages(context.Background(), "CSR-1", append(bad, good...))
	if len(fl.calls) != len(good) || len(loaded) != len(good) || fl.downloads != 10 {
		t.Fatalf("fetched %q, loaded %+v; want only the good ones", fl.calls, loaded)
	}
	if len(states) != len(bad)+len(good) {
		t.Fatalf("%d states", len(states))
	}
	for i, s := range states {
		want := cinderline.SkillFailed
		if i >= len(bad) {
			want = cinderline.SkillLoaded
		}
		if s.State != want {
			t.Errorf("state %d %+v, want %s", i, s, want)
		}
	}
	if !reflect.DeepEqual(loaded[0], client.SkillMount{Name: "impeccable", Volume: "vol-plugin", Path: "plugin"}) || loaded[1].Path != "" {
		t.Errorf("loaded %+v", loaded)
	}
}

// A package whose fetch fails, such as one whose commit is not the one
// named, is failed; a second package of the same name is too, and more than
// ten are; the run goes on with the rest.
func TestSkillFetchFailsAndLimits(t *testing.T) {
	f := newFixture(t)
	fl := &fills{errs: map[string]error{"https://github.com/o/bad": errors.New("fetched commit ffff, not aaaa: refused")}}
	f.p.fillSkill = fl.fill
	skills := []cinderline.Skill{pkg("bad", "https://github.com/o/bad", ""), pkg("p", "https://github.com/o/p", ""), pkg("p", "https://github.com/o/other", "")}
	for i := range 8 {
		skills = append(skills, pkg("q"+string(rune('a'+i)), "https://github.com/o/q", ""))
	}
	loaded, states := f.p.skillPackages(context.Background(), "CSR-1", skills)
	got := make([]string, len(states))
	for i, s := range states {
		got[i] = s.State
	}
	want := []string{"failed", "loaded", "failed", "loaded", "loaded", "loaded", "loaded", "loaded", "loaded", "loaded", "failed"}
	if !slices.Equal(got, want) || len(loaded) != 8 || len(fl.calls) != 9 {
		t.Errorf("states %q, %d loaded, %d fetches", got, len(loaded), len(fl.calls))
	}
}

// A strict runner loads only the packages config allows by name and URL,
// and none with downloads, whose URLs that allowance does not cover; one
// allowed under another URL is refused again, and nothing refused is fetched.
func TestSkillStrict(t *testing.T) {
	engine := pkg("engine", "https://github.com/o/engine", "")
	engine.Downloads = []cinderline.SkillDownload{withDefaults(cinderline.SkillDownload{})}
	skills := []cinderline.Skill{pkg("ponytail", "https://github.com/o/ponytail", ""), pkg("moved", "https://github.com/evil/moved", ""), pkg("other", "https://github.com/o/other", ""), engine}
	for _, tc := range []struct {
		name   string
		strict bool
		want   []string
	}{
		{"strict", true, []string{"loaded", "refused_strict", "refused_strict", "refused_strict"}},
		{"not strict", false, []string{"loaded", "loaded", "loaded", "loaded"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			fl := &fills{}
			f.p.fillSkill, f.p.StrictHosts = fl.fill, tc.strict
			f.p.AllowSkills = []config.AllowedSkill{{Name: "ponytail", URL: "https://github.com/o/ponytail"}, {Name: "moved", URL: "https://github.com/o/moved"}, {Name: "engine", URL: "https://github.com/o/engine"}}
			_, states := f.p.skillPackages(context.Background(), "CSR-1", skills)
			var got []string
			for _, s := range states {
				got = append(got, s.State)
			}
			if !slices.Equal(got, tc.want) || len(fl.calls) != strings.Count(strings.Join(tc.want, " "), "loaded") {
				t.Errorf("states %q, fetches %q; want %q", got, fl.calls, tc.want)
			}
		})
	}
}

// Every request of a run reports each named package's state; a job that
// names none leaves workspace.skills out.
func TestRunReportsSkillStates(t *testing.T) {
	f := newFixture(t)
	f.p.StrictHosts = true
	f.p.AllowSkills = []config.AllowedSkill{{Name: "ponytail", URL: "https://github.com/o/ponytail"}}
	pony := tools.SkillContents{Skills: []string{"ponytail-help"}, Extension: true}
	f.p.fillSkill = (&fills{contents: map[string]tools.SkillContents{"https://github.com/o/ponytail": pony}}).fill
	f.cl.Runs = []client.RunReply{ok("s1", "Planned."), ok("w1", "Part done.")}
	app := &fakeApp{onNext: stepsOf(runNew, `{"kind":"run","issue":{"ref":"CSR-1.2","title":"Part"},"agent":"Coder","session":"new","token":null,"prompt":"Part."}`, `{"kind":"done","session":"kept"}`)}
	jb := soloJob()
	jb.Skills = []cinderline.Skill{pkg("ponytail", "https://github.com/o/ponytail", ""), pkg("impeccable", "https://github.com/o/impeccable", "plugin"), pkg("bad", "ftp://x/y", "")}
	if started, err := f.p.run(context.Background(), app.client(t), proj, jb, 0); !started || err != nil {
		t.Fatalf("run = %v, %v", started, err)
	}
	want := []cinderline.SkillState{{Name: "ponytail", State: "loaded"}, {Name: "impeccable", State: "refused_strict"}, {Name: "bad", State: "failed"}}
	for _, r := range app.requests() {
		if !slices.Equal(r.Workspace.Skills, want) {
			t.Errorf("workspace.skills %+v, want %+v", r.Workspace.Skills, want)
		}
	}
	// Every turn of the run mounts the loaded packages, the orchestrator's
	// and a worker's alike (CLR-258).
	mounts := []client.SkillMount{{Name: "ponytail", Volume: "vol-", SkillContents: pony}}
	if ts := turns(f.cl); len(ts) != 2 || !reflect.DeepEqual(ts[0].Skills, mounts) || !reflect.DeepEqual(ts[1].Skills, mounts) {
		t.Errorf("turns %+v, want each with %+v", ts, mounts)
	}

	g := newFixture(t)
	app = &fakeApp{onNext: stepsOf(`{"kind":"done","session":"kept"}`)}
	if _, err := g.run(t, app); err != nil {
		t.Fatal(err)
	}
	if s := app.requests()[0].Workspace.Skills; s != nil {
		t.Errorf("workspace.skills %+v, want none", s)
	}
}

// Gemini CLI loads plain skills by folder name, so a later package's skill
// folder of a name an earlier one has is logged; an extension's skills are
// Gemini CLI's own to name. Every package still loads.
func TestSkillGeminiClashIsLogged(t *testing.T) {
	f := newFixture(t)
	var logs strings.Builder
	f.p.Log = slog.New(slog.NewTextHandler(&logs, nil))
	f.p.fillSkill = (&fills{contents: map[string]tools.SkillContents{
		"https://github.com/o/a":   {Skills: []string{"review", "a"}},
		"https://github.com/o/ext": {Skills: []string{"review"}, Extension: true},
		"https://github.com/o/b":   {Skills: []string{"review"}},
	}}).fill
	loaded, _ := f.p.skillPackages(context.Background(), "CSR-1", []cinderline.Skill{pkg("a", "https://github.com/o/a", ""), pkg("ext", "https://github.com/o/ext", ""), pkg("b", "https://github.com/o/b", "")})
	if len(loaded) != 3 {
		t.Fatalf("loaded %+v", loaded)
	}
	if n := strings.Count(logs.String(), "skill not loaded in Gemini CLI turns"); n != 1 || !strings.Contains(logs.String(), "skill=b skill_folder=review earlier=a") {
		t.Errorf("%d clashes logged:\n%s", n, logs.String())
	}
}
