package client

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/tools"
)

// Two skill packages as the run hands them over: ponytail at its
// repository's root, with skills, a Gemini manifest and an OpenCode plugin,
// and impeccable at plugin/ in its repository, with skills only.
var (
	ponytail = SkillMount{Name: "ponytail", Volume: "vol-p", SkillContents: tools.SkillContents{
		Skills: []string{"ponytail", "ponytail-help"}, Extension: true, Plugins: []string{".opencode/plugins/index.js"},
	}}
	impeccable = SkillMount{Name: "impeccable", Volume: "vol-i", Path: "plugin", SkillContents: tools.SkillContents{
		Skills: []string{"impeccable"},
	}}
)

// skillSpec runs one resumed turn of client with skills and token and
// returns what it asked the Environment for.
func skillSpec(t *testing.T, client string, skills []SkillMount, token string) environment.Spec {
	t.Helper()
	env := &environment.Fake{Runs: []environment.RunReply{{}}}
	c := &CLI{
		Name: client, Project: "CSR", Env: env, Credential: "key", MCPURL: "https://cs.example/mcp",
		Tools: tools.Mount{Args: []string{"-v", "tools:/opt/cinderline:ro"}},
		Exec:  func(context.Context, []string, ...string) (string, error) { return "", nil },
	}
	_, _ = c.Run(context.Background(), Turn{Dir: "/runs/CSR-7", SessionID: "s1", MaxTurns: 5, Model: "openrouter/m", Token: token, Skills: skills})
	calls := env.Calls()
	if len(calls) != 1 {
		t.Fatalf("%s: %d turns", client, len(calls))
	}
	return calls[0].Args[0].(environment.Spec)
}

// Every client mounts each package's repository read-only at
// /skills/<name>, then loads it its own way (docs/skills/client-loading.md):
// Claude Code with one --plugin-dir per package, Codex with each package's
// skills/ in $CODEX_HOME/skills, Gemini CLI with a package that has a
// manifest as an extension and any other's skill folders in its skills
// folder, OpenCode with skills.paths and the plugin files in its inline
// config. Nothing else of the turn changes, with a token or without.
func TestSkillPackagesPerClient(t *testing.T) {
	repo := []string{
		"--mount", "type=volume,src=vol-p,dst=/skills/ponytail,readonly,volume-subpath=repo",
		"--mount", "type=volume,src=vol-i,dst=/skills/impeccable,readonly,volume-subpath=repo",
	}
	type want struct {
		mounts []string // after the /skills mounts
		args   []string // after the argv without packages
		config string   // OpenCode: the inline config's skills and plugin members
	}
	cases := map[string][2]want{ // one package (ponytail), two (and impeccable)
		"claude": {
			{args: []string{"--plugin-dir", "/skills/ponytail"}},
			{args: []string{"--plugin-dir", "/skills/ponytail", "--plugin-dir", "/skills/impeccable/plugin"}},
		},
		"codex": {
			{mounts: []string{
				"--mount", "type=tmpfs,dst=/state/codex/skills",
				"--mount", "type=volume,src=vol-p,dst=/state/codex/skills/ponytail,readonly,volume-subpath=repo/skills",
			}},
			{mounts: []string{
				"--mount", "type=tmpfs,dst=/state/codex/skills",
				"--mount", "type=volume,src=vol-p,dst=/state/codex/skills/ponytail,readonly,volume-subpath=repo/skills",
				"--mount", "type=volume,src=vol-i,dst=/state/codex/skills/impeccable,readonly,volume-subpath=repo/plugin/skills",
			}},
		},
		"gemini": {
			{mounts: []string{
				"--mount", "type=tmpfs,dst=/state/gemini/.gemini/extensions",
				"--mount", "type=volume,src=vol-p,dst=/state/gemini/.gemini/extensions/ponytail,readonly,volume-subpath=repo",
			}},
			{mounts: []string{
				"--mount", "type=tmpfs,dst=/state/gemini/.gemini/extensions",
				"--mount", "type=volume,src=vol-p,dst=/state/gemini/.gemini/extensions/ponytail,readonly,volume-subpath=repo",
				"--mount", "type=tmpfs,dst=/state/gemini/.agents/skills",
				"--mount", "type=volume,src=vol-i,dst=/state/gemini/.agents/skills/impeccable,readonly,volume-subpath=repo/plugin/skills/impeccable",
			}},
		},
		"opencode": {
			{config: `{"plugin":["/skills/ponytail/.opencode/plugins/index.js"],"skills":{"paths":["/skills/ponytail/skills"]}}`},
			{config: `{"plugin":["/skills/ponytail/.opencode/plugins/index.js"],"skills":{"paths":["/skills/ponytail/skills","/skills/impeccable/plugin/skills"]}}`},
		},
	}
	for _, client := range slices.Sorted(maps.Keys(cases)) {
		for _, token := range []string{"tok", ""} {
			none := skillSpec(t, client, nil, token)
			if slices.ContainsFunc(none.Mounts, func(m string) bool { return strings.Contains(m, "skill") }) ||
				slices.ContainsFunc(none.Cmd, func(a string) bool { return strings.Contains(a, "--plugin-dir") }) ||
				slices.ContainsFunc(none.Env, func(e string) bool { return strings.Contains(e, `"skills"`) || strings.Contains(e, `"plugin"`) }) {
				t.Errorf("%s, token %q: a turn without packages loads some: %q %q %q", client, token, none.Mounts, none.Cmd, none.Env)
			}
			for n, w := range cases[client] {
				skills := []SkillMount{ponytail, impeccable}[:n+1]
				s := skillSpec(t, client, skills, token)
				name := client + ", " + strings.Repeat("+package", n+1) + ", token " + token
				if got, want := s.Mounts, slices.Concat(none.Mounts, repo[:2*(n+1)], w.mounts); !slices.Equal(got, want) {
					t.Errorf("%s: mounts\n%q\nwant\n%q", name, got, want)
				}
				if got, want := s.Cmd, slices.Concat(none.Cmd, w.args); !slices.Equal(got, want) {
					t.Errorf("%s: argv\n%q\nwant\n%q", name, got, want)
				}
				if client != "opencode" {
					if !slices.Equal(s.Env, none.Env) {
						t.Errorf("%s: env\n%q\nwant\n%q", name, s.Env, none.Env)
					}
					continue
				}
				// OpenCode: only the inline config changes, by its skills and plugin members.
				i := slices.IndexFunc(s.Env, func(e string) bool { return strings.HasPrefix(e, "OPENCODE_CONFIG_CONTENT=") })
				if i < 0 || !slices.Equal(slices.Delete(slices.Clone(s.Env), i, i+1), slices.Delete(slices.Clone(none.Env), i, i+1)) {
					t.Fatalf("%s: env\n%q\nwant only the config to differ from\n%q", name, s.Env, none.Env)
				}
				var got, base map[string]any
				_ = json.Unmarshal([]byte(strings.TrimPrefix(s.Env[i], "OPENCODE_CONFIG_CONTENT=")), &got)
				_ = json.Unmarshal([]byte(strings.TrimPrefix(none.Env[i], "OPENCODE_CONFIG_CONTENT=")), &base)
				var members map[string]any
				_ = json.Unmarshal([]byte(w.config), &members)
				for k, v := range members {
					base[k] = v
				}
				if !reflect.DeepEqual(got, base) {
					t.Errorf("%s: config\n%v\nwant\n%v", name, got, base)
				}
			}
		}
	}
}

// Gemini CLI does not namespace plain skills: a later package's skill folder
// of a name an earlier package's took is left out, rather than mounted twice
// at one place, which docker refuses. A package without skills, manifest or
// plugin is only mounted at /skills for every client but Claude Code.
func TestSkillPackagesGeminiClashAndEmptyPackage(t *testing.T) {
	a := SkillMount{Name: "a", Volume: "vol-a", SkillContents: tools.SkillContents{Skills: []string{"review", "lint"}}}
	b := SkillMount{Name: "b", Volume: "vol-b", SkillContents: tools.SkillContents{Skills: []string{"review", "docs"}}}
	got := geminiSkillMounts(Turn{Skills: []SkillMount{a, b}})
	want := []string{
		"--mount", "type=tmpfs,dst=/state/gemini/.agents/skills",
		"--mount", "type=volume,src=vol-a,dst=/state/gemini/.agents/skills/review,readonly,volume-subpath=repo/skills/review",
		"--mount", "type=volume,src=vol-a,dst=/state/gemini/.agents/skills/lint,readonly,volume-subpath=repo/skills/lint",
		"--mount", "type=volume,src=vol-b,dst=/state/gemini/.agents/skills/docs,readonly,volume-subpath=repo/skills/docs",
	}
	if !slices.Equal(got, want) {
		t.Errorf("mounts\n%q\nwant\n%q", got, want)
	}

	empty := []SkillMount{{Name: "hooks-only", Volume: "vol-h"}}
	if m := codexSkillMounts(Turn{Skills: empty}); m != nil {
		t.Errorf("codex mounts %q", m)
	}
	if m := geminiSkillMounts(Turn{Skills: empty}); m != nil {
		t.Errorf("gemini mounts %q", m)
	}
	if cfg := opencodeConfig(Turn{Skills: empty}, "u", nil); strings.Contains(cfg, `"skills"`) || strings.Contains(cfg, `"plugin"`) {
		t.Errorf("opencode config %s", cfg)
	}
}
