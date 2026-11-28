package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMedian(t *testing.T) {
	if median([]float64{3, 1, 2}) != 2 || median([]float64{1, 2, 3, 4}) != 2.5 || median(nil) != 0 {
		t.Fatal("median wrong")
	}
}

func tsv(rows ...string) string {
	return "t_s\tvmrss\tvmhwm\trssanon\trssfile\tmem_current\tanon\tfile\tinactive_file\tusage_usec\n" + strings.Join(rows, "\n") + "\n"
}

func TestRunStats(t *testing.T) {
	// 100 s window, 500000 usec CPU => 0.5%.
	in := tsv(
		"10.00\t20971520\t25165824\t1\t2\t30000000\t1\t1\t10000000\t1000000",
		"60.00\t31457280\t31457280\t1\t2\t30000000\t1\t1\t10000000\t1250000",
		"110.00\t20971520\t31457280\t1\t2\t30000000\t1\t1\t10000000\t1500000",
	)
	s, err := parseTSV(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	r, err := runStats(s)
	if err != nil {
		t.Fatal(err)
	}
	if r.VmRSSMedian != 20971520 || r.VmRSSMax != 31457280 || r.VmHWMMax != 31457280 {
		t.Errorf("%+v", r)
	}
	if r.IdleCPUPercent < 0.4999 || r.IdleCPUPercent > 0.5001 {
		t.Errorf("cpu = %v", r.IdleCPUPercent)
	}
	if r.CgroupMedian != 20000000 {
		t.Errorf("cgroup = %v", r.CgroupMedian)
	}
}

func TestParseErrors(t *testing.T) {
	if _, err := parseTSV(strings.NewReader("1\t2\n")); err == nil {
		t.Error("expected column error")
	}
	if _, err := runStats([]Sample{{}}); err == nil {
		t.Error("expected too-few-samples error")
	}
}

func TestGates(t *testing.T) {
	run := func(rssMB, cpu float64) RunStats {
		return RunStats{VmRSSMedian: rssMB * mb, IdleCPUPercent: cpu}
	}
	// Median of medians 209.9 MB is inside 200 MB +5% (210); one outlier run is ignored.
	s := scenario(2, []RunStats{run(209.9, 0.1), run(300, 0.1), run(30, 0.1)})
	if !s.Gates[0].Pass {
		t.Errorf("rss gate: %+v", s.Gates[0])
	}
	s = scenario(2, []RunStats{run(210.5, 0.1), run(211, 0.1), run(30, 0.1)})
	if s.Gates[0].Pass {
		t.Errorf("rss gate should fail: %+v", s.Gates[0])
	}
	s = scenario(2, []RunStats{run(50, 1.3), run(50, 1.3), run(50, 0.1)})
	if s.Gates[1].Pass {
		t.Errorf("cpu gate should fail (1.3 >= 1.2): %+v", s.Gates[1])
	}
	s = scenario(2, []RunStats{run(50, 1.1), run(50, 1.1), run(50, 0.1)})
	if !s.Gates[1].Pass {
		t.Errorf("cpu gate should pass with tolerance: %+v", s.Gates[1])
	}
	if g := binaryGates(41 * mb); g[0].Pass {
		t.Error("gzip gate should fail at 41 MB")
	}
}

func TestReportLabelsAndVerdict(t *testing.T) {
	r := Report{Date: "d", Arch: "a", Mode: "helper", Quick: true, Informational: true,
		Label: label(true, true), Scenarios: []ScenarioResult{scenario(2, []RunStats{{VmRSSMedian: 220 * mb, IdleCPUPercent: 0.1}})},
		Gates: binaryGates(10 * mb)}
	r.evaluate()
	if r.Pass {
		t.Error("220 MB must fail scenario 2")
	}
	md := markdown(r)
	for _, want := range []string{"quick", "informational", "FAIL", "not enforced"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
}

func TestScenarioGates(t *testing.T) {
	run := func(rss float64) []RunStats { return []RunStats{{VmRSSMedian: rss * mb, IdleCPUPercent: 0.1}} }
	// Scenario 4 (remote node with proxy) is gated at 50 MB +5%.
	if s := scenario(4, run(104.9)); !s.Gates[0].Pass {
		t.Errorf("104.9 MB should pass scenario 4: %+v", s.Gates)
	}
	if s := scenario(4, run(106)); s.Gates[0].Pass {
		t.Errorf("53 MB should fail scenario 4: %+v", s.Gates)
	}
	// Scenarios 1 and 3 are report only (ADR 0008): over target, the run still passes.
	r := Report{Scenarios: []ScenarioResult{scenario(1, run(70)), scenario(3, run(45))}, Gates: binaryGates(10 * mb)}
	r.evaluate()
	if !r.Pass {
		t.Error("report-only scenarios must not fail the run")
	}
	for _, sc := range r.Scenarios {
		if len(sc.Gates) != 1 || !sc.Gates[0].Report || sc.Gates[0].Pass {
			t.Errorf("scenario %d: want one failing report-only gate, got %+v", sc.ID, sc.Gates)
		}
	}
	if !strings.Contains(markdown(r), "report only") {
		t.Error("markdown should flag report-only")
	}
}

func TestTripwireWarnsButNeverFails(t *testing.T) {
	run := func(rss float64) []RunStats { return []RunStats{{VmRSSMedian: rss * mb, IdleCPUPercent: 0.1}} }
	for _, c := range []struct {
		id  int
		rss float64
	}{{2, 95}, {4, 60}} {
		r := Report{Scenarios: []ScenarioResult{scenario(c.id, run(c.rss))}, Gates: binaryGates(10 * mb)}
		r.evaluate()
		if !r.Pass {
			t.Errorf("scenario %d at %.0f MB is over the tripwire but under the ceiling: must pass", c.id, c.rss)
		}
		if !strings.Contains(markdown(r), "above tripwire") {
			t.Errorf("scenario %d: markdown must flag the tripwire", c.id)
		}
	}
	r := Report{Scenarios: []ScenarioResult{scenario(2, run(70)), scenario(4, run(45))}, Gates: binaryGates(10 * mb)}
	r.evaluate()
	if strings.Contains(markdown(r), "above tripwire") {
		t.Error("under the tripwire must not be flagged")
	}
}

// An S2 run made with the host's Docker socket carries a ".docker" marker: the
// scenario is labelled "S2 live watcher", and its gates are the same ones.
func TestScenarioWithTheLiveWatcherIsLabelledAndGatedAsS2(t *testing.T) {
	dir := t.TempDir()
	body := tsv(
		"10.00\t20971520\t25165824\t1\t2\t30000000\t1\t1\t10000000\t1000000",
		"60.00\t31457280\t31457280\t1\t2\t30000000\t1\t1\t10000000\t1250000",
		"110.00\t20971520\t31457280\t1\t2\t30000000\t1\t1\t10000000\t1500000",
	)
	for _, f := range []string{"s2-r1.tsv", "s4-r1.tsv"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plain, _, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s2-r1.tsv.docker"), []byte("docker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marked, _, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain[0].Name, "S2 ") || !strings.Contains(marked[0].Name, "S2 live watcher") {
		t.Errorf("names %q / %q", plain[0].Name, marked[0].Name)
	}
	if strings.Contains(marked[1].Name, "S2 ") {
		t.Errorf("scenario 4 labelled: %q", marked[1].Name)
	}
	if len(plain[0].Gates) != len(marked[0].Gates) || plain[0].VmRSSMedian != marked[0].VmRSSMedian {
		t.Errorf("gates differ: %v / %v", plain[0].Gates, marked[0].Gates)
	}
}

// An S2 run made without Docker (".nodocker") is labelled and never gates: even a
// result far over the ceiling and the tripwire passes the report.
func TestS2NoDockerIsReportOnly(t *testing.T) {
	dir := t.TempDir()
	body := tsv(
		"10.00\t524288000\t524288000\t1\t2\t30000000\t1\t1\t10000000\t1000000",
		"60.00\t524288000\t524288000\t1\t2\t30000000\t1\t1\t10000000\t1250000",
		"110.00\t524288000\t524288000\t1\t2\t30000000\t1\t1\t10000000\t1500000",
	)
	if err := os.WriteFile(filepath.Join(dir, "s2-r1.tsv"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gated, _, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := Report{Scenarios: gated}
	r.evaluate()
	if r.Pass {
		t.Fatal("an oversized S2 without a marker must fail the gate")
	}
	if err := os.WriteFile(filepath.Join(dir, "s2-r1.tsv.nodocker"), []byte("nodocker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, _, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep[0].Name, "S2 no Docker, report only") {
		t.Errorf("name %q", rep[0].Name)
	}
	r = Report{Scenarios: rep}
	r.evaluate()
	if !r.Pass {
		t.Error("S2 no Docker must not gate")
	}
}

// "S2 with one exposed service" (scenario 5) is gated like S2: the ceiling fails the run,
// the 80 MB tripwire only warns.
func TestScenarioFiveIsGatedLikeS2(t *testing.T) {
	run := func(rssMB float64) RunStats { return RunStats{VmRSSMedian: rssMB * mb, IdleCPUPercent: 0.1} }
	r := Report{Scenarios: []ScenarioResult{scenario(5, []RunStats{run(85)})}}
	r.evaluate()
	if !r.Pass {
		t.Error("85 MB is over the tripwire only: must pass")
	}
	var tripped bool
	for _, g := range r.Scenarios[0].Gates {
		tripped = tripped || (g.Tripwire && !g.Pass)
	}
	if !tripped {
		t.Error("the tripwire did not warn at 85 MB")
	}
	r = Report{Scenarios: []ScenarioResult{scenario(5, []RunStats{run(250)})}}
	r.evaluate()
	if r.Pass {
		t.Error("250 MB is over the ceiling: must fail")
	}
	if !strings.Contains(scenarioNames[5], "S2 with one exposed service") || !strings.Contains(scenarioNames[6], "S2 with 10 exposed idle services") {
		t.Error("the scenario names must carry the plan's labels")
	}
}

// "S2 with 10 exposed idle services" (scenario 6) is report only, even far over the tripwire.
func TestScenarioSixNeverFails(t *testing.T) {
	r := Report{Scenarios: []ScenarioResult{scenario(6, []RunStats{{VmRSSMedian: 500 * mb, IdleCPUPercent: 5}})}}
	r.evaluate()
	if !r.Pass || len(r.Scenarios[0].Gates) != 1 || !r.Scenarios[0].Gates[0].Report {
		t.Errorf("%+v", r.Scenarios[0].Gates)
	}
}

func deploySamples(rss func(t float64) float64, hwm func(t float64) float64, n int) []Sample {
	var out []Sample
	for i := 0; i < n; i++ {
		t := float64(i)
		out = append(out, Sample{T: 1000 + t, VmRSS: rss(t), VmHWM: hwm(t)})
	}
	return out
}

func TestDeployRun(t *testing.T) {
	// Baseline 50 MB for 20 s; the deploy runs 20..40 s with a 56 MB sampled peak and a
	// kernel high-water mark of 58 MB; 52 MB for the 60 s after (back to within 2 MB).
	rss := func(t float64) float64 {
		switch {
		case t < 20:
			return 50 * mb
		case t <= 40:
			return 56 * mb
		}
		return 52 * mb
	}
	hwm := func(t float64) float64 {
		if t < 30 {
			return 51 * mb
		}
		return 58 * mb
	}
	d, err := deployRun(deploySamples(rss, hwm, 105), 20, 40, 60, false)
	if err != nil {
		t.Fatal(err)
	}
	if d.BaselineRSS != 50*mb || d.PeakRSSSampled != 56*mb || d.PeakRSS != 58*mb || !d.NewHighWater {
		t.Errorf("%+v", d)
	}
	if d.RSSAfter60 != 52*mb || d.After60OverBase != 2*mb || d.BackAtBaseline {
		t.Errorf("%+v", d)
	}
	// A flat HWM means the sampled peak is the peak.
	d, err = deployRun(deploySamples(rss, func(float64) float64 { return 60 * mb }, 105), 20, 40, 60, false)
	if err != nil || d.NewHighWater || d.PeakRSS != 56*mb {
		t.Errorf("%+v %v", d, err)
	}
	// With the high-water mark reset before the deploy, VmHWM is the peak since, even when
	// lower than the earlier one (a 55 MB peak after a reset from 60 MB).
	d, err = deployRun(deploySamples(rss, func(float64) float64 { return 55 * mb }, 105), 20, 40, 60, true)
	if err != nil || !d.HWMReset || d.HWMBefore != 0 || !d.NewHighWater || d.PeakRSS != 56*mb {
		t.Errorf("%+v %v", d, err)
	}
	// Too short to hold +60 s.
	if _, err := deployRun(deploySamples(rss, hwm, 70), 20, 40, 60, false); err == nil {
		t.Error("expected an error for samples that stop before +60 s")
	}
	if _, err := deployRun(deploySamples(rss, hwm, 105), 20, 10, 60, false); err == nil {
		t.Error("expected an error for an end before the start")
	}
}

func TestLoadDeploysAndMarkdown(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("t_s\tvmrss\tvmhwm\trssanon\trssfile\tmem_current\tanon\tfile\tinactive_file\tusage_usec\n")
	for i := 0; i < 105; i++ {
		rss := 50e6
		if i >= 20 && i <= 40 {
			rss = 57e6
		}
		fmt.Fprintf(&b, "%d\t%.0f\t%.0f\t1\t2\t3\t1\t1\t1\t%d\n", 1000+i, rss, 57e6, i*1000)
	}
	if err := os.WriteFile(filepath.Join(dir, "s7-r1.tsv"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s7-r1.tsv.events"), []byte("20.5 40.2 60 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scen, _, err := load(dir)
	if err != nil || len(scen) != 0 {
		t.Fatalf("a deploy-only directory is valid: %v %v", scen, err)
	}
	ds, err := loadDeploys(dir)
	if err != nil || len(ds) != 1 || len(ds[0].Runs) != 1 {
		t.Fatalf("%v %v", ds, err)
	}
	r := Report{Deploys: ds}
	r.evaluate()
	md := markdown(r)
	if !r.Pass || !strings.Contains(md, "Scenario 7") || !strings.Contains(md, "report only") || !strings.Contains(md, "60 s after the deploy finished") {
		t.Errorf("%s", md)
	}
	if err := os.Remove(filepath.Join(dir, "s7-r1.tsv.events")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDeploys(dir); err == nil {
		t.Error("a deploy run without its events file must be an error")
	}
}

func TestDeployRunBaselineStabilityAndAnon(t *testing.T) {
	// The baseline climbs 1 MB across its 20 s: not stable. RssAnon is carried through.
	var s []Sample
	for i := 0; i < 105; i++ {
		rss := 50 * mb
		if i < 20 {
			rss += float64(i) / 19 * mb
		} else {
			rss = 52 * mb
		}
		s = append(s, Sample{T: 1000 + float64(i), VmRSS: rss, VmHWM: 60 * mb, RssAnon: 20 * mb})
	}
	d, err := deployRun(s, 20.5, 40, 60, false)
	if err != nil {
		t.Fatal(err)
	}
	if d.BaselineStable || d.BaselineDriftBytes < 0.8*mb {
		t.Errorf("drift %v stable %v", d.BaselineDriftBytes, d.BaselineStable)
	}
	if d.BaselineAnon != 20*mb || d.AnonAfter60 != 20*mb {
		t.Errorf("%+v", d)
	}
	r := Report{Deploys: []DeployResult{{ID: 7, Name: "x", Runs: []DeployRun{d}}}}
	md := markdown(r)
	for _, want := range []string{"VmHWM was not reset", "baseline moved by more than 0.5 MB", "layers are cached", "RssAnon"} {
		if !strings.Contains(md, want) {
			t.Errorf("the report lacks %q", want)
		}
	}
}

// On the direct path the deploy's absolute host uptimes align the window, not the offsets.
func TestLoadDeploysAlignsOnAbsoluteUptimesWhenDirect(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("t_s\tvmrss\tvmhwm\trssanon\trssfile\tmem_current\tanon\tfile\tinactive_file\tusage_usec\n")
	for i := 0; i < 105; i++ {
		rss := 50e6
		if i >= 25 && i <= 45 { // the deploy really ran 25..45 s into the samples
			rss = 58e6
		}
		fmt.Fprintf(&b, "%d\t%.0f\t%.0f\t1\t2\t3\t1\t1\t1\t%d\n", 1000+i, rss, 58e6, i*1000)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("s7-r1.tsv", b.String())
	// The offsets are 3 s late (sampling started early); the absolute uptimes are right.
	write("s7-r1.tsv.events", "28 48 60 1 1025 1045\n")
	write("s7-r1.tsv.mode", "direct\n")
	ds, err := loadDeploys(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := ds[0].Runs[0].BaselineDriftBytes; got != 0 {
		t.Errorf("drift %v: the window was not aligned on the absolute uptimes", got)
	}
	write("s7-r1.tsv.mode", "helper\n")
	ds, err = loadDeploys(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := ds[0].Runs[0].BaselineDriftBytes; got < 7e6 {
		t.Errorf("drift %v: on the helper path the offsets apply, so the late window's baseline holds deploy samples", got)
	}
}
