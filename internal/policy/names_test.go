package policy

import (
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	for _, s := range []string{"a", "web", "my-app-2", "a1", strings.Repeat("a", 30), "a--b"} {
		if err := ValidateName(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{"", "A", "Web", "1a", "-a", "a-", "a_b", "a.b", "a b", "é", strings.Repeat("a", 31), "a\x00", "a\n", "a/b", "ab-​"} {
		if err := ValidateName(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func FuzzName(f *testing.F) {
	for _, s := range []string{"a", "web-1", "", "A", "a-", strings.Repeat("a", 31)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if ValidateName(s) != nil {
			return
		}
		if len(s) < 1 || len(s) > 30 || s[0] < 'a' || s[0] > 'z' || s[len(s)-1] == '-' {
			t.Fatalf("accepted %q", s)
		}
		for _, r := range s {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				t.Fatalf("accepted %q", s)
			}
		}
		// A container name built from two valid names is a valid Docker name.
		if n := "sd-" + s + "-" + s + "-d9223372036854775807-a99"; len(n) > 128 {
			t.Fatalf("too long: %d", len(n))
		}
	})
}

func TestValidateHealthPath(t *testing.T) {
	for _, s := range []string{"/", "/healthz", "/a/b?x=1&y=2", "/" + strings.Repeat("a", 255)} {
		if err := ValidateHealthPath(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{"", "healthz", "/a b", "/a\tb", "/a\nb", "/é", "/a#b", "/a\\b", "/\x7f", "/" + strings.Repeat("a", 256), "http://x/"} {
		if err := ValidateHealthPath(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func FuzzHealthPath(f *testing.F) {
	for _, s := range []string{"/", "/x", "", "a", "/a b", "/a#"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if ValidateHealthPath(s) != nil {
			return
		}
		if len(s) == 0 || len(s) > 256 || s[0] != '/' {
			t.Fatalf("accepted %q", s)
		}
		for i := 0; i < len(s); i++ {
			if c := s[i]; c <= ' ' || c >= 0x7f || c == '#' || c == '\\' {
				t.Fatalf("accepted %q", s)
			}
		}
	})
}

func TestRangesAndCaps(t *testing.T) {
	check := func(name string, fn func(int) error, lo, hi int) {
		t.Helper()
		for _, v := range []int{lo, hi, (lo + hi) / 2} {
			if err := fn(v); err != nil {
				t.Errorf("%s(%d): %v", name, v, err)
			}
		}
		for _, v := range []int{lo - 1, hi + 1, -1 << 31, 1 << 31} {
			if err := fn(v); err == nil {
				t.Errorf("%s(%d) accepted", name, v)
			}
		}
	}
	check("container port", ValidateContainerPort, 1, 65535)
	check("health timeout", ValidateHealthTimeoutS, 5, 600)
	check("health passes", ValidateHealthPasses, 1, 10)
	check("drain", ValidateDrainS, 10, 300)
	check("stop grace", ValidateStopGraceS, 0, 300)
	if PidsLimit != 4096 || MaxServices != 100 || MinFreeBytes != 1<<30 {
		t.Error("a resource cap changed")
	}
	if CheckServiceCount(99) != nil || CheckServiceCount(100) == nil || CheckServiceCount(101) == nil {
		t.Error("the 100-service cap")
	}
	if CheckFreeDisk(1<<30) != nil || CheckFreeDisk(1<<30-1) == nil || CheckFreeDisk(0) == nil {
		t.Error("the 1 GiB free-disk check")
	}
}

func TestValidatePublicPort(t *testing.T) {
	own := []int{8080, 8443, 7443}
	for _, p := range []int{1024, 3000, 9000, 65535} {
		if err := ValidatePublicPort(p, own...); err != nil {
			t.Errorf("%d: %v", p, err)
		}
	}
	for _, p := range []int{0, -1, 1, 22, 80, 443, 1023, 65536, 8080, 8443, 7443} {
		if err := ValidatePublicPort(p, own...); err == nil {
			t.Errorf("%d accepted", p)
		}
	}
}

func TestUserLabelsRefuseTheReservedNamespaceInAnyCase(t *testing.T) {
	if err := ValidateUserLabels(map[string]string{"com.example.team": "web", "a": ""}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"serverdock.managed", "serverdock.x", "SERVERDOCK.MANAGED", "ServerDock.Instance", "serverdock."} {
		if ValidateUserLabels(map[string]string{k: "1"}) == nil {
			t.Errorf("%q accepted", k)
		}
	}
	for name, l := range map[string]map[string]string{
		"empty key": {"": "1"}, "space in key": {"a b": "1"}, "control in value": {"a": "x\x1b[2J"},
		"long value": {"a": strings.Repeat("v", 257)}, "long key": {strings.Repeat("k", 129): "1"},
	} {
		if ValidateUserLabels(l) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	many := map[string]string{}
	for i := 0; i < 33; i++ {
		many[strings.Repeat("k", i+1)] = "1"
	}
	if ValidateUserLabels(many) == nil {
		t.Error("33 labels accepted")
	}
}

func TestCheckImage(t *testing.T) {
	rep, err := CheckImage(ImageFacts{LabelKeys: []string{"org.opencontainers.image.title"}, Volumes: []string{"/var/lib/data", "/x\x1b[2J"}, ExposedPorts: []string{"53/udp", "8080/tcp", "9090/tcp"}})
	if err != nil || rep.SuggestedPort != 8080 || len(rep.Warnings) != 2 {
		t.Fatalf("%+v %v", rep, err)
	}
	if !strings.Contains(rep.Warnings[0], "/var/lib/data is not kept between deploys until persistent volumes arrive") || strings.ContainsRune(rep.Warnings[1], 0x1b) {
		t.Errorf("warnings %q", rep.Warnings)
	}
	for _, k := range []string{"serverdock.managed", "SERVERDOCK.Instance", "ServerDock.x"} {
		if _, err := CheckImage(ImageFacts{LabelKeys: []string{"a", k}}); err == nil {
			t.Errorf("image with label %q accepted", k)
		}
	}
	if rep, err := CheckImage(ImageFacts{}); err != nil || rep.SuggestedPort != 0 || len(rep.Warnings) != 0 {
		t.Errorf("%+v %v", rep, err)
	}
	vols := make([]string, 20)
	for i := range vols {
		vols[i] = "/v"
	}
	if rep, _ := CheckImage(ImageFacts{Volumes: vols}); len(rep.Warnings) != 8 {
		t.Errorf("%d warnings", len(rep.Warnings))
	}
}
