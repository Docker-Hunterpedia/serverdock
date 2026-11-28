package main

import (
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA-1: at T = 59 s the 8-digit code is 94287082, so the 6-digit one is 287082.
func TestTOTPRFC6238Vector(t *testing.T) {
	got, err := totpAt("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", time.Unix(59, 0))
	if err != nil || got != "287082" {
		t.Fatalf("%q %v", got, err)
	}
	if got, _ := totpAt("gezdgnbvgy3tqojqgezdgnbvgy3tqojq", time.Unix(59, 0)); got != "287082" {
		t.Errorf("lower case: %q", got)
	}
	if _, err := totpAt("not base32 !", time.Now()); err == nil {
		t.Error("expected an error")
	}
}

func TestExposeAndDeployValidateTheirArguments(t *testing.T) {
	c := &client{}
	for _, n := range []int{0, 101} {
		if err := c.expose(n, "img", time.Second); err == nil {
			t.Errorf("n=%d accepted", n)
		}
	}
	if err := c.expose(1, "", time.Second); err == nil {
		t.Error("an empty image was accepted")
	}
	if err := c.deploy([]string{""}, time.Second); err == nil {
		t.Error("an empty service list was accepted")
	}
}
