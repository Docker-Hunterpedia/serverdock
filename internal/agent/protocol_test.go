package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/Docker-Hunterpedia/serverdock/internal/pki"
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

// Only ops on the read-only allowlist may be RemoteOK; the zero value is
// local-only. Marking a new op RemoteOK fails here until security has reviewed
// it as read-only.
func TestOpSpecTableRemoteOKRule(t *testing.T) {
	readOnly := map[string]bool{OpLogs: true, OpHello: true}
	seen := map[string]bool{}
	for _, s := range Specs() {
		if s.Name == "" || seen[s.Name] {
			t.Errorf("bad or duplicate op name %q", s.Name)
		}
		seen[s.Name] = true
		if s.RemoteOK && !readOnly[s.Name] {
			t.Errorf("op %q is RemoteOK but not on the read-only allowlist", s.Name)
		}
		if readOnly[s.Name] && !s.RemoteOK {
			t.Errorf("read-only op %q is not RemoteOK (update the allowlist deliberately)", s.Name)
		}
	}
	if !seen[OpLogs] {
		t.Error("logs missing from the op table")
	}
}

func TestValidateURL(t *testing.T) {
	for _, u := range []string{"ws://127.0.0.1:80/agent/connect", "ws://[::1]:80/x", "ws://127.9.9.9/x", "wss://x.example/agent/connect"} {
		if err := ValidateURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	// ws:// is for loopback literals only.
	for _, u := range []string{"ws://localhost/agent/connect", "ws://x.example/agent/connect", "ws://10.0.0.5/x", "ws://0.0.0.0/x", "ws://localhost.evil.example/x"} {
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

func TestYamuxConfigBounds(t *testing.T) {
	cfg := YamuxConfig(30 * time.Second)
	if err := yamux.VerifyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AcceptBacklog != 8 || cfg.StreamOpenTimeout != 10*time.Second || cfg.StreamCloseTimeout != 10*time.Second {
		t.Errorf("config: backlog=%d open=%v close=%v", cfg.AcceptBacklog, cfg.StreamOpenTimeout, cfg.StreamCloseTimeout)
	}
	if !cfg.EnableKeepAlive || cfg.KeepAliveInterval != 30*time.Second {
		t.Error("keepalive not applied")
	}
	if YamuxConfig(0).EnableKeepAlive {
		t.Error("keepalive on for the local pipe")
	}
	// The plan asked for a 64 KiB MaxStreamWindowSize. yamux refuses anything
	// below its 256 KiB initial window, so the bound cannot be expressed here;
	// this test pins that fact so a library change is noticed.
	small := YamuxConfig(0)
	small.MaxStreamWindowSize = 64 << 10
	if err := yamux.VerifyConfig(small); err == nil {
		t.Error("yamux now accepts a 64 KiB MaxStreamWindowSize: use it (plan 0002 M1 step 6)")
	}
	if cfg.MaxStreamWindowSize != 256<<10 {
		t.Errorf("MaxStreamWindowSize = %d", cfg.MaxStreamWindowSize)
	}
}

// Streams past MaxAgentStreams are closed without reaching the log source.
func TestAgentStreamCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := net.Pipe()
	var calls atomic.Int32
	src := blockingLogs{calls: &calls}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ServeSession(ctx, b, src, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	hubSide, err := yamux.Client(a, YamuxConfig(0))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { hubSide.Close(); cancel(); <-done }()
	line := []byte(`{"op":"logs","container":"c","tail":5}` + "\n")
	var streams []*yamux.Stream
	for i := 0; i < MaxAgentStreams; i++ {
		st, err := hubSide.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		st.Write(line)
		streams = append(streams, st)
	}
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() < MaxAgentStreams && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() != MaxAgentStreams {
		t.Fatalf("log source calls = %d, want %d", calls.Load(), MaxAgentStreams)
	}
	extra, err := hubSide.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	extra.Write(line)
	extra.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(extra) // one explanatory frame, then EOF
	if err != nil || !strings.Contains(string(got), "too many open streams") {
		t.Fatalf("33rd stream: %q %v, want the error frame then EOF", got, err)
	}
	if calls.Load() != MaxAgentStreams {
		t.Errorf("the over-cap stream reached the log source (%d)", calls.Load())
	}
	for _, st := range streams {
		st.Close()
	}
}

type blockingLogs struct{ calls *atomic.Int32 }

func (b blockingLogs) Logs(ctx context.Context, _ string, _ int, _ io.Writer) error {
	b.calls.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

func TestExplainDialError(t *testing.T) {
	mk := func(code int) *http.Response { return &http.Response{StatusCode: code} }
	for code, want := range map[int]string{401: "SD_AGENT_TOKEN", 429: "rate limiting", 503: "session limit"} {
		if got := explainDialError(errors.New("raw"), mk(code)); !strings.Contains(got.Error(), want) {
			t.Errorf("status %d: %v", code, got)
		}
	}
	got := explainDialError(fmt.Errorf("x: %w", pki.ErrVerify), nil)
	if !strings.Contains(got.Error(), "--ca-fingerprint") {
		t.Errorf("pin mismatch: %v", got)
	}
	if raw := errors.New("boom"); explainDialError(raw, nil) != raw {
		t.Error("other errors must pass through")
	}
}
