package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
)

// v1 test ops: a RemoteOK op with small limits, one with a huge progress
// allowance (for the byte total), and a local-only op.
func v1Specs(name string) (agent.OpSpec, bool) {
	switch name {
	case "test-v1":
		return agent.OpSpec{Name: name, RemoteOK: true, MaxResult: 1024, MaxProgressLines: 3, Timeout: 5 * time.Second}, true
	case "test-flood":
		return agent.OpSpec{Name: name, RemoteOK: true, MaxResult: 1024, MaxProgressLines: 1 << 20, Timeout: 20 * time.Second}, true
	case "test-slow":
		return agent.OpSpec{Name: name, RemoteOK: true, MaxResult: 1024, Timeout: 150 * time.Millisecond}, true
	case "test-local":
		return agent.OpSpec{Name: name, MaxResult: 1024, MaxProgressLines: 3, Timeout: 5 * time.Second}, true
	}
	return agent.SpecFor(name)
}

// m2Reply is what an M1/M2 agent writes for a request line, copied from
// internal/agent/session.go (handleStream) and protocol.go (ParseOp) as of M2:
// the old strict Op, DisallowUnknownFields, a 4096-byte line, and
// writeLine("serverdock-agent: " + err.Error()).
func m2Reply(line []byte) string {
	type m2Op struct {
		Op        string `json:"op"`
		Container string `json:"container"`
		Tail      int    `json:"tail"`
	}
	fail := func(err error) string { return "serverdock-agent: " + err.Error() + "\n" }
	if len(line) > 4096 {
		return fail(errors.New("op line too long"))
	}
	var o m2Op
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return fail(fmt.Errorf("bad op: %w", err))
	}
	if dec.More() {
		return fail(errors.New("bad op: trailing data"))
	}
	if o.Op != "logs" {
		return fail(fmt.Errorf("unknown op %q", o.Op))
	}
	return "" // would stream logs
}

// fakeAgent attaches a hub session over a pipe and serves every stream with
// serve(op, stream-reader, stream); it returns once the handshake finished.
type fakeAgent struct {
	s    *Session
	mu   sync.Mutex
	reqs [][]byte // every request line received, in order
}

func (f *fakeAgent) requests() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.reqs...)
}

func newFakeAgent(t *testing.T, id string, serve func(f *fakeAgent, line []byte, st *yamux.Stream), opts ...Option) *fakeAgent {
	t.Helper()
	f := startFakeAgent(t, id, serve, opts...)
	select {
	case <-f.s.HelloDone():
	case <-time.After(10 * time.Second):
		t.Fatal("handshake did not finish")
	}
	return f
}

// startFakeAgent is newFakeAgent without waiting for the handshake.
func startFakeAgent(t *testing.T, id string, serve func(f *fakeAgent, line []byte, st *yamux.Stream), opts ...Option) *fakeAgent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := New(ctx, "", quiet, append([]Option{withSpecFor(v1Specs)}, opts...)...)
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Attach(ctx, id, a, 0) }()
	ys, err := yamux.Server(b, agent.YamuxConfig(0))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAgent{}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			st, err := ys.AcceptStream()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer st.Close()
				st.SetReadDeadline(time.Now().Add(5 * time.Second))
				line, err := agent.ReadLine(bufio.NewReader(st), agent.MaxRequestLine)
				if err != nil {
					return
				}
				f.mu.Lock()
				f.reqs = append(f.reqs, line)
				f.mu.Unlock()
				serve(f, line, st)
			})
		}
	})
	t.Cleanup(func() { ys.Close(); cancel(); <-done; wg.Wait() })
	f.s = waitSession(t, h, id, 0)
	return f
}

func okLine(r string) string { return `{"t":"result","ok":true,"r":` + r + `}` + "\n" }

func helloOK(line []byte, st *yamux.Stream) bool {
	if !strings.Contains(string(line), `"op":"hello"`) {
		return false
	}
	io.WriteString(st, okLine(`{"v":1,"agent":"fake-1.0\u001b[31m"}`))
	return true
}

func TestHelloNegotiatesV1(t *testing.T) {
	// A real agent over the in-memory pipe, the way runServer wires the local one.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := New(ctx, "", quiet, WithLocalVersion("9.9.9"))
	a, b := net.Pipe()
	var wg sync.WaitGroup
	wg.Go(func() { _ = h.Attach(ctx, LocalID, a, 0) })
	wg.Go(func() {
		_ = agent.Serve(ctx, b, agent.Config{Logs: newFakeLogs(), Version: "9.9.9", Mode: agent.ModeLocal}, 0, quiet)
	})
	defer func() { cancel(); wg.Wait() }()
	s := waitSession(t, h, LocalID, 0)
	select {
	case <-s.HelloDone():
	case <-time.After(10 * time.Second):
		t.Fatal("no handshake")
	}
	if s.Version() != 1 || s.AgentVersion() != "9.9.9" {
		t.Fatalf("version %d agent %q", s.Version(), s.AgentVersion())
	}
	// And over a real remote session (RunClient serves remote mode).
	e := newEnv(t)
	startAgent(t, e.wsURL(), "remote1", newFakeLogs(), func(c *agent.ClientConfig) { c.Version = "r-1" })
	rs := waitSession(t, e.hub, "remote1", 0)
	<-rs.HelloDone()
	if rs.Version() != 1 || rs.AgentVersion() != "r-1" {
		t.Fatalf("remote: version %d agent %q", rs.Version(), rs.AgentVersion())
	}
}

// A hostile hello reply: the version string is cleaned and capped.
func TestHelloVersionStringIsCleaned(t *testing.T) {
	f := newFakeAgent(t, "a1", func(_ *fakeAgent, line []byte, st *yamux.Stream) {
		io.WriteString(st, okLine(`{"v":1,"agent":"1.0\u001b[31m‮`+strings.Repeat("z", 200)+`"}`))
	})
	if f.s.Version() != 1 {
		t.Fatalf("version %d", f.s.Version())
	}
	v := f.s.AgentVersion()
	if len(v) > agent.MaxAgentVersion || strings.ContainsAny(v, "\x1b‮") || !strings.HasPrefix(v, "1.0[31m") {
		t.Errorf("agent version %q", v)
	}
}

// TestM2AgentReplyIsDetectedAsV0: the exact bytes an M1/M2 agent writes for the
// hub's own hello request, and for an unknown op, leave the session at v0.
func TestM2AgentReplyIsDetectedAsV0(t *testing.T) {
	req, _ := agent.MarshalRequest(agent.OpHello, agent.HelloArgs{HubMaxV: agent.ProtocolVersion}, 1)
	reply := m2Reply(bytes.TrimSuffix(req, []byte("\n")))
	if reply != "serverdock-agent: bad op: json: unknown field \"v\"\n" {
		t.Fatalf("the M2 reply to the real hello request changed: %q (update this test deliberately)", reply)
	}
	for name, body := range map[string]string{
		"real M2 reply to the hub's hello": reply,
		"unknown op reply":                 "serverdock-agent: unknown op \"hello\"\n",
		"stream cap reply":                 "serverdock-agent: too many open streams\n",
		"nothing at all":                   "",
	} {
		f := newFakeAgent(t, "m2", func(_ *fakeAgent, _ []byte, st *yamux.Stream) { io.WriteString(st, body) })
		if f.s.Version() != 0 || f.s.AgentVersion() != "" {
			t.Errorf("%s: version %d agent %q, want v0", name, f.s.Version(), f.s.AgentVersion())
		}
		// logs still works on a v0 session.
		rc, err := f.s.Logs(context.Background(), "c1", 5)
		if err != nil {
			t.Errorf("%s: logs on v0: %v", name, err)
		} else {
			rc.Close()
		}
	}
}

// TestFollowIsNeverSentToAV0Session: an M2 agent refuses the unknown `follow`
// field, so the hub never writes it to a v0 session; follow=false there fails
// instead of silently following. On v1, follow=false is sent and follow=true is
// left absent.
func TestFollowIsNeverSentToAV0Session(t *testing.T) {
	v0 := newFakeAgent(t, "v0", func(*fakeAgent, []byte, *yamux.Stream) {}) // never answers hello properly
	if v0.s.Version() != 0 {
		t.Fatalf("version %d", v0.s.Version())
	}
	if rc, err := v0.s.LogsOnce(context.Background(), "c1", 5); !errors.Is(err, ErrNeedsV1) || rc != nil {
		t.Fatalf("follow=false on v0: %v", err)
	}
	rc, err := v0.s.Logs(context.Background(), "c1", 5)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	waitFor(t, func() bool { return len(v0.requests()) >= 2 }) // hello + logs
	for _, r := range v0.requests() {
		if bytes.Contains(r, []byte("follow")) {
			t.Errorf("a v0 session was sent %q", r)
		}
		if m2 := m2Reply(r); strings.Contains(r2s(r), `"op":"logs"`) && m2 != "" {
			t.Errorf("an M2 agent would refuse %q with %q", r, m2)
		}
	}

	v1 := newFakeAgent(t, "v1", func(_ *fakeAgent, line []byte, st *yamux.Stream) { helloOK(line, st) })
	if v1.s.Version() != 1 {
		t.Fatalf("version %d", v1.s.Version())
	}
	rc, err = v1.s.LogsOnce(context.Background(), "c1", 5)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	rc, err = v1.s.Logs(context.Background(), "c1", 5)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	waitFor(t, func() bool { return len(v1.requests()) >= 3 })
	reqs := v1.requests()
	if !bytes.Contains(reqs[1], []byte(`"follow":false`)) {
		t.Errorf("v1 follow=false request: %q", reqs[1])
	}
	if bytes.Contains(reqs[2], []byte("follow")) {
		t.Errorf("follow=true must be absent: %q", reqs[2])
	}
}

func r2s(b []byte) string { return string(b) }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestV1NeverWidensTheRemoteOpSet: a remote session that negotiated v1 still
// refuses every local-only op (RemoteOK is checked independently of hello), and
// a v1 op on a v0 session is refused.
func TestV1NeverWidensTheRemoteOpSet(t *testing.T) {
	f := newFakeAgent(t, "remote1", func(_ *fakeAgent, line []byte, st *yamux.Stream) { helloOK(line, st) })
	if f.s.Version() != 1 {
		t.Fatal("not v1")
	}
	if _, err := f.s.Call(context.Background(), "test-local", nil, nil); !errors.Is(err, ErrLocalOnly) {
		t.Errorf("local-only op on a v1 remote session: %v", err)
	}
	if _, err := f.s.open("test-local"); !errors.Is(err, ErrLocalOnly) {
		t.Errorf("open: %v", err)
	}
	// A v0 session: a v1 op is refused before any stream is opened.
	v0 := newFakeAgent(t, "old", func(*fakeAgent, []byte, *yamux.Stream) {})
	before := len(v0.requests())
	if _, err := v0.s.Call(context.Background(), "test-v1", nil, nil); !errors.Is(err, ErrNeedsV1) {
		t.Errorf("v1 op on a v0 session: %v", err)
	}
	if _, err := v0.s.Call(context.Background(), "test-local", nil, nil); !errors.Is(err, ErrLocalOnly) {
		t.Errorf("local-only op on a v0 session: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if len(v0.requests()) != before {
		t.Error("a request reached a v0 agent")
	}
	// A session cannot raise its own version past what hello returned: an agent
	// that claims v9 is held to our maximum.
	hi := newFakeAgent(t, "hi", func(_ *fakeAgent, _ []byte, st *yamux.Stream) { io.WriteString(st, okLine(`{"v":9,"agent":"x"}`)) })
	if hi.s.Version() != agent.ProtocolVersion {
		t.Errorf("version %d", hi.s.Version())
	}
}

// TestRemoteSessionRefusesEveryLocalOnlyOp walks the whole op table.
func TestRemoteSessionRefusesEveryLocalOnlyOp(t *testing.T) {
	specs := append(agent.Specs(), agent.OpSpec{Name: "test-local"})
	f := newFakeAgent(t, "remote1", func(_ *fakeAgent, line []byte, st *yamux.Stream) { helloOK(line, st) },
		withSpecFor(func(name string) (agent.OpSpec, bool) {
			for _, sp := range specs {
				if sp.Name == name {
					return sp, true
				}
			}
			return agent.OpSpec{}, false
		}))
	n := len(f.requests())
	for _, sp := range specs {
		_, err := f.s.open(sp.Name)
		if sp.RemoteOK == errors.Is(err, ErrLocalOnly) && !(sp.RemoteOK && err == nil) {
			t.Errorf("op %q (RemoteOK=%v): %v", sp.Name, sp.RemoteOK, err)
		}
		if !sp.RemoteOK {
			if _, err := f.s.Call(context.Background(), sp.Name, nil, nil); !errors.Is(err, ErrLocalOnly) {
				t.Errorf("Call %q: %v", sp.Name, err)
			}
		}
	}
	if len(f.requests()) != n {
		t.Error("a local-only op reached the remote agent")
	}
}

// TestHostileAgentIsRefused: every way an agent can break the v1 framing fails
// the op with agent_protocol, closes the stream and leaks nothing (goleak runs
// in TestMain).
func TestHostileAgentIsRefused(t *testing.T) {
	flood := strings.Repeat(`{"t":"progress","p":{"x":"`+strings.Repeat("a", 950)+`"}}`+"\n", 4400)
	cases := map[string]string{
		"over-long progress line":    `{"t":"progress","p":{"x":"` + strings.Repeat("a", 2000) + `"}}` + "\n" + okLine(`{}`),
		"over-long result":           okLine(`{"x":"` + strings.Repeat("a", 2000) + `"}`),
		"result over MaxResult":      okLine(`{"x":"` + strings.Repeat("a", 1100) + `"}`),
		"two results":                okLine(`{}`) + okLine(`{}`),
		"no result":                  `{"t":"progress","p":{}}` + "\n",
		"nothing at all":             "",
		"unterminated line":          `{"t":"result","ok":true,"r":{}}`,
		"trailing bytes in line":     `{"t":"progress","p":{}} x` + "\n" + okLine(`{}`),
		"data after the result":      okLine(`{}`) + "junk",
		"unknown field":              `{"t":"progress","p":{},"z":1}` + "\n" + okLine(`{}`),
		"progress flood":             strings.Repeat(`{"t":"progress","p":{}}`+"\n", 4) + okLine(`{}`),
		"invalid UTF-8":              "{\"t\":\"progress\",\"p\":{\"x\":\"\xff\xfe\"}}\n" + okLine(`{}`),
		"raw escape sequence":        "{\"t\":\"progress\",\"p\":{\"x\":\"\x1b[2J\"}}\n" + okLine(`{}`),
		"event from a non-events op": `{"t":"event","e":{}}` + "\n" + okLine(`{}`),
		"unknown error code":         `{"t":"result","ok":false,"err":{"code":"nope","msg":"x"}}` + "\n",
		"not JSON":                   "serverdock-agent: bad op\n",
	}
	f := newFakeAgent(t, "remote1", func(f *fakeAgent, line []byte, st *yamux.Stream) {
		if helloOK(line, st) {
			return
		}
		if strings.Contains(string(line), `"op":"test-flood"`) {
			io.WriteString(st, flood)
			return
		}
		var req struct{ Args struct{ Case string } }
		json.Unmarshal(line, &req)
		io.WriteString(st, cases[req.Args.Case])
	})
	for name := range cases {
		_, err := f.s.Call(context.Background(), "test-v1", map[string]string{"Case": name}, nil)
		if !errors.Is(err, ErrAgentProtocol) {
			t.Errorf("%s: err = %v, want agent_protocol", name, err)
		}
	}
	// More than 4 MiB in total.
	if _, err := f.s.Call(context.Background(), "test-flood", nil, nil); !errors.Is(err, ErrAgentProtocol) || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("4 MiB total: %v", err)
	}
	// The session survives: a well-behaved op still works.
	f2 := newFakeAgent(t, "remote2", func(f *fakeAgent, line []byte, st *yamux.Stream) {
		if helloOK(line, st) {
			return
		}
		io.WriteString(st, `{"t":"progress","p":{"n":1}}`+"\n"+okLine(`{"done":true}`))
	})
	var got []agent.Line
	raw, err := f2.s.Call(context.Background(), "test-v1", nil, func(l agent.Line) { got = append(got, l) })
	if err != nil || string(raw) != `{"done":true}` || len(got) != 1 || got[0].T != agent.LineProgress {
		t.Errorf("good op: %s %v %v", raw, got, err)
	}
	// Streams are closed, so slots are free again.
	if n := len(f.s.sem); n != 0 {
		t.Errorf("%d stream slots still held", n)
	}
}

// A hello result over 256 bytes is a protocol violation on a direct Call.
func TestHelloResultOverCapIsRefused(t *testing.T) {
	var calls atomic.Int32
	f := newFakeAgent(t, "remote1", func(_ *fakeAgent, line []byte, st *yamux.Stream) {
		if calls.Add(1) == 1 {
			helloOK(line, st)
			return
		}
		io.WriteString(st, okLine(`{"v":1,"agent":"`+strings.Repeat("a", 300)+`"}`))
	})
	_, err := f.s.call(context.Background(), agent.OpHello, agent.HelloArgs{HubMaxV: 1}, nil, true)
	if !errors.Is(err, ErrAgentProtocol) {
		t.Errorf("hello over 256 B: %v", err)
	}
}

// A failed result comes back as an *agent.OpError with cleaned text.
func TestFailedResultIsAnOpError(t *testing.T) {
	f := newFakeAgent(t, "remote1", func(_ *fakeAgent, line []byte, st *yamux.Stream) {
		if helloOK(line, st) {
			return
		}
		io.WriteString(st, `{"t":"result","ok":false,"err":{"code":"not_found","msg":"no such \u001b[31mimage‮"}}`+"\n")
	})
	_, err := f.s.Call(context.Background(), "test-v1", nil, nil)
	var oe *agent.OpError
	if !errors.As(err, &oe) || oe.Code != agent.CodeNotFound || oe.Msg != "no such [31mimage" {
		t.Errorf("%v", err)
	}
	if errors.Is(err, ErrAgentProtocol) {
		t.Error("an agent's own error is not a protocol violation")
	}
}

// Cancelling the context ends a Call that is waiting for the agent.
func TestCallCancelledByContext(t *testing.T) {
	block := make(chan struct{})
	f := newFakeAgent(t, "remote1", func(_ *fakeAgent, line []byte, st *yamux.Stream) {
		if helloOK(line, st) {
			return
		}
		<-block
	})
	t.Cleanup(func() { close(block) })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := f.s.Call(ctx, "test-v1", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrOpTimeout) || time.Since(start) > 3*time.Second {
		t.Errorf("%v after %v", err, time.Since(start))
	}
	waitFor(t, func() bool { return len(f.s.sem) == 0 }) // the stream slot is released
	// The op's own timeout is told apart from the caller's context ending.
	if _, err := f.s.Call(context.Background(), "test-slow", nil, nil); !errors.Is(err, ErrOpTimeout) {
		t.Errorf("op timeout: %v", err)
	}
	waitFor(t, func() bool { return len(f.s.sem) == 0 })
}

// The protocol-violation log line is limited to one per op name per minute.
func TestProtocolViolationLoggedOncePerOpPerMinute(t *testing.T) {
	var buf syncBuf
	ctx, cancel := context.WithCancel(context.Background())
	h := New(ctx, "", slogTo(&buf), withSpecFor(v1Specs))
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Attach(ctx, "r", a, 0) }()
	ys, _ := yamux.Server(b, agent.YamuxConfig(0))
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			st, err := ys.AcceptStream()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer st.Close()
				line, _ := agent.ReadLine(bufio.NewReader(st), agent.MaxRequestLine)
				if !helloOK(line, st) {
					io.WriteString(st, "garbage\n")
				}
			})
		}
	})
	defer func() { ys.Close(); cancel(); <-done; wg.Wait() }()
	s := waitSession(t, h, "r", 0)
	<-s.HelloDone()
	for i := 0; i < 5; i++ {
		s.Call(context.Background(), "test-v1", nil, nil)
	}
	if n := strings.Count(buf.String(), "broke the op protocol"); n != 1 {
		t.Errorf("%d log lines for 5 violations, want 1", n)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func slogTo(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

func lowHelloTimeout(d time.Duration) Option {
	return withSpecFor(func(name string) (agent.OpSpec, bool) {
		sp, ok := v1Specs(name)
		if name == agent.OpHello {
			sp.Timeout = d
		}
		return sp, ok
	})
}

// The local agent is this binary: never v0, never asked, helloDone closed at
// once, version string from the hub option.
func TestLocalSessionIsNeverStuckAtV0(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := New(ctx, "", quiet, WithLocalVersion("L-1.0\x1b[31m"))
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Attach(ctx, LocalID, a, 0) }()
	ys, err := yamux.Server(b, agent.YamuxConfig(0)) // never accepts a stream: a hello would hang
	if err != nil {
		t.Fatal(err)
	}
	defer func() { ys.Close(); cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	var s *Session
	for s == nil && time.Now().Before(deadline) {
		s, _ = h.Get(LocalID)
		time.Sleep(5 * time.Millisecond)
	}
	if s == nil {
		t.Fatal("no local session")
	}
	select {
	case <-s.HelloDone():
	default:
		t.Fatal("helloDone is not closed for the local session")
	}
	if s.Version() != agent.ProtocolVersion || s.AgentVersion() != "L-1.0[31m" {
		t.Errorf("version %d agent %q", s.Version(), s.AgentVersion())
	}
}

func TestExportedCallRefusesHello(t *testing.T) {
	f := newFakeAgent(t, "remote1", func(_ *fakeAgent, line []byte, st *yamux.Stream) { helloOK(line, st) })
	if _, err := f.s.Call(context.Background(), agent.OpHello, agent.HelloArgs{HubMaxV: 1}, nil); err == nil {
		t.Error("Call(hello) accepted")
	}
}

// A silent agent (accepts the hello stream, never writes or closes) leaves the
// session at v0 after the hello timeout, and the hub closes the stream.
func TestSilentAgentFallsBackToV0AndStreamIsClosed(t *testing.T) {
	closed := make(chan struct{})
	f := newFakeAgent(t, "silent", func(_ *fakeAgent, _ []byte, st *yamux.Stream) {
		_, _ = io.Copy(io.Discard, st) // returns when the hub closes its end
		close(closed)
	}, lowHelloTimeout(200*time.Millisecond))
	if f.s.Version() != 0 {
		t.Fatalf("version %d", f.s.Version())
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the hub did not close the hello stream")
	}
	if rc, err := f.s.LogsOnce(context.Background(), "c", 5); !errors.Is(err, ErrNeedsV1) || rc != nil {
		t.Errorf("LogsOnce on a v0 session: %v", err)
	}
}

// A valid reply that arrives after the timeout is ignored.
func TestSlowAgentReplyAfterTimeoutIsIgnored(t *testing.T) {
	f := newFakeAgent(t, "slow", func(_ *fakeAgent, line []byte, st *yamux.Stream) {
		time.Sleep(500 * time.Millisecond)
		helloOK(line, st)
	}, lowHelloTimeout(150*time.Millisecond))
	if f.s.Version() != 0 {
		t.Fatalf("version %d right after the timeout", f.s.Version())
	}
	time.Sleep(800 * time.Millisecond)
	if f.s.Version() != 0 || f.s.AgentVersion() != "" {
		t.Errorf("a late hello reply was applied: version %d agent %q", f.s.Version(), f.s.AgentVersion())
	}
}

// A call made right after attach waits for the handshake instead of seeing a
// not-yet-negotiated v0.
func TestCallRightAfterAttachWaitsForHello(t *testing.T) {
	f := startFakeAgent(t, "remote1", func(_ *fakeAgent, line []byte, st *yamux.Stream) {
		if strings.Contains(string(line), `"op":"hello"`) {
			time.Sleep(300 * time.Millisecond)
			helloOK(line, st)
			return
		}
		io.WriteString(st, okLine(`{"ok":1}`))
	})
	start := time.Now()
	raw, err := f.s.Call(context.Background(), "test-v1", nil, nil)
	if err != nil || string(raw) != `{"ok":1}` || time.Since(start) < 250*time.Millisecond {
		t.Errorf("%s %v after %v", raw, err, time.Since(start))
	}
	// Same for LogsOnce on a fresh session.
	g := startFakeAgent(t, "remote2", func(_ *fakeAgent, line []byte, st *yamux.Stream) {
		if strings.Contains(string(line), `"op":"hello"`) {
			time.Sleep(300 * time.Millisecond)
			helloOK(line, st)
		}
	})
	rc, err := g.s.LogsOnce(context.Background(), "c", 5)
	if err != nil {
		t.Fatalf("LogsOnce right after attach: %v", err)
	}
	rc.Close()
}
