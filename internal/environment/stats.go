package environment

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// ContainerStats is one container of a run, as docker stats measured it.
type ContainerStats struct {
	Name             string  `json:"name"` // the compose service's name, or agent
	CPUPercent       float64 `json:"cpu_percent"`
	MemoryBytes      int64   `json:"memory_bytes"`
	MemoryLimitBytes int64   `json:"memory_limit_bytes"` // 0: docker gave none
}

// Stats measures the CPU and memory of the issue's agent container and
// compose services, not of its egress proxy, once. No container (between
// steps, or after the run) is an empty list. Other containers with the
// issue's labels, the throwaway git helpers, are left out too.
func (d *Docker) Stats(ctx context.Context, issue string) ([]ContainerStats, error) {
	if !workspace.IssueRef.MatchString(issue) {
		return nil, fmt.Errorf("stats: issue %q", issue)
	}
	argv := []string{"docker", "ps"}
	l := workspace.Labels(d.Project, issue)
	for i := 1; i < len(l); i += 2 {
		argv = append(argv, "--filter", "label="+l[i])
	}
	out, err := d.exec(ctx, append(argv, "--format", `{{.Names}} {{.Label "com.docker.compose.service"}}`)...)
	if err != nil {
		return nil, err
	}
	names := map[string]string{} // container name: the name shown
	var containers []string
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		switch {
		case len(f) == 2:
			names[f[0]] = f[1]
		case len(f) == 1 && f[0] == d.network(issue)+"-agent":
			names[f[0]] = "agent"
		default:
			continue
		}
		containers = append(containers, f[0])
	}
	stats := []ContainerStats{}
	if len(containers) == 0 {
		return stats, nil
	}
	out, err = d.exec(ctx, append([]string{"docker", "stats", "--no-stream", "--format", "{{json .}}"}, containers...)...)
	if err != nil {
		return nil, err
	}
	for line := range strings.Lines(out) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Container is the name as given, Name docker's own.
		var s struct{ Container, CPUPerc, MemUsage string }
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			return nil, fmt.Errorf("docker stats: %w", err)
		}
		c, err := parseStats(s.CPUPerc, s.MemUsage)
		if err != nil {
			return nil, fmt.Errorf("docker stats of %s: %w", s.Container, err)
		}
		c.Name = names[s.Container]
		stats = append(stats, c)
	}
	slices.SortFunc(stats, func(a, b ContainerStats) int { return strings.Compare(a.Name, b.Name) })
	return stats, nil
}

// parseStats reads docker stats' texts: cpu as 12.5%, mem as
// 1.2GiB / 4GiB, or without the limit as on Windows. Docker writes -- for
// a number it does not have yet, which is 0.
func parseStats(cpu, mem string) (ContainerStats, error) {
	var c ContainerStats
	var err error
	if cpu != "--" {
		if c.CPUPercent, err = strconv.ParseFloat(strings.TrimSuffix(cpu, "%"), 64); err != nil {
			return c, fmt.Errorf("cpu %q", cpu)
		}
	}
	used, limit, _ := strings.Cut(mem, " / ")
	if c.MemoryBytes, err = parseBytes(used); err != nil {
		return c, fmt.Errorf("memory %q", mem)
	}
	if c.MemoryLimitBytes, err = parseBytes(limit); err != nil {
		return c, fmt.Errorf("memory %q", mem)
	}
	return c, nil
}

// binaryUnits are the units docker writes memory in (go-units' BytesSize).
var binaryUnits = []string{"KiB", "MiB", "GiB", "TiB", "PiB"}

// parseBytes reads one of docker's sizes, such as 1.2GiB; "" and -- are 0.
func parseBytes(s string) (int64, error) {
	if s == "" || s == "--" {
		return 0, nil
	}
	n, mult := s, 1.0
	for i, u := range binaryUnits {
		if strings.HasSuffix(s, u) {
			n, mult = strings.TrimSuffix(s, u), float64(int64(1)<<(10*(i+1)))
			break
		}
	}
	if mult == 1 {
		n = strings.TrimSuffix(s, "B")
	}
	f, err := strconv.ParseFloat(n, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("size %q", s)
	}
	return int64(f * mult), nil
}
