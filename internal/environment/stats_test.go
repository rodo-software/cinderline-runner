package environment

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestParseStats(t *testing.T) {
	for _, tc := range []struct {
		cpu, mem string
		want     ContainerStats
	}{
		{"12.5%", "1.2GiB / 4GiB", ContainerStats{CPUPercent: 12.5, MemoryBytes: 1288490188, MemoryLimitBytes: 4 << 30}},
		{"0.00%", "512KiB / 64MiB", ContainerStats{MemoryBytes: 512 << 10, MemoryLimitBytes: 64 << 20}},
		{"250.31%", "800B / 1TiB", ContainerStats{CPUPercent: 250.31, MemoryBytes: 800, MemoryLimitBytes: 1 << 40}},
		{"1.5%", "3.5MiB", ContainerStats{CPUPercent: 1.5, MemoryBytes: 3670016}}, // no limit, as on Windows
		{"--", "-- / --", ContainerStats{}}, // not measured yet
		{"0.00%", "0B / 0B", ContainerStats{}},
	} {
		got, err := parseStats(tc.cpu, tc.mem)
		if err != nil || got != tc.want {
			t.Errorf("parseStats(%q, %q) = %+v, %v, want %+v", tc.cpu, tc.mem, got, err, tc.want)
		}
	}
	for _, bad := range [][2]string{{"x%", "1B / 1B"}, {"1%", "lots / 1B"}, {"1%", "1B / 2XB"}, {"1%", "-1MiB / 2MiB"}} {
		if _, err := parseStats(bad[0], bad[1]); err == nil {
			t.Errorf("parseStats(%q, %q) took it", bad[0], bad[1])
		}
	}
}

// The agent and the compose services are measured by one docker stats; the
// proxy and a throwaway helper with the issue's labels are left out.
func TestStatsLeavesOutTheProxy(t *testing.T) {
	ps := "cinderline-CSR-CSR-7-agent \ncinderline-CSR-CSR-7-proxy \ncsr-7-db-1 db\nhelper_1 \n"
	stats := `{"BlockIO":"0B / 0B","CPUPerc":"1.00%","Container":"csr-7-db-1","MemUsage":"10MiB / 512MiB","Name":"csr-7-db-1"}
{"BlockIO":"0B / 0B","CPUPerc":"50.00%","Container":"cinderline-CSR-CSR-7-agent","MemUsage":"1GiB / 4GiB","Name":"cinderline-CSR-CSR-7-agent"}
`
	d, calls := scripted([]string{ps, stats}, nil)
	got, err := d.Stats(context.Background(), "CSR-7")
	if err != nil {
		t.Fatal(err)
	}
	want := []ContainerStats{
		{Name: "agent", CPUPercent: 50, MemoryBytes: 1 << 30, MemoryLimitBytes: 4 << 30},
		{Name: "db", CPUPercent: 1, MemoryBytes: 10 << 20, MemoryLimitBytes: 512 << 20},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Stats = %+v, want %+v", got, want)
	}
	wantCalls := []execCall{
		{nil, []string{"docker", "ps", "--filter", "label=cinderline.project=CSR", "--filter", "label=cinderline.issue=CSR-7",
			"--format", `{{.Names}} {{.Label "com.docker.compose.service"}}`}},
		{nil, []string{"docker", "stats", "--no-stream", "--format", "{{json .}}", "cinderline-CSR-CSR-7-agent", "csr-7-db-1"}},
	}
	if !reflect.DeepEqual(*calls, wantCalls) {
		t.Errorf("calls =\n%q\nwant\n%q", *calls, wantCalls)
	}
}

// Between steps only the proxy, or nothing, runs: an empty list, no docker stats.
func TestStatsWithoutContainers(t *testing.T) {
	d, calls := scripted([]string{"cinderline-CSR-CSR-7-proxy \n"}, nil)
	got, err := d.Stats(context.Background(), "CSR-7")
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("Stats = %#v, %v, want an empty list", got, err)
	}
	if len(*calls) != 1 {
		t.Errorf("calls = %q, want only docker ps", *calls)
	}
}

func TestStatsErrors(t *testing.T) {
	boom := errors.New("boom")
	d, _ := scripted([]string{"cinderline-CSR-CSR-7-agent \n"}, map[int]error{1: boom})
	if _, err := d.Stats(context.Background(), "CSR-7"); !errors.Is(err, boom) {
		t.Errorf("Stats = %v, want boom", err)
	}
	d, calls := scripted(nil, nil)
	if _, err := d.Stats(context.Background(), "CSR-7 --all"); err == nil || len(*calls) != 0 {
		t.Errorf("Stats of a bad ref = %v, calls %q, want refused before docker", err, *calls)
	}
}
