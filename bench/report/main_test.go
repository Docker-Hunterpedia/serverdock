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
	run := func(rssMiB, cpu float64) RunStats {
		return RunStats{VmRSSMedian: rssMiB * mib, IdleCPUPercent: cpu}
	}
	// Median of medians 41.9 MiB is inside 40 MiB +5% (42); one outlier run is ignored.
	s := scenario(1, []RunStats{run(41.9, 0.1), run(60, 0.1), run(30, 0.1)})
	if !s.Gates[0].Pass {
		t.Errorf("rss gate: %+v", s.Gates[0])
	}
	s = scenario(1, []RunStats{run(42.5, 0.1), run(43, 0.1), run(30, 0.1)})
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
	if g := binaryGates(41 * mib); g[0].Pass {
		t.Error("gzip gate should fail at 41 MiB")
	}
}

func TestReportLabelsAndVerdict(t *testing.T) {
	r := Report{Date: "d", Arch: "a", Mode: "helper", Quick: true, Informational: true,
		Label: label(true, true), Scenarios: []ScenarioResult{scenario(2, []RunStats{{VmRSSMedian: 90 * mib, IdleCPUPercent: 0.1}})},
		Gates: binaryGates(10 * mib)}
	r.evaluate()
	if r.Pass {
		t.Error("90 MiB must fail scenario 2")
	}
	md := markdown(r)
	for _, want := range []string{"quick", "informational", "FAIL", "not enforced"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
}
