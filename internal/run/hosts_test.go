package run

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/environment"
)

// refusingClient has the proxy refuse hosts during each turn, as Docker's
// OnRefused does when the turn ends.
type refusingClient struct {
	*client.Fake
	refuse func()
}

func (r refusingClient) Run(ctx context.Context, t client.Turn) (client.Result, error) {
	r.refuse()
	return r.Fake.Run(ctx, t)
}

// refusingEnv does the same during a check.
type refusingEnv struct {
	*environment.Fake
	refuse func()
}

func (r refusingEnv) Run(ctx context.Context, s environment.Spec) (string, error) {
	r.refuse()
	return r.Fake.Run(ctx, s)
}

const checkStep = `{"kind":"check","issue":{"ref":"CSR-1","title":"Fix it"}}`

// CLR-193: each run, resolve and check step reports the hosts the proxy
// refused during it, once each, in lower case, and only well-formed ones;
// what came before the step is not its own. The report still counts every
// refused connection.
func TestEachStepReportsItsRefusedHosts(t *testing.T) {
	f := newFixture(t)
	f.cl.Runs = []client.RunReply{ok("s", "Done.")}
	f.p.Clients["claude"] = refusingClient{f.cl, func() {
		for _, hp := range []string{"Evil.Example:443", "pypi.org:443", "evil.example:443", "garbage:"} {
			f.p.AddRefused(hp)
		}
	}}
	f.env.Runs = []environment.RunReply{{}}
	f.p.Env = refusingEnv{f.env, func() { f.p.AddRefused("files.pythonhosted.org:443") }}
	f.p.AddRefused("before.example:443")
	app := &fakeApp{onNext: stepsOf(runNew, checkStep, `{"kind":"stop"}`)}
	if _, err := f.run(t, app); err != nil {
		t.Fatal(err)
	}
	lasts := app.lasts()
	if len(lasts) != 3 {
		t.Fatalf("lasts = %+v", lasts)
	}
	if got := lasts[0].RefusedHosts; !slices.Equal(got, []string{"evil.example:443", "pypi.org:443"}) {
		t.Errorf("run step refused %q", got)
	}
	if got := lasts[1].RefusedHosts; lasts[1].Step != "check" || !slices.Equal(got, []string{"files.pythonhosted.org:443"}) {
		t.Errorf("check step %s refused %q", lasts[1].Step, got)
	}
	if got := lasts[2].RefusedHosts; got != nil {
		t.Errorf("stop step refused %q, want none", got)
	}
	if f.p.refused != 6 {
		t.Errorf("refused count %d, want 6", f.p.refused)
	}
}

// CLR-193: the hosts a person allowed for the issue join the step's
// allowlist, each checked as an allow_hosts entry; an invalid one is
// dropped, not the step. On a strict runner none joins.
func TestAStepOpensTheHostsAPersonAllowed(t *testing.T) {
	allow := `"allow_hosts":["PyPI.org:443","localhost:8080","host.docker.internal:11434","127.0.0.1:1","*.evil.example:443","evil.example","files.pythonhosted.org:443"],`
	for _, strict := range []bool{false, true} {
		f := newFixture(t)
		f.p.StrictHosts = strict
		f.cl.Runs = []client.RunReply{ok("s", "Done.")}
		f.env.Runs = []environment.RunReply{{}}
		run := strings.Replace(runNew, `"prompt"`, allow+`"prompt"`, 1)
		check := strings.Replace(checkStep, `"issue"`, allow+`"issue"`, 1)
		app := &fakeApp{onNext: stepsOf(run, check)}
		if _, err := f.run(t, app); err != nil {
			t.Fatal(err)
		}
		var want []string
		if !strict {
			want = []string{"pypi.org:443", "files.pythonhosted.org:443"}
		}
		if lasts := app.lasts(); len(lasts) != 2 || lasts[0].Outcome != "finished" || lasts[1].Outcome != "passed" {
			t.Errorf("strict %v: lasts = %+v", strict, lasts)
		}
		if got := turns(f.cl)[0].Allow; !slices.Equal(got, want) {
			t.Errorf("strict %v: turn allows %q, want %q", strict, got, want)
		}
		if got := f.env.Calls()[0].Args[0].(environment.Spec).Allow; !slices.Equal(got, want) {
			t.Errorf("strict %v: check allows %q, want %q", strict, got, want)
		}
	}
}
