package agent

import (
	"strings"
	"testing"
	"time"
)

func TestParseOp(t *testing.T) {
	good := `{"op":"logs","container":"my-app_1.x","tail":100}`
	op, err := ParseOp([]byte(good))
	if err != nil || op.Container != "my-app_1.x" || op.Tail != 100 {
		t.Fatalf("%+v %v", op, err)
	}
	bad := []string{
		``, `not json`,
		`{"op":"exec","container":"a","tail":1}`,
		`{"op":"logs","container":"","tail":1}`,
		`{"op":"logs","container":"-bad","tail":1}`,
		`{"op":"logs","container":"a/b","tail":1}`,
		`{"op":"logs","container":"a b","tail":1}`,
		`{"op":"logs","container":"../x","tail":1}`,
		`{"op":"logs","container":"` + strings.Repeat("a", 129) + `","tail":1}`,
		`{"op":"logs","container":"a","tail":0}`,
		`{"op":"logs","container":"a","tail":5001}`,
		`{"op":"logs","container":"a","tail":-1}`,
		`{"op":"logs","container":"a","tail":1,"extra":1}`,
		`{"op":"logs","container":"a","tail":1} {"op":"logs"}`,
		strings.Repeat(" ", MaxOpLine+1),
	}
	for _, b := range bad {
		if _, err := ParseOp([]byte(b)); err == nil {
			t.Errorf("expected error for %.60q", b)
		}
	}
	for _, tail := range []int{MinTail, MaxTail} {
		if err := (Op{Op: OpLogs, Container: "a", Tail: tail}).Validate(); err != nil {
			t.Errorf("tail %d: %v", tail, err)
		}
	}
}

func TestBackoffSchedule(t *testing.T) {
	b := Backoff{Min: time.Second, Max: 30 * time.Second, Rand: func() float64 { return 0.999999 }}
	var got []time.Duration
	for i := 0; i < 8; i++ {
		got = append(got, b.Next().Round(time.Second))
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("schedule %v want %v", got, want)
		}
	}
	b.Reset()
	if d := b.Next().Round(time.Second); d != time.Second {
		t.Fatalf("after reset: %v", d)
	}
	z := Backoff{Min: time.Second, Max: time.Minute, Rand: func() float64 { return 0 }}
	if z.Next() != 0 {
		t.Fatal("full jitter lower bound should be 0")
	}
	for i := 0; i < 100; i++ { // no overflow far past the cap
		b.Next()
	}
	if b.Ceiling() != 30*time.Second {
		t.Fatalf("ceiling %v", b.Ceiling())
	}
}

// Every op not on the read-only allowlist must be LocalOnly. Adding an op
// without LocalOnly fails here until security has reviewed it as read-only.
func TestOpSpecTableLocalOnlyRule(t *testing.T) {
	readOnly := map[string]bool{OpLogs: true}
	seen := map[string]bool{}
	for _, s := range Specs() {
		if s.Name == "" || seen[s.Name] {
			t.Errorf("bad or duplicate op name %q", s.Name)
		}
		seen[s.Name] = true
		if !readOnly[s.Name] && !s.LocalOnly {
			t.Errorf("op %q is not LocalOnly and not on the read-only allowlist", s.Name)
		}
		if readOnly[s.Name] && s.LocalOnly {
			t.Errorf("read-only op %q is marked LocalOnly (update the allowlist deliberately)", s.Name)
		}
	}
	if !seen[OpLogs] {
		t.Error("logs missing from the op table")
	}
}

func TestValidateURL(t *testing.T) {
	for _, u := range []string{"ws://127.0.0.1:80/agent/connect", "ws://localhost/agent/connect", "ws://[::1]:80/x", "ws://127.9.9.9/x", "wss://x.example/agent/connect"} {
		if err := ValidateURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	// ws:// is for loopback literals only.
	for _, u := range []string{"ws://x.example/agent/connect", "ws://10.0.0.5/x", "ws://0.0.0.0/x", "ws://localhost.evil.example/x"} {
		if err := ValidateURL(u); err == nil {
			t.Errorf("%s: cleartext to a non-loopback host accepted", u)
		}
	}
	for _, u := range []string{"http://x/", "ftp://x", "ws://", "::bad"} {
		if err := ValidateURL(u); err == nil {
			t.Errorf("%s should fail", u)
		}
	}
}

func TestClientConfigValidate(t *testing.T) {
	fp := "sha256:" + strings.Repeat("ab", 32)
	ok := []ClientConfig{
		{URL: "wss://hub.example:7443/agent/connect", CAFingerprint: fp},
		{URL: "ws://127.0.0.1:80/agent/connect"},
	}
	for _, c := range ok {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", c.URL, err)
		}
	}
	bad := []ClientConfig{
		{URL: "wss://hub.example/agent/connect"},                             // no fingerprint
		{URL: "wss://hub.example/agent/connect", CAFingerprint: "sha256:00"}, // bad fingerprint
		{URL: "ws://hub.example/agent/connect", CAFingerprint: fp},           // cleartext to a remote host
	}
	for _, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("%s accepted", c.URL)
		}
	}
}
