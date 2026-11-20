// Command report aggregates the bench TSV samples written by bench/measure.sh
// into bench/results/<date>-<arch>.{md,json} and evaluates the gates from plan
// 0001: median of per-run medians, with +5% tolerance on memory and +0.2
// points on CPU.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const mb = 1e6 // decimal megabytes, matching docs/architecture.md

const (
	memTolerance = 1.05
	cpuTolerance = 0.2
)

// Sample is one TSV row.
type Sample struct {
	T                                    float64
	VmRSS, VmHWM, RssAnon, RssFile       float64
	MemCurrent, Anon, File, InactiveFile float64
	UsageUsec                            float64
}

// RunStats summarises one run's idle window.
type RunStats struct {
	Samples        int     `json:"samples"`
	VmRSSMedian    float64 `json:"vmrss_median_bytes"`
	VmRSSMax       float64 `json:"vmrss_max_bytes"`
	VmHWMMax       float64 `json:"vmhwm_max_bytes"`
	RssAnonMedian  float64 `json:"rssanon_median_bytes"`
	RssFileMedian  float64 `json:"rssfile_median_bytes"`
	CgroupMedian   float64 `json:"cgroup_mem_minus_inactive_file_median_bytes"`
	IdleCPUPercent float64 `json:"idle_cpu_percent"`
	WindowSeconds  float64 `json:"window_seconds"`
}

// Gate is one pass/fail evaluation.
type Gate struct {
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Limit  float64 `json:"limit"`
	Unit   string  `json:"unit"`
	Pass   bool    `json:"pass"`
	Report bool    `json:"report_only,omitempty"`
}

// ScenarioResult is the aggregate for one scenario.
type ScenarioResult struct {
	ID          int        `json:"id"`
	Name        string     `json:"name"`
	Runs        []RunStats `json:"runs"`
	VmRSSMedian float64    `json:"vmrss_median_of_medians_bytes"`
	VmHWMMax    float64    `json:"vmhwm_max_bytes"`
	CPUMedian   float64    `json:"idle_cpu_percent_median_of_runs"`
	Gates       []Gate     `json:"gates"`
}

// Report is the JSON document.
type Report struct {
	Date          string           `json:"date"`
	Arch          string           `json:"arch"`
	Mode          string           `json:"mode"` // direct | helper
	Quick         bool             `json:"quick"`
	Informational bool             `json:"informational"`
	Label         string           `json:"label"`
	BinaryBytes   int64            `json:"binary_bytes"`
	BinaryGzip    int64            `json:"binary_gzip9_bytes"`
	Scenarios     []ScenarioResult `json:"scenarios"`
	Gates         []Gate           `json:"binary_gates"`
	Pass          bool             `json:"pass"`
}

var scenarioNames = map[int]string{
	1: "server, --proxy=false (report only, ADR 0008)",
	2: "single node: server + local agent + Caddy",
	3: "remote agent without proxy (report only until the slim agent, ADR 0008)",
	4: "remote node: agent + its own Caddy",
}

type limits struct {
	rssMB float64
	cpu   float64
}

// Gated scenarios are the two shapes people deploy (ADR 0008): a single node
// and a remote node with its own proxy.
var scenarioLimits = map[int]limits{
	2: {80, 1.0},
	4: {50, 0.5},
}

// hardRSS is an absolute memory ceiling (no tolerance). Empty until the slim
// agent build (Phase 2) brings back scenario 3 with its 20 MB / 25 MB hard gate.
var hardRSS = map[int]float64{}

// reportRSS is a target shown for report-only scenarios; it never fails the run.
// Scenario 1 and 3 numbers are mostly file-backed pages of the shared binary.
var reportRSS = map[int]float64{1: 40, 3: 20}

const gzipLimitMB = 40

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func max(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

func parseTSV(r io.Reader) ([]Sample, error) {
	sc := bufio.NewScanner(r)
	var out []Sample
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if first {
			first = false
			if strings.HasPrefix(line, "t_s") {
				continue
			}
		}
		f := strings.Split(line, "\t")
		if len(f) != 10 {
			return nil, fmt.Errorf("bad row %q: want 10 columns", line)
		}
		var v [10]float64
		for i, s := range f {
			x, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return nil, fmt.Errorf("bad row %q: %w", line, err)
			}
			v[i] = x
		}
		out = append(out, Sample{v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7], v[8], v[9]})
	}
	return out, sc.Err()
}

func runStats(s []Sample) (RunStats, error) {
	if len(s) < 2 {
		return RunStats{}, fmt.Errorf("need at least 2 samples, got %d", len(s))
	}
	col := func(f func(Sample) float64) []float64 {
		o := make([]float64, len(s))
		for i, x := range s {
			o[i] = f(x)
		}
		return o
	}
	window := s[len(s)-1].T - s[0].T
	if window <= 0 {
		return RunStats{}, fmt.Errorf("non-positive sample window")
	}
	cpu := (s[len(s)-1].UsageUsec - s[0].UsageUsec) / (window * 1e6) * 100
	return RunStats{
		Samples:        len(s),
		VmRSSMedian:    median(col(func(x Sample) float64 { return x.VmRSS })),
		VmRSSMax:       max(col(func(x Sample) float64 { return x.VmRSS })),
		VmHWMMax:       max(col(func(x Sample) float64 { return x.VmHWM })),
		RssAnonMedian:  median(col(func(x Sample) float64 { return x.RssAnon })),
		RssFileMedian:  median(col(func(x Sample) float64 { return x.RssFile })),
		CgroupMedian:   median(col(func(x Sample) float64 { return x.MemCurrent - x.InactiveFile })),
		IdleCPUPercent: cpu,
		WindowSeconds:  window,
	}, nil
}

func scenario(id int, runs []RunStats) ScenarioResult {
	r := ScenarioResult{ID: id, Name: scenarioNames[id], Runs: runs}
	var rss, cpu, hwm []float64
	for _, x := range runs {
		rss = append(rss, x.VmRSSMedian)
		cpu = append(cpu, x.IdleCPUPercent)
		hwm = append(hwm, x.VmHWMMax)
	}
	r.VmRSSMedian, r.CPUMedian, r.VmHWMMax = median(rss), median(cpu), max(hwm)
	if l, ok := scenarioLimits[id]; ok {
		r.Gates = []Gate{
			{Name: "VmRSS median", Value: r.VmRSSMedian / mb, Limit: l.rssMB * memTolerance, Unit: "MB",
				Pass: r.VmRSSMedian <= l.rssMB*mb*memTolerance},
			{Name: "idle CPU", Value: r.CPUMedian, Limit: l.cpu + cpuTolerance, Unit: "%",
				Pass: r.CPUMedian < l.cpu+cpuTolerance},
		}
		if h, ok := hardRSS[id]; ok {
			r.Gates = append(r.Gates, Gate{Name: "VmRSS hard limit", Value: r.VmRSSMedian / mb, Limit: h, Unit: "MB",
				Pass: r.VmRSSMedian <= h*mb})
		}
	}
	if t, ok := reportRSS[id]; ok {
		r.Gates = append(r.Gates, Gate{Name: "VmRSS target (report only)", Value: r.VmRSSMedian / mb, Limit: t, Unit: "MB",
			Pass: r.VmRSSMedian <= t*mb, Report: true})
	}
	return r
}

func binaryGates(gz int64) []Gate {
	return []Gate{{Name: "server binary gzip -9", Value: float64(gz) / mb, Limit: gzipLimitMB, Unit: "MB",
		Pass: float64(gz) <= gzipLimitMB*mb}}
}

func (r *Report) evaluate() {
	r.Pass = true
	for _, g := range r.Gates {
		r.Pass = r.Pass && g.Pass
	}
	for _, s := range r.Scenarios {
		for _, g := range s.Gates {
			r.Pass = r.Pass && (g.Pass || g.Report)
		}
	}
}

func label(quick, informational bool) string {
	var p []string
	if quick {
		p = append(p, "quick (shortened warm-up/idle/runs; not a valid gate run)")
	}
	if informational {
		p = append(p, "informational (Docker Desktop / helper container path)")
	}
	if len(p) == 0 {
		return "full run"
	}
	return strings.Join(p, "; ")
}

func markdown(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Bench results %s %s\n\n", r.Date, r.Arch)
	fmt.Fprintf(&b, "- Run type: **%s**\n- Measurement path: `%s`\n", r.Label, r.Mode)
	verdict := "PASS"
	if !r.Pass {
		verdict = "FAIL"
	}
	fmt.Fprintf(&b, "- Gates: **%s**%s\n", verdict, map[bool]string{true: " (not enforced: informational)", false: ""}[r.Informational])
	fmt.Fprintf(&b, "- Binary: %.1f MB raw, %.1f MB gzip -9\n", float64(r.BinaryBytes)/mb, float64(r.BinaryGzip)/mb)
	fmt.Fprintf(&b, "- Gates use the median of per-run medians; tolerance +5%% memory, +0.2 points CPU. 1 MB = 1,000,000 bytes.\n\n")
	for _, s := range r.Scenarios {
		fmt.Fprintf(&b, "## Scenario %d: %s\n\n", s.ID, s.Name)
		fmt.Fprintf(&b, "| run | samples | VmRSS median MB | VmRSS max MB | VmHWM max MB | RssAnon MB | RssFile MB | cgroup-inactive MB | idle CPU %% |\n|---|---|---|---|---|---|---|---|---|\n")
		for i, x := range s.Runs {
			fmt.Fprintf(&b, "| %d | %d | %.1f | %.1f | %.1f | %.1f | %.1f | %.1f | %.2f |\n", i+1, x.Samples,
				x.VmRSSMedian/mb, x.VmRSSMax/mb, x.VmHWMMax/mb, x.RssAnonMedian/mb, x.RssFileMedian/mb, x.CgroupMedian/mb, x.IdleCPUPercent)
		}
		fmt.Fprintf(&b, "\nMedian of medians: VmRSS **%.1f MB**, idle CPU **%.2f%%**, VmHWM max %.1f MB\n\n", s.VmRSSMedian/mb, s.CPUMedian, s.VmHWMMax/mb)
		writeGates(&b, s.Gates)
	}
	fmt.Fprintf(&b, "## Binary\n\n")
	writeGates(&b, r.Gates)
	return b.String()
}

func writeGates(b *strings.Builder, gs []Gate) {
	if len(gs) == 0 {
		return
	}
	fmt.Fprintf(b, "| gate | value | limit | result |\n|---|---|---|---|\n")
	for _, g := range gs {
		res := "pass"
		if !g.Pass {
			res = "FAIL"
			if g.Report {
				res = "above target (report only)"
			}
		}
		fmt.Fprintf(b, "| %s | %.2f %s | %.2f %s | %s |\n", g.Name, g.Value, g.Unit, g.Limit, g.Unit, res)
	}
	b.WriteString("\n")
}

// load reads s<id>-r<n>.tsv files from dir.
func load(dir string) ([]ScenarioResult, string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "s*-r*.tsv"))
	if err != nil {
		return nil, "", err
	}
	byID := map[int][]string{}
	for _, f := range files {
		var id, run int
		if _, err := fmt.Sscanf(filepath.Base(f), "s%d-r%d.tsv", &id, &run); err != nil {
			continue
		}
		byID[id] = append(byID[id], f)
	}
	if len(byID) == 0 {
		return nil, "", fmt.Errorf("no s<N>-r<K>.tsv files in %s", dir)
	}
	var ids []int
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	mode := "direct"
	var out []ScenarioResult
	for _, id := range ids {
		sort.Strings(byID[id])
		var runs []RunStats
		for _, f := range byID[id] {
			fh, err := os.Open(f)
			if err != nil {
				return nil, "", err
			}
			samples, err := parseTSV(fh)
			fh.Close()
			if err != nil {
				return nil, "", fmt.Errorf("%s: %w", f, err)
			}
			rs, err := runStats(samples)
			if err != nil {
				return nil, "", fmt.Errorf("%s: %w", f, err)
			}
			runs = append(runs, rs)
			if m, err := os.ReadFile(f + ".mode"); err == nil && strings.TrimSpace(string(m)) == "helper" {
				mode = "helper"
			}
		}
		out = append(out, scenario(id, runs))
	}
	return out, mode, nil
}

func main() {
	dir := flag.String("dir", "", "directory with s<N>-r<K>.tsv samples")
	out := flag.String("out", "", "output path prefix (writes .md and .json)")
	arch := flag.String("arch", "unknown", "architecture label")
	date := flag.String("date", "", "date label")
	bin := flag.Int64("binary-bytes", 0, "raw binary size")
	gz := flag.Int64("binary-gzip-bytes", 0, "gzip -9 binary size")
	quick := flag.Bool("quick", false, "label as quick run")
	info := flag.Bool("informational", false, "do not fail on gate misses")
	flag.Parse()
	if *dir == "" || *out == "" || *gz <= 0 {
		fmt.Fprintln(os.Stderr, "usage: report -dir D -out PREFIX -binary-gzip-bytes N [-arch A -date D -quick -informational]")
		os.Exit(2)
	}
	scen, mode, err := load(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		os.Exit(2)
	}
	informational := *info || mode == "helper"
	rep := Report{Date: *date, Arch: *arch, Mode: mode, Quick: *quick, Informational: informational,
		Label: label(*quick, informational), BinaryBytes: *bin, BinaryGzip: *gz, Scenarios: scen, Gates: binaryGates(*gz)}
	rep.evaluate()
	js, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(*out+".json", append(js, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		os.Exit(2)
	}
	md := markdown(rep)
	if err := os.WriteFile(*out+".md", []byte(md), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		os.Exit(2)
	}
	fmt.Print(md)
	if !rep.Pass && !informational {
		os.Exit(1)
	}
}
