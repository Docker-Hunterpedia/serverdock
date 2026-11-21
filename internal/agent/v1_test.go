package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hashicorp/yamux"
)

func TestCleanText(t *testing.T) {
	cases := []struct {
		name, in string
		max      int
		want     string
	}{
		{"plain", "pull complete", 100, "pull complete"},
		{"escape sequence", "\x1b[31mred\x1b[0m", 100, "[31mred[0m"},
		{"newline and tab removed", "a\nb\tc\r", 100, "abc"},
		{"C0 and DEL", "a\x00\x07\x7fb", 100, "ab"},
		{"C1", "a\u0085\u009bb", 100, "ab"},
		{"bidi overrides", "a‮b⁦c‏d", 100, "abcd"},
		{"invalid UTF-8 replaced", "a\xffb\xc3", 100, "a�b�"},
		{"cap in bytes", strings.Repeat("a", 600), MaxReasonText, strings.Repeat("a", MaxReasonText)},
		{"cap never splits a rune", "aé", 2, "a"},
		{"zero cap", "abc", 0, ""},
	}
	for _, c := range cases {
		got := CleanText(c.in, c.max)
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if !utf8.ValidString(got) || len(got) > c.max {
			t.Errorf("%s: invalid or over the cap: %q", c.name, got)
		}
	}
	if got := CleanLogText("a\x1b[0m\nb\tc‮", 100); got != "a[0m\nb\tc" {
		t.Errorf("CleanLogText: %q", got)
	}
}

func TestRemoteOKAllowlistIsHelloAndLogs(t *testing.T) {
	got := map[string]bool{}
	for _, s := range Specs() {
		if s.RemoteOK {
			got[s.Name] = true
		}
	}
	if len(got) != 2 || !got[OpHello] || !got[OpLogs] {
		t.Fatalf("RemoteOK ops = %v, want exactly {hello, logs}", got)
	}
	if s, _ := SpecFor(OpHello); s.MaxResult != MaxHelloResult || s.Timeout <= 0 {
		t.Errorf("hello spec: %+v", s)
	}
}

func TestParseV1Request(t *testing.T) {
	good, err := MarshalRequest(OpHello, HelloArgs{HubMaxV: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !IsV1Request(good) {
		t.Fatalf("the hub's own request is not routed to v1: %s", good)
	}
	r, err := ParseV1Request(bytes.TrimSuffix(good, []byte("\n")))
	if err != nil || r.Op != OpHello {
		t.Fatalf("%+v %v", r, err)
	}
	bad := map[string]string{
		"trailing bytes": `{"v":1,"op":"hello","args":{"hub_max_v":1}} x`,
		"trailing space": `{"v":1,"op":"hello","args":{"hub_max_v":1}} `,
		"second object":  `{"v":1,"op":"hello"}{"v":1,"op":"hello"}`,
		"unknown field":  `{"v":1,"op":"hello","extra":1}`,
		"wrong version":  `{"v":2,"op":"hello"}`,
		"no version":     `{"op":"hello"}`,
		"unknown op":     `{"v":1,"op":"exec"}`,
		"logs is not v1": `{"v":1,"op":"logs"}`,
		"leading space":  ` {"v":1,"op":"hello"}`,
		"not an object":  `[1]`,
		"empty":          ``,
		"over 64 KiB":    `{"v":1,"op":"hello","args":{"x":"` + strings.Repeat("a", MaxRequestLine) + `"}}`,
	}
	for name, b := range bad {
		if _, err := ParseV1Request([]byte(b)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var a HelloArgs
	if err := DecodeArgs(json.RawMessage(`{"hub_max_v":1,"x":2}`), &a); err == nil {
		t.Error("unknown arg field accepted")
	}
	if err := DecodeArgs(json.RawMessage(`{"hub_max_v":1} `), &a); err == nil {
		t.Error("trailing bytes after args accepted")
	}
	if err := DecodeArgs(nil, &a); err != nil {
		t.Errorf("absent args: %v", err)
	}
}

func TestParseAgentLine(t *testing.T) {
	spec := OpSpec{Name: "test", MaxResult: 64, MaxProgressLines: 1}
	ok := []string{
		`{"t":"progress","p":{"a":1}}`,
		`{"t":"result","ok":true,"r":{}}`,
		`{"t":"result","ok":false,"err":{"code":"docker","msg":"x"}}`,
	}
	for _, l := range ok {
		if _, err := ParseAgentLine([]byte(l), spec); err != nil {
			t.Errorf("%s: %v", l, err)
		}
	}
	bad := map[string]string{
		"trailing bytes":     `{"t":"progress","p":{}} x`,
		"two objects":        `{"t":"progress","p":{}}{"t":"progress","p":{}}`,
		"unknown field":      `{"t":"progress","p":{},"z":1}`,
		"event off-events":   `{"t":"event","e":{}}`,
		"progress with r":    `{"t":"progress","p":{},"r":{}}`,
		"result no ok":       `{"t":"result","r":{}}`,
		"ok with err":        `{"t":"result","ok":true,"r":{},"err":{"code":"docker","msg":"x"}}`,
		"err with r":         `{"t":"result","ok":false,"r":{},"err":{"code":"docker","msg":"x"}}`,
		"unknown code":       `{"t":"result","ok":false,"err":{"code":"boom","msg":"x"}}`,
		"unknown type":       `{"t":"log"}`,
		"oversize result":    `{"t":"result","ok":true,"r":{"x":"` + strings.Repeat("a", 64) + `"}}`,
		"oversize progress":  `{"t":"progress","p":{"x":"` + strings.Repeat("a", MaxProgressLine) + `"}}`,
		"invalid UTF-8":      "{\"t\":\"progress\",\"p\":{\"x\":\"\xff\"}}",
		"raw escape byte":    "{\"t\":\"progress\",\"p\":{\"x\":\"\x1b[31m\"}}",
		"leading whitespace": ` {"t":"progress","p":{}}`,
	}
	for name, l := range bad {
		if _, err := ParseAgentLine([]byte(l), spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// An escaped control character in an error message is cleaned, not fatal.
	l, err := ParseAgentLine([]byte(`{"t":"result","ok":false,"err":{"code":"docker","msg":"a\u001b[31m‮b"}}`), spec)
	if err != nil || l.Err.Msg != "a[31mb" {
		t.Errorf("escaped control characters: %+v %v", l.Err, err)
	}
	events := OpSpec{Name: OpEvents}
	if _, err := ParseAgentLine([]byte(`{"t":"event","e":{"a":1}}`), events); err != nil {
		t.Errorf("event line on events: %v", err)
	}
}

func FuzzParseAgentLine(f *testing.F) {
	for _, s := range []string{
		`{"t":"progress","p":{"a":1}}`, `{"t":"result","ok":true,"r":{}}`,
		`{"t":"result","ok":false,"err":{"code":"docker","msg":"x"}}`, `{"t":"event","e":{}}`,
		`{"t":"progress","p":{}} x`, "{\"t\":\"progress\",\"p\":\"\xff\"}", ``, `null`, `{}`,
	} {
		f.Add([]byte(s))
	}
	spec := OpSpec{Name: OpEvents, MaxResult: 64, MaxProgressLines: 1}
	f.Fuzz(func(t *testing.T, b []byte) {
		l, err := ParseAgentLine(b, spec)
		if err != nil {
			return
		}
		// Whatever is accepted obeys the caps and carries clean text.
		if l.T == LineProgress && len(b) > MaxProgressLine {
			t.Fatalf("oversize progress accepted")
		}
		if l.Err != nil && (len(l.Err.Msg) > MaxReasonText || !utf8.ValidString(l.Err.Msg) || CleanText(l.Err.Msg, MaxReasonText) != l.Err.Msg) {
			t.Fatalf("uncleaned error text: %q", l.Err.Msg)
		}
		if !ValidErrorCode(codeOf(l)) && l.Err != nil {
			t.Fatalf("unknown code accepted")
		}
	})
}

func codeOf(l Line) string {
	if l.Err == nil {
		return ""
	}
	return l.Err.Code
}

func FuzzParseV1Request(f *testing.F) {
	for _, s := range []string{
		`{"v":1,"op":"hello","args":{"hub_max_v":1}}`, `{"v":1,"op":"hello"}`, `{"v":1,"op":"hello"} x`,
		`{"v":2,"op":"hello"}`, `{"op":"logs","container":"a","tail":1}`, ``, `{"v":1,"op":"hello","args":null}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := ParseV1Request(b)
		if err != nil {
			return
		}
		if r.V != ProtocolVersion || len(b) > MaxRequestLine {
			t.Fatalf("accepted %+v from %d bytes", r, len(b))
		}
		if _, ok := SpecFor(r.Op); !ok || r.Op == OpLogs {
			t.Fatalf("accepted op %q", r.Op)
		}
		var a HelloArgs
		_ = DecodeArgs(r.Args, &a) // must not panic
	})
}

func TestReadLine(t *testing.T) {
	br := func(s string) *bufio.Reader { return bufio.NewReaderSize(strings.NewReader(s), 16) }
	if l, err := ReadLine(br("hello\nrest"), 10); err != nil || string(l) != "hello" {
		t.Errorf("%q %v", l, err)
	}
	long := strings.Repeat("a", 100) + "\n" // longer than the 16-byte buffer
	if l, err := ReadLine(br(long), 100); err != nil || len(l) != 100 {
		t.Errorf("a line longer than the buffer: %d %v", len(l), err)
	}
	if _, err := ReadLine(br(long), 99); !errors.Is(err, ErrLineTooLong) {
		t.Errorf("over the limit: %v", err)
	}
	if _, err := ReadLine(br("no newline"), 100); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("no newline: %v", err)
	}
}

// pipeAgent serves cfg on one end of a pipe and returns a yamux client for the
// other (the test acts as the hub).
func pipeAgent(t *testing.T, cfg Config) *yamux.Session {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Serve(ctx, b, cfg, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	hubSide, err := yamux.Client(a, YamuxConfig(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hubSide.Close(); cancel(); <-done })
	return hubSide
}

// roundTrip writes req to a fresh stream and returns everything the agent sends.
func roundTrip(t *testing.T, ys *yamux.Session, req []byte) string {
	t.Helper()
	st, err := ys.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Write(req)
	st.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatalf("read: %v (got %q)", err, got)
	}
	return string(got)
}

func TestAgentHelloAndRefusals(t *testing.T) {
	ys := pipeAgent(t, Config{Logs: blockingLogs{calls: &atomic.Int32{}}, Version: "1.2.3-\x1b[31mevil‮" + strings.Repeat("x", 100)})
	got := roundTrip(t, ys, []byte(`{"v":1,"op":"hello","args":{"hub_max_v":1}}`+"\n"))
	var l Line
	if err := json.Unmarshal([]byte(strings.TrimSuffix(got, "\n")), &l); err != nil || l.OK == nil || !*l.OK {
		t.Fatalf("hello: %q %v", got, err)
	}
	var hr HelloResult
	if err := json.Unmarshal(l.R, &hr); err != nil || hr.V != 1 || len(hr.Agent) > MaxAgentVersion || !strings.HasPrefix(hr.Agent, "1.2.3-[31mevil") {
		t.Fatalf("hello result: %+v %v", hr, err)
	}
	if len(l.R) > MaxHelloResult {
		t.Errorf("hello result is %d bytes", len(l.R))
	}
	// hub_max_v above ours negotiates down.
	got = roundTrip(t, ys, []byte(`{"v":1,"op":"hello","args":{"hub_max_v":7}}`+"\n"))
	if !strings.Contains(got, `"v":1`) {
		t.Errorf("negotiate down: %q", got)
	}
	refused := map[string][]byte{
		"trailing bytes":   []byte(`{"v":1,"op":"hello","args":{"hub_max_v":1}} x` + "\n"),
		"unknown field":    []byte(`{"v":1,"op":"hello","args":{"hub_max_v":1,"z":1}}` + "\n"),
		"unknown op":       []byte(`{"v":1,"op":"container_stop","args":{}}` + "\n"),
		"bad hub_max_v":    []byte(`{"v":1,"op":"hello","args":{"hub_max_v":0}}` + "\n"),
		"line over 64 KiB": append([]byte(`{"v":1,"op":"hello","args":{"hub_max_v":1,"x":"`+strings.Repeat("a", MaxRequestLine)+`"}}`), '\n'),
	}
	for name, req := range refused {
		got := roundTrip(t, ys, req)
		if !strings.Contains(got, `"ok":false`) || !strings.Contains(got, `"code":"invalid"`) {
			t.Errorf("%s: %.200q", name, got)
		}
		if strings.Count(got, "\n") != 1 {
			t.Errorf("%s: want exactly one line: %.200q", name, got)
		}
	}
}

// TestRemoteAgentProcessServesOnlyRemoteOKOps: the agent's own handler table
// leaves out local-only ops in remote mode, whatever the hub sends; local mode
// serves them.
func TestRemoteAgentProcessServesOnlyRemoteOKOps(t *testing.T) {
	var ran atomic.Int32
	h := func(context.Context, json.RawMessage, *Out) (any, error) {
		ran.Add(1)
		return map[string]int{"n": 1}, nil
	}
	cfg := Config{
		Logs: blockingLogs{calls: &atomic.Int32{}},
		specs: []OpSpec{
			{Name: OpLogs, RemoteOK: true},
			{Name: OpHello, RemoteOK: true, MaxResult: MaxHelloResult, Timeout: time.Second},
			{Name: "test_write", Timeout: time.Second}, // zero value: local-only
			{Name: "test_read", RemoteOK: true, Timeout: time.Second},
		},
		impls: map[string]Handler{OpHello: Config{}.hello, "test_write": h, "test_read": h},
	}
	if _, ok := cfg.handlerTable()["test_write"]; ok {
		t.Fatal("remote table contains a local-only op")
	}
	cfg.Mode = ModeLocal
	if _, ok := cfg.handlerTable()["test_write"]; !ok {
		t.Fatal("local table lacks the local-only op")
	}
	// End to end: a spec lookup that knows the op (ParseV1Request uses the
	// package table, so register the names there for the duration).
	saved := opSpecs
	opSpecs = cfg.specs
	t.Cleanup(func() { opSpecs = saved })
	for _, mode := range []Mode{ModeRemote, ModeLocal} {
		cfg.Mode = mode
		ys := pipeAgent(t, cfg)
		ran.Store(0)
		got := roundTrip(t, ys, []byte(`{"v":1,"op":"test_write"}`+"\n"))
		served := strings.Contains(got, `"ok":true`)
		if served != (mode == ModeLocal) || (ran.Load() == 1) != (mode == ModeLocal) {
			t.Errorf("mode %d: test_write served=%v ran=%d reply %q", mode, served, ran.Load(), got)
		}
		if got := roundTrip(t, ys, []byte(`{"v":1,"op":"test_read"}`+"\n")); !strings.Contains(got, `"ok":true`) {
			t.Errorf("mode %d: RemoteOK op refused: %q", mode, got)
		}
	}
}

// v1 ops never see a `logs` request, and v0 logs lines still work with and
// without follow.
func TestLogsFollowFalseEndsAtTail(t *testing.T) {
	src := &followSource{}
	ys := pipeAgent(t, Config{Logs: src})
	got := roundTrip(t, ys, []byte(`{"op":"logs","container":"c1","tail":5,"follow":false}`+"\n"))
	if got != "tail of c1\n" || src.follow.Load() != 0 {
		t.Fatalf("follow=false: %q follow=%d", got, src.follow.Load())
	}
	// Absent follow = true (the v0 default); follow=true is also accepted.
	for _, line := range []string{`{"op":"logs","container":"c1","tail":5}`, `{"op":"logs","container":"c1","tail":5,"follow":true}`} {
		src.follow.Store(-1)
		st, _ := ys.OpenStream()
		st.Write([]byte(line + "\n"))
		st.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 64)
		n, err := st.Read(buf)
		if err != nil || string(buf[:n]) != "following c1\n" {
			t.Errorf("%s: %q %v", line, buf[:n], err)
		}
		st.Close()
	}
	// A source that cannot end at the tail refuses follow=false rather than following forever.
	ys2 := pipeAgent(t, Config{Logs: blockingLogs{calls: &atomic.Int32{}}})
	got = roundTrip(t, ys2, []byte(`{"op":"logs","container":"c1","tail":5,"follow":false}`+"\n"))
	if !strings.Contains(got, "follow=false is not supported") {
		t.Errorf("no FollowLogSource: %q", got)
	}
}

type followSource struct{ follow atomic.Int32 }

func (f *followSource) Logs(ctx context.Context, c string, tail int, w io.Writer) error {
	_, _ = io.WriteString(w, "following "+c+"\n")
	<-ctx.Done()
	return ctx.Err()
}

func (f *followSource) LogsFollow(ctx context.Context, c string, tail int, follow bool, w io.Writer) error {
	if follow {
		return f.Logs(ctx, c, tail, w)
	}
	f.follow.Store(0)
	_, _ = io.WriteString(w, "tail of "+c+"\n")
	return nil // the stream ends at the tail
}
