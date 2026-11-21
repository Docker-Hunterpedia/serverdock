package hub

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
)

// upgrade sends a well-formed WebSocket upgrade request and returns the raw
// response (it does not follow up, so a 101 leaves the server holding a
// handshaken connection until the response body is closed).
func upgrade(t *testing.T, e *env, id, tok string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+"/agent/connect?id="+id, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSeventeenthSessionIsRefusedBeforeUpgrade(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < DefaultMaxSessions; i++ {
		startAgent(t, e.wsURL(), fmt.Sprintf("a%d", i), newFakeLogs(), nil)
	}
	for i := 0; i < DefaultMaxSessions; i++ {
		waitSession(t, e.hub, fmt.Sprintf("a%d", i), 0)
	}
	resp := upgrade(t, e, "a-extra", token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("17th session: %d, want 503", resp.StatusCode)
	}
	// Refused before websocket.Accept: not an upgrade response in any form.
	if resp.Header.Get("Upgrade") != "" || resp.Header.Get("Sec-WebSocket-Accept") != "" || resp.StatusCode == http.StatusSwitchingProtocols {
		t.Errorf("over-cap request got upgrade headers: %v", resp.Header)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After on 503")
	}
	if _, ok := e.hub.Get("a-extra"); ok {
		t.Error("over-cap agent got a session")
	}
	// An agent that is already counted can reconnect at the cap (replacing its
	// old session), instead of being locked out by its own stale session.
	s0 := waitSession(t, e.hub, "a0", 0)
	s0.ys.Close()
	waitSession(t, e.hub, "a0", s0.Seq)
}

func TestSessionSlotsAreReleased(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := New(ctx, token, quiet, WithMaxSessions(2))
	if !h.reserve("a") || !h.reserve("b") || h.reserve("c") {
		t.Fatal("cap of 2 not enforced")
	}
	if !h.reserve("a") { // same id: no new slot
		t.Fatal("re-reserving an id at the cap was refused")
	}
	h.release("a")
	if h.reserve("c") {
		t.Error("slot freed while the id still holds one")
	}
	h.release("a")
	if !h.reserve("c") {
		t.Error("slot not freed")
	}
	if len(h.slots) != 2 {
		t.Errorf("slots = %v", h.slots)
	}
}

func TestAuthFailureLimiterRunsBeforeTheTokenCompare(t *testing.T) {
	e := newEnv(t)
	var compares atomic.Int32
	orig := e.hub.compare
	e.hub.compare = func(tok string) bool { compares.Add(1); return orig(tok) }
	spoof := map[string]string{"X-Forwarded-For": "198.51.100.77"} // must not matter on this listener

	for i := 0; i < AuthFailureBurst; i++ {
		resp := upgrade(t, e, "a", "wrong-token", spoof)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("bad token %d: %d, want 401", i+1, resp.StatusCode)
		}
	}
	if n := compares.Load(); n != AuthFailureBurst {
		t.Fatalf("compares = %d, want %d", n, AuthFailureBurst)
	}
	// The 11th bad token is rate limited, and so is a CORRECT token from the same
	// IP: neither reaches the compare.
	for _, tok := range []string{"wrong-token", token} {
		resp := upgrade(t, e, "a", tok, spoof)
		resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("limited IP got %d, want 429", resp.StatusCode)
		}
		if ra := resp.Header.Get("Retry-After"); ra == "" || ra == "0" {
			t.Errorf("Retry-After = %q", ra)
		}
	}
	if n := compares.Load(); n != AuthFailureBurst {
		t.Errorf("the token was compared for a rate-limited IP (compares = %d)", n)
	}
	if _, ok := e.hub.Get("a"); ok {
		t.Error("a limited IP got a session")
	}
	if e.hub.fails.Len() != 1 {
		t.Errorf("limiter keys = %d (X-Forwarded-For must not create keys)", e.hub.fails.Len())
	}
}

func TestSuccessfulAuthDoesNotConsumeFailureBudget(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 3*AuthFailureBurst; i++ {
		startAgent(t, e.wsURL(), "same", newFakeLogs(), nil)()
	}
	if b, _ := e.hub.fails.Blocked("127.0.0.1"); b {
		t.Error("good logins exhausted the failure budget")
	}
}

// pipeSession attaches a hub session over an in-memory pipe whose other end is
// returned as a raw yamux server session, so tests can act as the agent.
func pipeSession(t *testing.T, id string) (*Session, *yamux.Session) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := New(ctx, "", quiet)
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Attach(ctx, id, a, 0) }()
	ys, err := yamux.Server(b, agent.YamuxConfig(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ys.Close(); cancel(); <-done })
	return waitSession(t, h, id, 0), ys
}

// Agents never open streams: the first one ends the session (closing the
// stream alone would leave it buffered in yamux for up to its close timeout).
func TestAgentOpenedStreamEndsTheSession(t *testing.T) {
	for _, n := range []int{1, 8, 40} {
		s, agentSide := pipeSession(t, fmt.Sprintf("x%d", n))
		for i := 0; i < n; i++ {
			st, err := agentSide.OpenStream()
			if err != nil {
				break // the hub already ended the session
			}
			st.Write([]byte("junk"))
		}
		select {
		case <-s.ys.CloseChan():
		case <-time.After(3 * time.Second):
			t.Fatalf("n=%d: the session survived an agent-opened stream", n)
		}
		select {
		case <-agentSide.CloseChan():
		case <-time.After(3 * time.Second):
			t.Fatalf("n=%d: the agent side never saw the session end", n)
		}
	}
}

// holdLogs writes a line and holds the stream open until cancelled.
type holdLogs struct{}

func (holdLogs) Logs(ctx context.Context, c string, _ int, w io.Writer) error {
	_, _ = io.WriteString(w, "hello "+c+"\n")
	<-ctx.Done()
	return ctx.Err()
}

func TestThirtyThirdStreamErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := New(ctx, "", quiet)
	a, b := net.Pipe()
	go func() { _ = h.Attach(ctx, LocalID, a, 0) }()
	go func() { _ = agent.Serve(ctx, b, agent.Config{Logs: holdLogs{}}, 0, quiet) }()
	s := waitSession(t, h, LocalID, 0)
	var open []io.ReadCloser
	for i := 0; i < MaxServerStreams; i++ {
		rc, err := s.Logs(ctx, fmt.Sprintf("c%d", i), 5)
		if err != nil {
			t.Fatalf("stream %d: %v", i+1, err)
		}
		open = append(open, rc)
	}
	if _, err := s.Logs(ctx, "one-too-many", 5); !errors.Is(err, ErrTooManyStreams) {
		t.Fatalf("33rd stream: %v, want ErrTooManyStreams", err)
	}
	// Closing one (twice, harmlessly) frees exactly one slot.
	open[0].Close()
	open[0].Close()
	rc, err := s.Logs(ctx, "after-close", 5)
	if err != nil {
		t.Fatalf("after freeing a slot: %v", err)
	}
	open = append(open, rc)
	if _, err := s.Logs(ctx, "again-too-many", 5); !errors.Is(err, ErrTooManyStreams) {
		t.Fatalf("slot accounting off: %v", err)
	}
	for _, rc := range open {
		rc.Close()
	}
}

// A remote session is capped at 8 streams (the local session keeps 32).
func TestNinthRemoteStreamErrors(t *testing.T) {
	e := newEnv(t)
	startAgent(t, e.wsURL(), "r1", holdLogs{}, nil)
	s := waitSession(t, e.hub, "r1", 0)
	ctx := context.Background()
	var open []io.ReadCloser
	for i := 0; i < MaxRemoteStreams; i++ {
		rc, err := s.Logs(ctx, fmt.Sprintf("c%d", i), 5)
		if err != nil {
			t.Fatalf("stream %d: %v", i+1, err)
		}
		open = append(open, rc)
	}
	if _, err := s.Logs(ctx, "ninth", 5); !errors.Is(err, ErrTooManyStreams) {
		t.Fatalf("9th remote stream: %v, want ErrTooManyStreams", err)
	}
	open[0].Close()
	rc, err := s.Logs(ctx, "after-close", 5)
	if err != nil {
		t.Fatalf("after freeing a slot: %v", err)
	}
	open = append(open, rc)
	for _, rc := range open {
		rc.Close()
	}
}

// An already-cancelled context must not race with Logs setting up its watcher
// (run with -race): the stream is closed and its slot freed.
func TestLogsWithCancelledContext(t *testing.T) {
	e := newEnv(t)
	startAgent(t, e.wsURL(), "c1", holdLogs{}, nil)
	s := waitSession(t, e.hub, "c1", 0)
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rc, err := s.Logs(ctx, "web", 5)
		if err == nil {
			rc.Close()
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(s.sem) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(s.sem) != 0 {
		t.Errorf("stream slots leaked: %d", len(s.sem))
	}
}

// G1 regression (from the security review's scratch program): a peer that
// sends yamux ping SYNs and never reads used to make yamux start a goroutine per
// ping (640k goroutines and 2 GiB in one second). The hub now ends the session
// past a few pings and the goroutine count stays bounded.
func TestPingFloodEndsTheSessionAndGoroutinesStayBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := New(ctx, "", quiet)
	hubEnd, agentEnd := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Attach(ctx, "flooder", hubEnd, agent.DefaultKeepAlive) }()
	waitSession(t, h, "flooder", 0)
	base := runtime.NumGoroutine()

	var sent atomic.Int64
	stop := make(chan struct{})
	go func() {
		hdr := make([]byte, 12)
		hdr[1], hdr[3] = 2, 1 // typePing, flagSYN
		buf := make([]byte, 0, 12*1000)
		for i := 0; i < 1000; i++ {
			binary.BigEndian.PutUint32(hdr[8:], uint32(i))
			buf = append(buf, hdr...)
		}
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, err := agentEnd.Write(buf) // never reads
			sent.Add(int64(n / 12))
			if err != nil {
				return
			}
		}
	}()
	defer close(stop)

	select {
	case <-done: // Attach returns when the session ends
	case <-time.After(5 * time.Second):
		t.Fatalf("the session survived a ping flood (%d pings sent, %d goroutines, base %d)", sent.Load(), runtime.NumGoroutine(), base)
	}
	if _, ok := h.Get("flooder"); ok {
		t.Error("the flooding session is still registered")
	}
	// Let the few goroutines of the allowed pings drain, then check the bound.
	deadline := time.Now().Add(8 * time.Second)
	for runtime.NumGoroutine() > base+25 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > base+25 {
		t.Errorf("goroutines = %d (base %d): the flood leaked goroutines", g, base)
	}
	if sent.Load() > 5000 {
		t.Errorf("%d pings were read before the session ended", sent.Load())
	}
}
