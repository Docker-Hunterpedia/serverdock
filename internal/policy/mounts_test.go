package policy

import "testing"

func TestMountAndLimitValidators(t *testing.T) {
	for _, ok := range []string{"/data", "/var/lib/x", "/a-b_c.d", "/devices", "/processing"} {
		if err := ValidateMountTarget(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/", "data", "/a/", "/a//b", "/a/./b", "/a/../b", "/proc", "/proc/1", "/sys/x", "/dev", "/dev/shm", "/a b", "/a:b", "/a\tb", "/é", "/\x7f"} {
		if ValidateMountTarget(bad) == nil {
			t.Errorf("accepted target %q", bad)
		}
	}
	if err := ValidateMountTarget("/" + string(make([]byte, 0)) + repeat("a", 254)); err != nil {
		t.Errorf("255 bytes: %v", err)
	}
	if ValidateMountTarget("/"+repeat("a", 255)) == nil {
		t.Error("256 bytes accepted")
	}
	for _, ok := range []string{"a", "data", "a-", "a1-b2", repeat("a", 30)} {
		if err := ValidateVolumeName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1a", "-a", "A", "a_b", "a.b", repeat("a", 31)} {
		if ValidateVolumeName(bad) == nil {
			t.Errorf("accepted volume name %q", bad)
		}
	}
	if ValidateBindSourceShape("/srv/a,b") == nil || ValidateBindSourceShape("/srv/a:b") == nil || ValidateBindSourceShape("/srv/ab") != nil {
		t.Error("bind source separators")
	}
	if ValidateMemoryMB(31) == nil || ValidateMemoryMB(32) != nil {
		t.Error("memory limit bounds")
	}
	if ValidateCPUsMilli(49) == nil || ValidateCPUsMilli(50) != nil || ValidateCPUsMilli(75) == nil || ValidateCPUsMilli(1500) != nil {
		t.Error("cpu limit bounds")
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
