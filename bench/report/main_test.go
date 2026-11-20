package main

import (
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
	// Median of medians 83.9 MB is inside 80 MB +5% (84); one outlier run is ignored.
	s := scenario(2, []RunStats{run(83.9, 0.1), run(120, 0.1), run(30, 0.1)})
	if !s.Gates[0].Pass {
		t.Errorf("rss gate: %+v", s.Gates[0])
	}
	s = scenario(2, []RunStats{run(84.5, 0.1), run(85, 0.1), run(30, 0.1)})
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
		Label: label(true, true), Scenarios: []ScenarioResult{scenario(2, []RunStats{{VmRSSMedian: 90 * mb, IdleCPUPercent: 0.1}})},
		Gates: binaryGates(10 * mb)}
	r.evaluate()
	if r.Pass {
		t.Error("90 MB must fail scenario 2")
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
	if s := scenario(4, run(52.4)); !s.Gates[0].Pass {
		t.Errorf("52.4 MB should pass scenario 4: %+v", s.Gates)
	}
	if s := scenario(4, run(53)); s.Gates[0].Pass {
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
