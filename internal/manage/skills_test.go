package manage

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
)

// skillsConfig writes a config.json with WEB, and API in two places: IDs
// WEB, API and team-API; API allows api-only.
func skillsConfig(t *testing.T) config.Paths {
	t.Helper()
	p := paths(t)
	cfg := &config.Config{Projects: []config.Project{
		{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"},
		{Workspace: "default", URL: "https://a.example.com", Key: "API", Repository: "https://git.example.com/o/api", AllowSkills: []config.AllowedSkill{{Name: "api-only", URL: "https://github.com/o/a"}}},
		{Workspace: "team", URL: "https://b.example.com", Key: "API", Repository: "https://git.example.com/o/api2"},
	}}
	if err := cfg.Save(p.File); err != nil {
		t.Fatal(err)
	}
	return p
}

// allowSkills are the runner-wide entries, then those of each project.
func allowSkills(t *testing.T, p config.Paths) [][]config.AllowedSkill {
	t.Helper()
	cfg, err := config.Load(p.File) // Load validates
	if err != nil {
		t.Fatal(err)
	}
	out := [][]config.AllowedSkill{cfg.AllowSkills}
	for _, q := range cfg.Projects {
		out = append(out, q.AllowSkills)
	}
	return out
}

func skill(n, url string) config.AllowedSkill { return config.AllowedSkill{Name: n, URL: url} }

// CLR-296: AddSkill and RemoveSkill change runner-wide allow_skills for an
// empty project and that project's otherwise, and nothing else; a new URL
// replaces a name's old one.
func TestAddAndRemoveSkill(t *testing.T) {
	p := skillsConfig(t)
	for _, a := range [][3]string{
		{"", "pony", "https://github.com/o/pony"},
		{"WEB", "pony", "https://github.com/o/pony"},
		{"team-API", "imp", "https://github.com/o/imp"},
		{"API", "api-only", "https://github.com/fork/a"},
	} {
		if err := AddSkill(p, a[0], a[1], a[2]); err != nil {
			t.Fatalf("AddSkill %q: %v", a, err)
		}
	}
	want := [][]config.AllowedSkill{
		{skill("pony", "https://github.com/o/pony")},
		{skill("pony", "https://github.com/o/pony")},
		{skill("api-only", "https://github.com/fork/a")},
		{skill("imp", "https://github.com/o/imp")},
	}
	if got := allowSkills(t, p); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("after adds %v, want %v", got, want)
	}
	for _, r := range [][2]string{{"", "pony"}, {"API", "api-only"}} {
		if err := RemoveSkill(p, r[0], r[1]); err != nil {
			t.Fatalf("RemoveSkill %q: %v", r, err)
		}
	}
	want = [][]config.AllowedSkill{nil, want[1], nil, want[3]}
	if got := allowSkills(t, p); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("after removes %v, want %v", got, want)
	}
}

// AddSkill works without a config.json yet, as the CLI did.
func TestAddSkillWithoutAConfig(t *testing.T) {
	p := paths(t)
	if err := AddSkill(p, "", "pony", "https://github.com/o/pony"); err != nil {
		t.Fatal(err)
	}
	if got := allowSkills(t, p); !slices.Equal(got[0], []config.AllowedSkill{skill("pony", "https://github.com/o/pony")}) {
		t.Errorf("allow_skills %v", got[0])
	}
}

// Every refusal is Refused with the command's text and writes nothing;
// CheckAddSkill refuses the same adds, with the same text.
func TestSkillRefusals(t *testing.T) {
	p := skillsConfig(t)
	if err := AddSkill(p, "WEB", "pony", "https://github.com/o/pony"); err != nil {
		t.Fatal(err)
	}
	before := allowSkills(t, p)
	for _, c := range []struct {
		what               string
		add                bool
		project, name, url string
		want               string
	}{
		{"upper case", true, "WEB", "Pony", "https://github.com/o/p", `"Pony" is not a skill package's name: lower-case letters and digits with single hyphens, at most 64 characters, as in Cinderline's catalog`},
		{"double hyphen", true, "", "a--b", "https://github.com/o/p", "not a skill package's name"},
		{"http", true, "WEB", "p", "http://github.com/o/p", "skill package p: a skill package's URL must be https://host/path"},
		{"user info", true, "", "p", "https://u:tok@github.com/o/p", "https://host/path"},
		{"query", true, "WEB", "p", "https://github.com/o/p?ref=main", "https://host/path"},
		{"duplicate", true, "WEB", "pony", "https://github.com/o/pony", "the allow_skills of WEB already allows pony from https://github.com/o/pony"},
		{"unknown project", true, "NOPE", "p", "https://github.com/o/p", "no project NOPE"},
		{"a key, not an ID", true, "web", "p", "https://github.com/o/p", "no project web"},
		{"remove missing", false, "WEB", "nope", "", "the allow_skills of WEB has no entry nope; its entries: pony"},
		{"remove missing runner-wide", false, "", "nope", "", "runner-wide allow_skills has no entry nope; its entries: none"},
		{"remove unknown project", false, "NOPE", "pony", "", "no project NOPE"},
	} {
		var err error
		if c.add {
			err = AddSkill(p, c.project, c.name, c.url)
		} else {
			err = RemoveSkill(p, c.project, c.name)
		}
		if r := (Refused{}); !errors.As(err, &r) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want Refused with %q", c.what, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), "u:tok") {
			t.Errorf("%s: a password in the error: %v", c.what, err)
		}
		if got := allowSkills(t, p); !slices.EqualFunc(got, before, slices.Equal) {
			t.Fatalf("%s wrote %v", c.what, got)
		}
		if c.add {
			if cerr := CheckAddSkill(p, c.project, c.name, c.url); cerr == nil || err == nil || cerr.Error() != err.Error() {
				t.Errorf("CheckAddSkill %s = %v, want AddSkill's %v", c.what, cerr, err)
			}
		}
	}
	if err := CheckAddSkill(p, "WEB", "pony", "https://github.com/fork/pony"); err != nil {
		t.Errorf("CheckAddSkill of a new URL = %v", err)
	}
	if got := allowSkills(t, p); !slices.EqualFunc(got, before, slices.Equal) {
		t.Errorf("CheckAddSkill wrote %v", got)
	}
}

// The same entry again is SkillAllowed, so the CLI can say nothing changed.
func TestAddSkillDuplicateIsSkillAllowed(t *testing.T) {
	p := skillsConfig(t)
	err := AddSkill(p, "API", "api-only", "https://github.com/o/a")
	if a := (SkillAllowed{}); !errors.As(err, &a) || a != (SkillAllowed{"API", skill("api-only", "https://github.com/o/a")}) {
		t.Errorf("AddSkill = %v, want SkillAllowed of API api-only", err)
	}
	if err := AddSkill(p, "", "pony", "https://github.com/o/pony"); err != nil {
		t.Fatal(err)
	}
	if err := AddSkill(p, "", "pony", "https://github.com/o/pony"); err == nil || err.Error() != "runner-wide allow_skills already allows pony from https://github.com/o/pony" {
		t.Errorf("runner-wide duplicate = %v", err)
	}
}
