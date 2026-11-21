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
	"reflect"
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
		{"zero-width and BOM", "a\u200bb\u200c\u200dc\u2060d\ufeffe\u00adf", 100, "abcdef"},
		{"tag block", "a\U000E0041\U000E007Fb", 100, "ab"},
		{"line and paragraph separators", "a\u2028b\u2029c", 100, "abc"},
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
	good, err := MarshalRequest(OpHello, HelloArgs{HubMaxV: 1}, 1)
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
		"trailing bytes":      `{"v":1,"op":"hello","args":{"hub_max_v":1}} x`,
		"trailing space":      `{"v":1,"op":"hello","args":{"hub_max_v":1}} `,
		"second object":       `{"v":1,"op":"hello"}{"v":1,"op":"hello"}`,
		"unknown field":       `{"v":1,"op":"hello","extra":1}`,
		"duplicate key":       `{"v":1,"op":"hello","op":"hello"}`,
		"case-variant key":    `{"V":1,"op":"hello"}`,
		"case-variant op key": `{"v":1,"Op":"hello"}`,
		"hello must be v1":    `{"v":2,"op":"hello"}`,
		"wrong version":       `{"v":2,"op":"hello"}`,
		"no version":          `{"op":"hello"}`,
		"unknown op":          `{"v":1,"op":"exec"}`,
		"logs is not v1":      `{"v":1,"op":"logs"}`,
		"leading space":       ` {"v":1,"op":"hello"}`,
		"not an object":       `[1]`,
		"empty":               ``,
		"over 64 KiB":         `{"v":1,"op":"hello","args":{"x":"` + strings.Repeat("a", MaxRequestLine) + `"}}`,
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
	for _, in := range []string{`{"hub_max_v":1,"hub_max_v":2}`, `{"Hub_Max_V":1}`, `{"HUB_MAX_V":1}`} {
		if err := DecodeArgs(json.RawMessage(in), &a); err == nil {
			t.Errorf("%s accepted", in)
		}
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
		"trailing bytes":      `{"t":"progress","p":{}} x`,
		"two objects":         `{"t":"progress","p":{}}{"t":"progress","p":{}}`,
		"unknown field":       `{"t":"progress","p":{},"z":1}`,
		"event off-events":    `{"t":"event","e":{}}`,
		"progress with r":     `{"t":"progress","p":{},"r":{}}`,
		"result no ok":        `{"t":"result","r":{}}`,
		"ok with err":         `{"t":"result","ok":true,"r":{},"err":{"code":"docker","msg":"x"}}`,
		"err with r":          `{"t":"result","ok":false,"r":{},"err":{"code":"docker","msg":"x"}}`,
		"unknown code":        `{"t":"result","ok":false,"err":{"code":"boom","msg":"x"}}`,
		"unknown type":        `{"t":"log"}`,
		"oversize result":     `{"t":"result","ok":true,"r":{"x":"` + strings.Repeat("a", 64) + `"}}`,
		"oversize progress":   `{"t":"progress","p":{"x":"` + strings.Repeat("a", MaxProgressLine) + `"}}`,
		"invalid UTF-8":       "{\"t\":\"progress\",\"p\":{\"x\":\"\xff\"}}",
		"raw escape byte":     "{\"t\":\"progress\",\"p\":{\"x\":\"\x1b[31m\"}}",
		"leading whitespace":  ` {"t":"progress","p":{}}`,
		"duplicate key":       `{"t":"progress","t":"result","p":{}}`,
		"case-variant key":    `{"T":"progress","p":{}}`,
		"duplicate in err":    `{"t":"result","ok":false,"err":{"code":"docker","code":"policy","msg":"x"}}`,
		"case-variant in err": `{"t":"result","ok":false,"err":{"Code":"docker","msg":"x"}}`,
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
		`{"t":"progress","t":"result","p":{}}`, `{"T":"progress","p":{}}`, `{"t":"result","ok":false,"err":{"code":"docker","Code":"x","msg":""}}`,
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
		`{"v":1,"op":"hello","op":"hello"}`, `{"V":1,"op":"hello"}`, `{"v":1,"OP":"hello"}`,
		`{"v":1,"op":"hello","args":{"hub_max_v":1,"hub_max_v":2}}`, `{"v":1,"op":"hello","args":[{"Type":"volume","type":"bind"}]}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := ParseV1Request(b)
		if err != nil {
			return
		}
		if r.V < 1 || r.V > ProtocolVersion || len(b) > MaxRequestLine {
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

// The hello bytes are frozen so a future v2 hub and a v1 agent can negotiate:
// the envelope is always v:1 whatever version the session negotiated.
func TestHelloBytesAreFrozen(t *testing.T) {
	for _, v := range []int{0, 1, 2, 7} {
		got, err := MarshalRequest(OpHello, HelloArgs{HubMaxV: 2}, v)
		if err != nil || string(got) != `{"v":1,"op":"hello","args":{"hub_max_v":2}}`+"\n" {
			t.Errorf("session v%d: %q %v", v, got, err)
		}
	}
	ys := pipeAgent(t, Config{Logs: blockingLogs{calls: &atomic.Int32{}}, Version: "1.2.3"})
	want := `{"t":"result","ok":true,"r":{"v":1,"agent":"1.2.3"}}` + "\n"
	for _, n := range []string{"1", "2"} { // a v2 hub says hub_max_v 2; the agent answers with what it speaks
		got := roundTrip(t, ys, []byte(`{"v":1,"op":"hello","args":{"hub_max_v":`+n+`}}`+"\n"))
		if got != want {
			t.Errorf("hub_max_v %s: %q", n, got)
		}
	}
	if _, err := ParseV1Request([]byte(`{"v":2,"op":"hello","args":{"hub_max_v":1}}`)); err == nil {
		t.Error("hello with v:2 accepted")
	}
}

// JSON escaping can grow an error message past MaxErrorLine; the agent shortens
// it so the hub never sees an over-long line. Same for the hello version.
func TestAgentErrorAndHelloLinesStayUnderTheirCaps(t *testing.T) {
	msg := strings.Repeat(`<>"&`, 128) // 512 bytes, about 3 KiB once escaped
	h := func(context.Context, json.RawMessage, *Out) (any, error) {
		return nil, &OpError{Code: CodeDocker, Msg: msg}
	}
	cfg := Config{
		Logs:  blockingLogs{calls: &atomic.Int32{}},
		Mode:  ModeLocal,
		specs: []OpSpec{{Name: OpLogs, RemoteOK: true}, {Name: "test_fail", Timeout: time.Second}},
		impls: map[string]Handler{"test_fail": h},
	}
	ys := pipeAgent(t, cfg)
	got := roundTrip(t, ys, []byte(`{"v":1,"op":"test_fail"}`+"\n"))
	line := strings.TrimSuffix(got, "\n")
	if len(line) > MaxErrorLine || strings.Count(got, "\n") != 1 {
		t.Fatalf("error line is %d bytes", len(line))
	}
	l, err := ParseAgentLine([]byte(line), OpSpec{Name: "test_fail"})
	if err != nil || l.Err == nil || l.Err.Code != CodeDocker || l.Err.Msg == "" {
		t.Fatalf("hub parse: %+v %v", l, err)
	}
	ys = pipeAgent(t, Config{Logs: blockingLogs{calls: &atomic.Int32{}}, Version: strings.Repeat("<", 64)})
	got = roundTrip(t, ys, []byte(`{"v":1,"op":"hello","args":{"hub_max_v":1}}`+"\n"))
	if l, err := ParseAgentLine([]byte(strings.TrimSuffix(got, "\n")), mustSpec(OpHello)); err != nil || l.OK == nil || !*l.OK {
		t.Errorf("escape-heavy version: %q %v", got, err)
	}
}

func mustSpec(name string) OpSpec { s, _ := SpecFor(name); return s }

// strictProbe is a destination type for the checkKeys tests.
type strictProbe struct {
	Items []struct {
		Type string `json:"type"`
	} `json:"items"`
	Ptrs []*struct {
		Name string `json:"name"`
	} `json:"ptrs"`
	Grid [][]struct {
		X int `json:"x"`
	} `json:"grid"`
	Labels map[string]string `json:"labels"`
	Nested map[string]struct {
		K string `json:"k"`
	} `json:"nested"`
	Raw json.RawMessage `json:"raw"`
	Any any             `json:"any"`
}

func TestCheckKeysDescendsIntoArraysAndMaps(t *testing.T) {
	bad := []string{
		`{"items":[{"Type":"volume","type":"bind"}]}`, // case variant of a known key inside an array element
		`{"items":[{"type":"a","type":"b"}]}`,         // duplicate inside an array element
		`{"items":[{"type":"a"},{"nope":1}]}`,         // unknown field in the second element
		`{"ptrs":[{"name":"a","name":"b"}]}`,          // pointer element
		`{"grid":[[{"x":1,"X":2}]]}`,                  // array of arrays
		`{"labels":{"a":"1","a":"2"}}`,                // duplicate map key
		`{"nested":{"a":{"k":"1"},"a":{"k":"2"}}}`,    // duplicate key at the map level
		`{"nested":{"a":{"k":"1","k":"2"}}}`,          // duplicate inside a map value
		`{"nested":{"a":{"K":"1"}}}`,                  // unknown field inside a map value
		`{"any":{"a":1,"a":2}}`,                       // interface values: duplicates only
		`{"any":[{"a":1,"a":2}]}`,                     // ... also inside arrays
		`{"labels":{"a":"1"},"labels":{}}`,            // duplicate top-level key
		`{"items":[{"type":"a"}`,                      // truncated
	}
	for _, s := range bad {
		if err := checkKeys([]byte(s), reflect.TypeOf(strictProbe{})); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
	good := []string{
		`{}`,
		`{"items":[{"type":"a"},{"type":"b"}],"ptrs":[{"name":"x"},null]}`,
		`{"grid":[[{"x":1}],[]]}`,
		`{"labels":{"a":"1","A":"2"}}`, // different keys, even if they differ only by case
		`{"nested":{"a":{"k":"1"},"b":{"k":"2"}}}`,
		`{"raw":{"a":1,"a":2}}`, // RawMessage contents are checked where they are decoded
		`{"any":{"a":1,"b":[1,{"c":2}]}}`,
	}
	for _, s := range good {
		if err := checkKeys([]byte(s), reflect.TypeOf(strictProbe{})); err != nil {
			t.Errorf("refused %s: %v", s, err)
		}
	}
}

// decodeStrict is the entry point: the same cases through it, with the real
// destination types.
func TestDecodeStrictRefusesCaseVariantsInsideArrays(t *testing.T) {
	var p strictProbe
	if err := decodeStrict([]byte(`{"items":[{"Type":"volume","type":"bind"}]}`), &p); err == nil {
		t.Fatal("accepted a case-variant key inside an array element")
	}
	if err := decodeStrict([]byte(`{"items":[{"type":"bind"}]}`), &p); err != nil || p.Items[0].Type != "bind" {
		t.Fatalf("refused a good body: %v", err)
	}
}

// fieldByTag fails closed on a field without a json tag name, so every type
// that goes through decodeStrict (and anything nested in it) must tag every
// field: an untagged or embedded field would make its keys "unknown".
func TestProtocolTypesTagEveryField(t *testing.T) {
	roots := []any{Request{}, Line{}, OpError{}, HelloArgs{}, HelloResult{}, Op{}}
	seen := map[reflect.Type]bool{}
	var walk func(t reflect.Type)
	walk = func(ty reflect.Type) {
		for ty.Kind() == reflect.Pointer || ty.Kind() == reflect.Slice || ty.Kind() == reflect.Array || ty.Kind() == reflect.Map {
			ty = ty.Elem()
		}
		if ty.Kind() != reflect.Struct || seen[ty] {
			return
		}
		seen[ty] = true
		for i := 0; i < ty.NumField(); i++ {
			f := ty.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" || f.Anonymous {
				t.Errorf("%s.%s has no json tag name (or is embedded)", ty, f.Name)
			}
			walk(f.Type)
		}
	}
	for _, r := range roots {
		walk(reflect.TypeOf(r))
	}
	if len(seen) < 5 {
		t.Fatalf("only %d types walked", len(seen))
	}
}

// What checkKeys accepts must decode with no duplicate key anywhere: a second
// decode of the accepted bytes into a generic value must see each object once.
func FuzzCheckKeys(f *testing.F) {
	for _, s := range []string{
		`{"items":[{"Type":"volume","type":"bind"}]}`, `{"items":[{"type":"a"}]}`, `{"labels":{"a":"1","a":"2"}}`,
		`{"nested":{"a":{"k":"1"}}}`, `{"grid":[[{"x":1}]]}`, `{"any":[{"a":1,"a":2}]}`, `{`, `[]`, ``,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if err := checkKeys(b, reflect.TypeOf(strictProbe{})); err != nil {
			return
		}
		var p strictProbe
		if err := json.Unmarshal(b, &p); err != nil {
			return
		}
		if bytes.Contains(b, []byte(`"Items"`)) || bytes.Contains(b, []byte(`"ITEMS"`)) {
			t.Fatalf("accepted a case-variant key: %q", b)
		}
	})
}
