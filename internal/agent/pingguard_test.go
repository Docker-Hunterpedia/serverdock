package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

func frame(typ byte, flags uint16, id, length uint32) []byte {
	h := make([]byte, 12)
	h[1] = typ
	binary.BigEndian.PutUint16(h[2:], flags)
	binary.BigEndian.PutUint32(h[4:], id)
	binary.BigEndian.PutUint32(h[8:], length)
	return h
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestGuard() (*pingGuard, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	a, _ := net.Pipe()
	return newPingGuard(a, clk.Now), clk
}

func TestPingGuardCountsOnlyPingSYNsAndSkipsDataPayloads(t *testing.T) {
	g, _ := newTestGuard()
	var stream bytes.Buffer
	// A data frame whose payload LOOKS like 20 ping SYN headers: not pings.
	fake := bytes.Repeat(frame(2, 1, 7, 7), 20)
	stream.Write(frame(0, 0, 1, uint32(len(fake))))
	stream.Write(fake)
	// Window updates, go-away and ping ACKs are not pings either.
	for i := 0; i < 50; i++ {
		stream.Write(frame(1, 0, 3, 1000))
		stream.Write(frame(2, 2, 5, uint32(i))) // ACK
	}
	stream.Write(frame(3, 0, 0, 0))
	// Exactly the burst of real ping SYNs is allowed.
	for i := 0; i < PingBurst; i++ {
		stream.Write(frame(2, 1, 0, uint32(i)))
	}
	// Whole, and one byte at a time (frames split across reads).
	for _, chunk := range []int{stream.Len(), 1, 5, 11, 13} {
		g, _ := newTestGuard()
		b := stream.Bytes()
		for len(b) > 0 {
			n := min(chunk, len(b))
			if g.feed(b[:n]) {
				t.Fatalf("chunk %d: tripped inside the burst", chunk)
			}
			b = b[n:]
		}
	}
	// One more ping SYN trips it, however the bytes are split.
	over := append(append([]byte{}, stream.Bytes()...), frame(2, 1, 0, 99)...)
	for _, chunk := range []int{len(over), 1, 7} {
		g, _ := newTestGuard()
		tripped := false
		for b := over; len(b) > 0 && !tripped; {
			n := min(chunk, len(b))
			tripped = g.feed(b[:n])
			b = b[n:]
		}
		if !tripped {
			t.Errorf("chunk %d: the %dth ping did not trip the guard", chunk, PingBurst+1)
		}
	}
	_ = g
}

func TestPingGuardRefillsAtTheSustainedRate(t *testing.T) {
	g, clk := newTestGuard()
	var pings []byte
	for i := 0; i < PingBurst; i++ {
		pings = append(pings, frame(2, 1, 0, uint32(i))...)
	}
	if g.feed(pings) {
		t.Fatal("burst tripped")
	}
	clk.Add(time.Second) // 4 pings per second refill
	for i := 0; i < MaxPingsPerSecond; i++ {
		if g.feed(frame(2, 1, 0, 1)) {
			t.Fatalf("refilled ping %d refused", i)
		}
	}
	if !g.feed(frame(2, 1, 0, 1)) {
		t.Error("pings above the sustained rate were allowed")
	}
}

func TestPingGuardClosesTheConnectionAndReportsIt(t *testing.T) {
	a, b := net.Pipe()
	g := GuardPings(a)
	go func() {
		var buf []byte
		for i := 0; i < 100; i++ {
			buf = append(buf, frame(2, 1, 0, uint32(i))...)
		}
		b.Write(buf)
	}()
	buf := make([]byte, 4096)
	n, err := g.Read(buf)
	if !errors.Is(err, ErrPingFlood) || n != 0 {
		t.Fatalf("read: %d %v", n, err)
	}
	if _, err := a.Write([]byte("x")); err == nil {
		t.Error("the underlying connection was not closed")
	}
	// Ordinary traffic passes through untouched.
	c, d := net.Pipe()
	g2 := GuardPings(c)
	go func() { d.Write([]byte("hello")); d.Close() }()
	got, _ := io.ReadAll(g2)
	if string(got) != "hello" {
		t.Errorf("passthrough: %q", got)
	}
}

// The agent side has the same guard: a hub (or anything on the other end) that
// floods pings ends Serve instead of piling up goroutines.
func TestServeEndsOnAPingFlood(t *testing.T) {
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), b, Config{Logs: blockingLogs{calls: &atomic.Int32{}}}, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	go func() {
		buf := make([]byte, 0, 12*200)
		for i := 0; i < 200; i++ {
			buf = append(buf, frame(2, 1, 0, uint32(i))...)
		}
		for {
			if _, err := a.Write(buf); err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve survived a ping flood")
	}
}

// G12: the agent never follows an HTTP redirect with its bearer token.
func TestAgentDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("the token reached the redirect target")
		}
	}))
	defer target.Close()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer hub.Close()
	var buf lockedBuf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = RunClient(ctx, ClientConfig{URL: "ws" + strings.TrimPrefix(hub.URL, "http") + ConnectPath, Token: "a-token-that-must-not-travel", ID: "a",
			Log: slog.New(slog.NewTextHandler(&buf, nil)), Backoff: Backoff{Min: 10 * time.Millisecond, Max: 20 * time.Millisecond}})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "redirect") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if !strings.Contains(buf.String(), "refusing to follow") {
		t.Errorf("no refusal logged: %s", buf.String())
	}
	if strings.Contains(buf.String(), "a-token-that-must-not-travel") {
		t.Error("token in the log")
	}
	if hits.Load() != 0 {
		t.Errorf("the redirect target was contacted %d times", hits.Load())
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

var _ = yamux.ErrSessionShutdown
