package run

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
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
			f.p.AddRefused("CSR-1", notAllowed(hp))
		}
	}}
	f.env.Runs = []environment.RunReply{{}}
	f.p.Env = refusingEnv{f.env, func() { f.p.AddRefused("CSR-1", notAllowed("files.pythonhosted.org:443")) }}
	f.p.AddRefused("CSR-1", notAllowed("before.example:443"))
	app := &fakeApp{onNext: stepsOf(runNew, checkStep, `{"kind":"stop"}`)}
	if _, err := f.run(t, app); err != nil {
		t.Fatal(err)
	}
	lasts := app.lasts()
	if len(lasts) != 3 {
		t.Fatalf("lasts = %+v", lasts)
	}
	if got := hostsOf(lasts[0].Refused); !slices.Equal(got, []string{"evil.example:443", "pypi.org:443"}) {
		t.Errorf("run step refused %q", got)
	}
	if got := hostsOf(lasts[1].Refused); lasts[1].Step != "check" || !slices.Equal(got, []string{"files.pythonhosted.org:443"}) {
		t.Errorf("check step %s refused %q", lasts[1].Step, got)
	}
	if got := lasts[2].Refused; got != nil {
		t.Errorf("stop step refused %v, want none", got)
	}
	if f.p.refused != 6 {
		t.Errorf("refused count %d, want 6", f.p.refused)
	}
}

// CLR-193: the hosts a person allowed for the issue join the step's
// allowlist, each checked as an allow_hosts entry; an invalid one is
// dropped, not the step. On a strict runner none joins.
func TestAStepOpensTheHostsAPersonAllowed(t *testing.T) {
	allow := `"allow_hosts":["PyPI.org:443","localhost:8080","host.docker.internal:11434","127.0.0.1:1","*.evil.example:443","evil.example","files.pythonhosted.org:443","93.184.216.34:443","172.17.0.1:80","192.168.1.1:80","10.0.0.5:443","169.254.169.254:80","0.0.0.0:80"],`
	for _, strict := range []bool{false, true} {
		f := newFixture(t)
		var logs bytes.Buffer
		f.p.Log = slog.New(slog.NewTextHandler(&logs, nil))
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
			want = []string{"pypi.org:443", "files.pythonhosted.org:443", "93.184.216.34:443"}
		}
		if lasts := app.lasts(); len(lasts) != 2 || lasts[0].Outcome != "finished" || lasts[1].Outcome != "passed" {
			t.Errorf("strict %v: lasts = %+v", strict, lasts)
		}
		if got := turns(f.cl)[0].Allow; !slices.Equal(got, want) {
			t.Errorf("strict %v: turn allows %q, want %q", strict, got, want)
		}
		if got := f.env.Calls()[0].Args[0].(environment.Spec).AppAllow; !slices.Equal(got, want) {
			t.Errorf("strict %v: check allows %q, want %q", strict, got, want)
		}
		if !strict {
			for _, hp := range []string{"172.17.0.1:80", "192.168.1.1:80", "10.0.0.5:443", "169.254.169.254:80", "0.0.0.0:80"} {
				if !strings.Contains(logs.String(), `msg="host allowed in Cinderline dropped:`) || !strings.Contains(logs.String(), "host="+hp) {
					t.Errorf("missing drop log for %s: %s", hp, logs.String())
				}
			}
		}
	}
}

// notAllowed is a refusal of hp as on no allowlist, without its request.
func notAllowed(hp string) environment.Refusal {
	return environment.Refusal{Host: hp, Reason: cinderline.RefusedNotAllowed}
}

// hostsOf is the host:port of each refused host.
func hostsOf(rs []cinderline.RefusedHost) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Host)
	}
	return out
}

// CLR-248: a step reports each refused host once, with the proxy's reason
// and the first request the proxy read there, also when an earlier
// connection to it had none; at most 20 hosts.
func TestAStepReportsEachHostsReasonAndFirstRequest(t *testing.T) {
	f := newFixture(t)
	refuse := func(hp, reason, method, url string) {
		f.p.AddRefused("CSR-1", environment.Refusal{Host: hp, Reason: reason, Method: method, URL: url})
	}
	refuse("pinned.example:443", cinderline.RefusedNotAllowed, "", "")
	refuse("pinned.example:443", cinderline.RefusedNotAllowed, "GET", "https://pinned.example/first")
	refuse("pinned.example:443", cinderline.RefusedNotAllowed, "POST", "https://pinned.example/second")
	refuse("localhost:8080", cinderline.RefusedRunnerMachine, "", "")
	refuse("internal.example:443", cinderline.RefusedPrivateAddress, "GET", "https://internal.example/")
	for i := range 30 {
		refuse(fmt.Sprintf("h%d.example:443", i), cinderline.RefusedNotAllowed, "", "")
	}
	got := f.p.takeRefused("CSR-1", nil)
	if len(got) != cinderline.MaxRefused || got[19].Host != "h16.example:443" {
		t.Fatalf("refused %q, want the first 20 hosts", hostsOf(got))
	}
	if r := got[0]; r.Reason != "not_allowed" || r.Method == nil || *r.Method != "GET" || *r.URL != "https://pinned.example/first" {
		t.Errorf("pinned.example = %+v, want its first request", r)
	}
	if r := got[1]; r.Host != "localhost:8080" || r.Reason != "runner_machine" || r.URL != nil {
		t.Errorf("localhost = %+v", r)
	}
	if r := got[2]; r.Reason != "private_address" || *r.URL != "https://internal.example/" {
		t.Errorf("internal.example = %+v", r)
	}
	if f.p.refused != 35 {
		t.Errorf("refused count %d, want every connection", f.p.refused)
	}
}

// CLR-248: a host a person allowed in Cinderline that the proxy still
// found on no allowlist was dropped by the runner: on a strict runner it is
// reported as strict, otherwise as a private address. The proxy's own
// reasons and the hosts the step did not allow stay.
func TestAStepReportsWhyItDroppedAnAllowedHost(t *testing.T) {
	allow := `"allow_hosts":["PyPI.org:443","10.0.0.5:443","localhost:8080"],`
	for _, strict := range []bool{false, true} {
		f := newFixture(t)
		f.p.StrictHosts = strict
		f.cl.Runs = []client.RunReply{ok("s", "Done.")}
		f.p.Clients["claude"] = refusingClient{f.cl, func() {
			f.p.AddRefused("CSR-1", notAllowed("pypi.org:443"))
			f.p.AddRefused("CSR-1", notAllowed("10.0.0.5:443"))
			f.p.AddRefused("CSR-1", environment.Refusal{Host: "localhost:8080", Reason: cinderline.RefusedRunnerMachine})
			f.p.AddRefused("CSR-1", notAllowed("other.example:443"))
			f.p.AddRefused("CSR-1", environment.Refusal{Host: "app.example:443", Reason: cinderline.RefusedPrivateAddress})
		}}
		app := &fakeApp{onNext: stepsOf(strings.Replace(runNew, `"prompt"`, allow+`"prompt"`, 1))}
		if _, err := f.run(t, app); err != nil {
			t.Fatal(err)
		}
		dropped := map[bool]string{false: "private_address", true: "strict"}[strict]
		var got []string
		for _, r := range app.lasts()[0].Refused {
			got = append(got, r.Host+" "+r.Reason)
		}
		want := []string{"pypi.org:443 " + dropped, "10.0.0.5:443 " + dropped, "localhost:8080 runner_machine", "other.example:443 not_allowed", "app.example:443 private_address"}
		if !slices.Equal(got, want) {
			t.Errorf("strict %v: refused %q, want %q", strict, got, want)
		}
	}
}

// CLR-273: an app approval of the web page's port on the runner's own
// machine never joins a step's allowlist.
func TestAnAppApprovalNeverOpensTheWebPort(t *testing.T) {
	f := newFixture(t)
	web := strconv.Itoa(config.DefaultWebPort)
	hosts := []string{"host.docker.internal:" + web, "127.0.0.1:" + web, "localhost:" + web, "[::1]:" + web}
	if got := f.p.allowHosts("CSR-1", hosts); got != nil {
		t.Errorf("allowHosts(%q) = %q, want none", hosts, got)
	}
}
