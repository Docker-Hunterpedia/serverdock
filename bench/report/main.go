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
	// Tripwire marks a warning gate: never fails the run, needs a written reason.
	Tripwire bool `json:"tripwire,omitempty"`
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
	Deploys       []DeployResult   `json:"deploy_runs,omitempty"`
	Gates         []Gate           `json:"binary_gates"`
	Pass          bool             `json:"pass"`
}

var scenarioNames = map[int]string{
	1: "server, --proxy=false (report only, ADR 0008)",
	2: "single node: server + local agent + Caddy",
	3: "remote agent without proxy (report only until the slim agent, ADR 0008)",
	4: "remote node: agent + its own Caddy",
	// Step 15 (plan 0004): the S2 live-watcher server with real apps behind public ports.
	5: "S2 with one exposed service (single node, one app container of the test image behind a public port)",
	6: "S2 with 10 exposed idle services (report only: single node, ten app containers behind public ports)",
}

// Deploy scenarios (report only): RSS of the S2 server sampled every second around a deploy.
const (
	deployOne = 7 // one deploy of one service
	deployTwo = 8 // two deploys of two services, queued at once
)

func isDeployScenario(id int) bool { return id == deployOne || id == deployTwo }

type limits struct {
	rssMB float64
	cpu   float64
}

// Gated scenarios are the two shapes people deploy (ADR 0008): a single node
// and a remote node with its own proxy. Hard ceilings per ADR 0011.
var scenarioLimits = map[int]limits{
	2: {200, 1.0},
	4: {100, 0.5},
	5: {200, 1.0}, // "S2 with one exposed service": the S2 ceiling
}

// tripwireRSS is the early-warning size per gated scenario (ADR 0011): going
// above it never fails the run, but the report flags it and the PR must give
// a written reason (footprint review).
var tripwireRSS = map[int]float64{2: 80, 4: 50, 5: 80}

// hardRSS is an absolute memory ceiling (no tolerance). Empty until the slim
// agent build (Phase 2) brings back scenario 3 with its 20 MB / 25 MB hard gate.
var hardRSS = map[int]float64{}

// reportRSS is a target shown for report-only scenarios; it never fails the run.
// Scenario 1 and 3 numbers are mostly file-backed pages of the shared binary.
var reportRSS = map[int]float64{1: 40, 3: 20, 6: 80} // 6: ten exposed services, reported against the S2 tripwire

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
	if t, ok := tripwireRSS[id]; ok {
		r.Gates = append(r.Gates, Gate{Name: "VmRSS tripwire (warning)", Value: r.VmRSSMedian / mb, Limit: t, Unit: "MB",
			Pass: r.VmRSSMedian <= t*mb, Report: true, Tripwire: true})
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
	writeDeploys(&b, r.Deploys)
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
			if g.Tripwire {
				res = "⚠ above tripwire: give a written reason in the PR (ADR 0011)"
			}
		}
		fmt.Fprintf(b, "| %s | %.2f %s | %.2f %s | %s |\n", g.Name, g.Value, g.Unit, g.Limit, g.Unit, res)
	}
	b.WriteString("\n")
}

// deployBackWithinBytes is how close to the idle baseline the RSS must be 60 s after a
// deploy for the report to say "back to baseline" (1 MB, decimal). Report only.
const deployBackWithinBytes = 1 * mb

// DeployRun is one run of a deploy scenario: the S2 server sampled every second before,
// during and for 60 s after the deploy.
type DeployRun struct {
	Samples            int     `json:"samples"`
	DeployS            float64 `json:"deploy_seconds"`
	BaselineRSS        float64 `json:"baseline_vmrss_median_bytes"`
	PeakRSSSampled     float64 `json:"peak_vmrss_sampled_during_bytes"`
	HWMBefore          float64 `json:"vmhwm_before_bytes"`
	HWMAfter           float64 `json:"vmhwm_after_bytes"`
	HWMReset           bool    `json:"vmhwm_reset_before_deploy"` // the bench reset the high-water mark just before the deploy
	NewHighWater       bool    `json:"new_high_water_mark"`
	PeakRSS            float64 `json:"peak_vmrss_bytes"` // the higher of the sampled peak and a new VmHWM
	RSSAfter60         float64 `json:"vmrss_at_plus_60s_bytes"`
	PeakOverBaseline   float64 `json:"peak_over_baseline_bytes"`
	After60OverBase    float64 `json:"plus_60s_over_baseline_bytes"`
	BackAtBaseline     bool    `json:"back_to_baseline"`
	SecondsAfterFinish float64 `json:"seconds_sampled_after_finish"`
}

// DeployResult aggregates the runs of one deploy scenario (report only, never a gate).
type DeployResult struct {
	ID   int         `json:"id"`
	Name string      `json:"name"`
	Runs []DeployRun `json:"runs"`
}

// deployRun reads one run: s holds the samples, startOff and endOff are the seconds from the
// start of sampling at which the deploy was queued and finished (host clock differences
// between them, so they hold on Docker Desktop too), postS is the length of the window
// after the finish that the run asked for.
func deployRun(s []Sample, startOff, endOff, postS float64, hwmReset bool) (DeployRun, error) {
	if len(s) < 5 {
		return DeployRun{}, fmt.Errorf("need at least 5 samples, got %d", len(s))
	}
	if startOff <= 0 || endOff < startOff {
		return DeployRun{}, fmt.Errorf("bad deploy window %.1f..%.1f", startOff, endOff)
	}
	t0 := s[0].T
	tStart, tEnd := t0+startOff, t0+endOff
	var before, during, tail []Sample
	for _, x := range s {
		switch {
		case x.T < tStart:
			before = append(before, x)
		case x.T <= tEnd+1: // the sample that straddles the finish still belongs to the deploy
			during = append(during, x)
		}
		if x.T >= tEnd+postS-5 && x.T <= tEnd+postS+1 {
			tail = append(tail, x) // the last 5 s of the 60 s window
		}
	}
	if len(before) < 3 || len(during) == 0 || len(tail) == 0 {
		return DeployRun{}, fmt.Errorf("the samples do not cover the baseline (%d), the deploy (%d) and +%.0f s (%d)", len(before), len(during), postS, len(tail))
	}
	rss := func(v []Sample) []float64 {
		o := make([]float64, len(v))
		for i, x := range v {
			o[i] = x.VmRSS
		}
		return o
	}
	d := DeployRun{Samples: len(s), DeployS: endOff - startOff, SecondsAfterFinish: s[len(s)-1].T - tEnd}
	d.BaselineRSS = median(rss(before))
	d.PeakRSSSampled = max(rss(during))
	d.HWMBefore = before[len(before)-1].VmHWM
	d.HWMAfter = tail[len(tail)-1].VmHWM
	d.HWMReset = hwmReset
	d.NewHighWater = d.HWMAfter > d.HWMBefore
	if hwmReset { // VmHWM now holds the peak since the reset, whatever it was before
		d.HWMBefore, d.NewHighWater = 0, true
	}
	d.PeakRSS = d.PeakRSSSampled
	if d.NewHighWater && d.HWMAfter > d.PeakRSS {
		d.PeakRSS = d.HWMAfter // the kernel saw a higher peak than the 1 s samples
	}
	d.RSSAfter60 = median(rss(tail))
	d.PeakOverBaseline = d.PeakRSS - d.BaselineRSS
	d.After60OverBase = d.RSSAfter60 - d.BaselineRSS
	d.BackAtBaseline = d.After60OverBase <= deployBackWithinBytes
	return d, nil
}

// readEvents reads "<start offset> <end offset> <post seconds> [hwm reset 0|1]" from a run's ".events" file.
func readEvents(path string) (start, end, post float64, reset bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, false, err
	}
	var r int
	n, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%g %g %g %d", &start, &end, &post, &r)
	if n < 3 {
		return 0, 0, 0, false, fmt.Errorf("%s: %w", path, err)
	}
	return start, end, post, r == 1, nil
}

func loadDeploys(dir string) ([]DeployResult, error) {
	var out []DeployResult
	for _, id := range []int{deployOne, deployTwo} {
		files, err := filepath.Glob(filepath.Join(dir, fmt.Sprintf("s%d-r*.tsv", id)))
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			continue
		}
		sort.Strings(files)
		res := DeployResult{ID: id, Name: map[int]string{
			deployOne: "RSS around one deploy (S2 live watcher, 2 exposed services, one redeployed; report only)",
			deployTwo: "RSS around two concurrent deploys (S2 live watcher, 2 exposed services, both redeployed at once; report only)",
		}[id]}
		for _, f := range files {
			fh, err := os.Open(f)
			if err != nil {
				return nil, err
			}
			samples, err := parseTSV(fh)
			fh.Close()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			a, b, post, reset, err := readEvents(f + ".events")
			if err != nil {
				return nil, err
			}
			r, err := deployRun(samples, a, b, post, reset)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			res.Runs = append(res.Runs, r)
		}
		out = append(out, res)
	}
	return out, nil
}

func writeDeploys(b *strings.Builder, ds []DeployResult) {
	for _, d := range ds {
		fmt.Fprintf(b, "## Scenario %d: %s\n\n", d.ID, d.Name)
		fmt.Fprintf(b, "| run | deploy s | idle baseline MB | peak MB (during) | VmHWM before MB | VmHWM +60 s MB | new high-water mark | VmRSS at +60 s MB | +60 s over baseline MB | back to baseline (within 1 MB) |\n|---|---|---|---|---|---|---|---|---|---|\n")
		var peaks, afters, bases []float64
		for i, r := range d.Runs {
			before := fmt.Sprintf("%.1f", r.HWMBefore/mb)
			if r.HWMReset {
				before = "reset"
			}
			fmt.Fprintf(b, "| %d | %.1f | %.1f | %.1f | %s | %.1f | %v | %.1f | %+.1f | %v |\n", i+1, r.DeployS, r.BaselineRSS/mb, r.PeakRSS/mb,
				before, r.HWMAfter/mb, r.NewHighWater, r.RSSAfter60/mb, r.After60OverBase/mb, r.BackAtBaseline)
			peaks, afters, bases = append(peaks, r.PeakRSS), append(afters, r.RSSAfter60), append(bases, r.BaselineRSS)
		}
		fmt.Fprintf(b, "\nMedian of runs: baseline **%.1f MB**, peak **%.1f MB** (%+.1f MB), VmRSS 60 s after the deploy finished **%.1f MB** (%+.1f MB). Report only: sampled every 1 s from outside; the peak is the higher of the sampled maximum and a new VmHWM (\"reset\": the bench reset the high-water mark just before the deploy, so VmHWM is the peak since).\n\n",
			median(bases)/mb, median(peaks)/mb, (median(peaks)-median(bases))/mb, median(afters)/mb, (median(afters)-median(bases))/mb)
	}
}

// load reads s<id>-r<n>.tsv files from dir.
func load(dir string) ([]ScenarioResult, string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "s*-r*.tsv"))
	if err != nil {
		return nil, "", err
	}
	byID := map[int][]string{}
	deployFiles, helperDeploy := false, false
	for _, f := range files {
		var id, run int
		if _, err := fmt.Sscanf(filepath.Base(f), "s%d-r%d.tsv", &id, &run); err != nil {
			continue
		}
		if isDeployScenario(id) {
			deployFiles = true
			if m, err := os.ReadFile(f + ".mode"); err == nil && strings.TrimSpace(string(m)) == "helper" {
				helperDeploy = true
			}
			continue // read by loadDeploys
		}
		byID[id] = append(byID[id], f)
	}
	if len(byID) == 0 && !deployFiles {
		return nil, "", fmt.Errorf("no s<N>-r<K>.tsv files in %s", dir)
	}
	var ids []int
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	mode := "direct"
	if helperDeploy {
		mode = "helper"
	}
	var out []ScenarioResult
	docker := map[int]bool{}   // scenarios whose runs had the host's Docker socket: "S2 live watcher" (a ".docker" marker)
	noDocker := map[int]bool{} // S2 runs made without it: "S2 no Docker", report only (a ".nodocker" marker)
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
			if _, err := os.Stat(f + ".docker"); err == nil {
				docker[id] = true
			}
			if _, err := os.Stat(f + ".nodocker"); err == nil {
				noDocker[id] = true
			}
			if m, err := os.ReadFile(f + ".mode"); err == nil && strings.TrimSpace(string(m)) == "helper" {
				mode = "helper"
			}
		}
		sc := scenario(id, runs)
		switch {
		case noDocker[id]:
			// Report only: the shape without the live watcher is never a gate.
			sc.Name += " (S2 no Docker, report only)"
			for i := range sc.Gates {
				sc.Gates[i].Report = true
			}
		case docker[id]:
			// The gated S2 number: same ceiling and tripwire, the server ran against real Docker.
			sc.Name += " (S2 live watcher)"
		}
		out = append(out, sc)
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
	deploys, err := loadDeploys(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		os.Exit(2)
	}
	informational := *info || mode == "helper"
	rep := Report{Date: *date, Arch: *arch, Mode: mode, Quick: *quick, Informational: informational,
		Label: label(*quick, informational), BinaryBytes: *bin, BinaryGzip: *gz, Scenarios: scen, Deploys: deploys, Gates: binaryGates(*gz)}
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
