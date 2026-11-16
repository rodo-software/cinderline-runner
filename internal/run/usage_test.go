package run

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/client"
)

func cost(usd float64) *client.Usage {
	return &client.Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 40, CostUSD: &usd}
}

// usageReports returns the bodies of the usage calls on ref.
func usageReports(api *fakeAPI, ref string) []string {
	var out []string
	for _, w := range api.writes() {
		if body, ok := strings.CutPrefix(w, "POST /runs/"+ref+"/usage "); ok {
			out = append(out, body)
		}
	}
	return out
}

// The end of every run reports its usage in one call; a stopped run too.
func TestReportUsageSendsOneCallAndClears(t *testing.T) {
	p := project(t)
	p.Hooks()
	p.run.addEntry("CSR-1", *usageEntry("CSR-2", "Qwen", "", cost(0.25)))
	p.run.addEntry("CSR-1", *usageEntry("CSR-1", "Builder", "m", &client.Usage{InputTokens: 1}))
	p.run.addEntry("CSR-9", *usageEntry("CSR-9", "Other", "m", cost(1)))
	api := &fakeAPI{routes: map[string]reply{"POST /runs/CSR-1/usage": {204, ""}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p.reportUsage(ctx, api.client(t), "CSR-1")
	p.reportUsage(ctx, api.client(t), "CSR-1")

	want := `{"entries":[` +
		`{"issue":"CSR-2","agent":"Qwen","model":null,"input_tokens":10,"output_tokens":20,"cache_read_tokens":30,"cache_write_tokens":40,"cost_usd":0.25},` +
		`{"issue":"CSR-1","agent":"Builder","model":"m","input_tokens":1,"output_tokens":0,"cache_read_tokens":0,"cache_write_tokens":0,"cost_usd":null}]}`
	if got := usageReports(api, "CSR-1"); !slices.Equal(got, []string{want}) {
		t.Errorf("usage reports = %q, want %q", got, want)
	}
	if got := p.run.usage["CSR-9"]; len(got) != 1 {
		t.Errorf("another run's entries = %v, want kept", got)
	}
	if e := usageEntry("CSR-1", "Builder", "m", nil); e != nil {
		t.Errorf("usage of a CLI that printed none = %+v, want nil", e)
	}
}
