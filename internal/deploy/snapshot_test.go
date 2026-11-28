package deploy

import (
	"encoding/json"
	"strings"
	"testing"
)

const v1JSON = `{"v":1,"image_ref":"nginx:1.27","container_port":80,"health":{"kind":"http","path":"/","timeout_s":60,"passes":3},"strategy":"blue_green","drain_s":10,"stop_grace_s":10}`

// A snapshot an M3a server wrote still parses: it is returned as version 2 with
// nothing new set, and it is written back as version 2.
func TestSnapshotV1ReadsAsV2(t *testing.T) {
	s, err := ParseSnapshot(v1JSON)
	if err != nil {
		t.Fatal(err)
	}
	if s.V != 2 || s.ImageRef != "nginx:1.27" || s.Strategy != "blue_green" || s.ContainerPort != 80 {
		t.Errorf("%+v", s)
	}
	if s.EnvVersion != 0 || s.EnvVersionID != 0 || s.StrategyForced || len(s.Mounts) != 0 || s.MemoryMB != 0 || s.CPUsMilli != 0 || s.NoNewPrivileges {
		t.Errorf("a v1 snapshot has v2 fields set: %+v", s)
	}
	raw, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), `{"v":2,`) {
		t.Errorf("not written as v2: %s", raw)
	}
	if strings.Contains(string(raw), "mounts") || strings.Contains(string(raw), "env_version") {
		t.Errorf("empty v2 fields were written: %s", raw)
	}
	// Marshal always writes the current version, even for a value built with V: 1.
	old := s
	old.V = 1
	if raw, _ := old.Marshal(); !strings.HasPrefix(string(raw), `{"v":2,`) {
		t.Errorf("%s", raw)
	}
	// A v1 snapshot cannot smuggle in v2 fields, and unknown keys are still refused.
	for _, bad := range []string{
		strings.Replace(v1JSON, `"strategy"`, `"memory_mb":64,"strategy"`, 1),
		strings.Replace(v1JSON, `"strategy"`, `"mounts":[],"env_version":1,"env_version_id":2,"strategy"`, 1),
		strings.Replace(v1JSON, `"strategy"`, `"no_new_privileges":true,"strategy"`, 1),
		strings.Replace(v1JSON, `"strategy"`, `"surprise":1,"strategy"`, 1),
		strings.Replace(v1JSON, `"v":1`, `"v":3`, 1),
	} {
		if _, err := ParseSnapshot(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func v2Base() Snapshot {
	return Snapshot{V: 2, ImageRef: "nginx:1.27", ContainerPort: 80, Health: SnapHealth{Kind: "tcp", TimeoutS: 60, Passes: 3},
		Strategy: "blue_green", DrainS: 10, StopGraceS: 10}
}

func TestSnapshotV2Validate(t *testing.T) {
	full := v2Base()
	full.Strategy = "stop_first"
	full.StrategyForced = true
	full.EnvVersion, full.EnvVersionID = 3, 17
	full.Mounts = []SnapMount{{Kind: "volume", Source: "data", Target: "/data"}, {Kind: "bind", Source: "/srv/binds/x", Target: "/files", ReadOnly: true}}
	full.MemoryMB, full.CPUsMilli, full.NoNewPrivileges = 256, 1500, true
	if err := full.Validate(); err != nil {
		t.Fatalf("a full v2 snapshot: %v", err)
	}
	raw, err := full.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseSnapshot(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if b1, _ := json.Marshal(back); string(b1) != string(raw) {
		t.Errorf("round trip changed it:\n%s\n%s", raw, b1)
	}

	ro := v2Base() // a read-only volume keeps blue-green
	ro.Mounts = []SnapMount{{Kind: "volume", Source: "data", Target: "/data", ReadOnly: true}}
	if err := ro.Validate(); err != nil {
		t.Errorf("a read-only mount with blue_green: %v", err)
	}

	cases := map[string]func(s *Snapshot){
		"env version without an id":       func(s *Snapshot) { s.EnvVersion = 1 },
		"env id without a version":        func(s *Snapshot) { s.EnvVersionID = 1 },
		"negative env version":            func(s *Snapshot) { s.EnvVersion, s.EnvVersionID = -1, -1 },
		"writable mount with blue_green":  func(s *Snapshot) { s.Mounts = []SnapMount{{Kind: "volume", Source: "data", Target: "/data"}} },
		"forced without a writable mount": func(s *Snapshot) { s.Strategy, s.StrategyForced = "stop_first", true },
		"forced with blue_green":          func(s *Snapshot) { s.StrategyForced = true },
		"unknown mount kind":              func(s *Snapshot) { s.Mounts = []SnapMount{{Kind: "tmpfs", Source: "x", Target: "/x", ReadOnly: true}} },
		"bad volume name": func(s *Snapshot) {
			s.Mounts = []SnapMount{{Kind: "volume", Source: "Data", Target: "/d", ReadOnly: true}}
		},
		"bind source not absolute": func(s *Snapshot) {
			s.Mounts = []SnapMount{{Kind: "bind", Source: "srv/x", Target: "/d", ReadOnly: true}}
		},
		"bind source with ..": func(s *Snapshot) {
			s.Mounts = []SnapMount{{Kind: "bind", Source: "/srv/../etc", Target: "/d", ReadOnly: true}}
		},
		"bind source not canonical": func(s *Snapshot) {
			s.Mounts = []SnapMount{{Kind: "bind", Source: "/srv//x/", Target: "/d", ReadOnly: true}}
		},
		"bind source with a control char": func(s *Snapshot) {
			s.Mounts = []SnapMount{{Kind: "bind", Source: "/srv/x\n", Target: "/d", ReadOnly: true}}
		},
		"target is /": func(s *Snapshot) { s.Mounts = []SnapMount{{Kind: "volume", Source: "d", Target: "/", ReadOnly: true}} },
		"target under /proc": func(s *Snapshot) {
			s.Mounts = []SnapMount{{Kind: "volume", Source: "d", Target: "/proc/x", ReadOnly: true}}
		},
		"target with a colon": func(s *Snapshot) {
			s.Mounts = []SnapMount{{Kind: "volume", Source: "d", Target: "/a:b", ReadOnly: true}}
		},
		"target with a space": func(s *Snapshot) {
			s.Mounts = []SnapMount{{Kind: "volume", Source: "d", Target: "/a b", ReadOnly: true}}
		},
		"relative target": func(s *Snapshot) {
			s.Mounts = []SnapMount{{Kind: "volume", Source: "d", Target: "data", ReadOnly: true}}
		},
		"duplicate target": func(s *Snapshot) {
			s.Mounts = []SnapMount{{Kind: "volume", Source: "a", Target: "/d", ReadOnly: true}, {Kind: "volume", Source: "b", Target: "/d", ReadOnly: true}}
		},
		"memory below 32":          func(s *Snapshot) { s.MemoryMB = 31 },
		"cpu below 50":             func(s *Snapshot) { s.CPUsMilli = 25 },
		"cpu not a multiple of 50": func(s *Snapshot) { s.CPUsMilli = 120 },
		"eleven mounts": func(s *Snapshot) {
			for i := 0; i < 11; i++ {
				s.Mounts = append(s.Mounts, SnapMount{Kind: "volume", Source: "v" + string(rune('a'+i)), Target: "/m" + string(rune('a'+i)), ReadOnly: true})
			}
		},
	}
	for name, mut := range cases {
		s := v2Base()
		mut(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Ten mounts are fine.
	ten := v2Base()
	for i := 0; i < 10; i++ {
		ten.Mounts = append(ten.Mounts, SnapMount{Kind: "volume", Source: "v" + string(rune('a'+i)), Target: "/m" + string(rune('a'+i)), ReadOnly: true})
	}
	if err := ten.Validate(); err != nil {
		t.Errorf("ten mounts: %v", err)
	}
	// The settings that decide the job's timeout follow the effective strategy.
	if full.Settings().Strategy != "stop_first" {
		t.Errorf("%+v", full.Settings())
	}
}
