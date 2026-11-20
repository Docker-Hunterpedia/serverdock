package limits

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestKeyedBurstAndRefill(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	k := NewKeyed(5, 10, 0, clk.Now) // 5/min, burst 10
	if b, _ := k.Blocked("a"); b {
		t.Fatal("fresh key blocked")
	}
	for i := 0; i < 10; i++ {
		if !k.Take("a") {
			t.Fatalf("take %d refused inside the burst", i)
		}
	}
	b, retry := k.Blocked("a")
	if !b || retry <= 0 || retry > 13*time.Second {
		t.Fatalf("after burst: blocked=%v retry=%v", b, retry)
	}
	if k.Take("a") {
		t.Error("11th take allowed")
	}
	if b, _ := k.Blocked("other"); b {
		t.Error("another key is blocked")
	}
	clk.Add(12 * time.Second) // 5/min = one token per 12 s
	if b, _ := k.Blocked("a"); b {
		t.Error("still blocked after a refill period")
	}
	if !k.Take("a") || k.Take("a") {
		t.Error("refill granted the wrong number of tokens")
	}
	clk.Add(time.Hour)
	if k.Take("a"); k.Len() != 1 {
		t.Errorf("len %d", k.Len())
	}
}

func TestKeyedMapIsBounded(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	k := NewKeyed(1, 2, 0, clk.Now)
	for i := 0; i < MaxKeys+300; i++ {
		k.Take(fmt.Sprintf("k%d", i))
		if k.Len() > MaxKeys {
			t.Fatalf("map grew to %d", k.Len())
		}
		if i%1000 == 0 {
			clk.Add(time.Second)
		}
	}
}

// A blocked bucket is never evicted (that would give its owner a fresh budget);
// when the map is full of blocked keys a new key is refused and treated as blocked.
func TestEvictionNeverDropsABlockedKey(t *testing.T) {
	clk := &clock{t: time.Unix(5000, 0)}
	k := NewKeyed(60, 1, 8, clk.Now) // burst 1, one token per second, 8 keys
	for i := 0; i < 8; i++ {
		if !k.Take(fmt.Sprintf("hot%d", i)) { // drains each key's only token
			t.Fatalf("hot%d", i)
		}
		clk.Add(time.Millisecond)
	}
	for i := 0; i < 8; i++ {
		if b, _ := k.Blocked(fmt.Sprintf("hot%d", i)); !b {
			t.Fatalf("hot%d should be blocked", i)
		}
	}
	// Every key blocked: a new key is refused, not squeezed in by evicting one.
	if k.Take("new") {
		t.Error("a new key was admitted while every slot is blocked")
	}
	if b, wait := k.Blocked("new"); !b || wait < time.Second {
		t.Errorf("an untrackable new key must be treated as blocked: %v %v", b, wait)
	}
	if k.Len() != 8 {
		t.Errorf("len %d", k.Len())
	}
	for i := 0; i < 8; i++ {
		if b, _ := k.Blocked(fmt.Sprintf("hot%d", i)); !b {
			t.Errorf("hot%d lost its block", i)
		}
	}
	// Some blocks expire: only the unblocked slots are reusable.
	k2 := NewKeyed(60, 1, 4, clk.Now)
	k2.Take("a0")
	clk.Add(100 * time.Millisecond)
	k2.Take("a1")
	clk.Add(900 * time.Millisecond)
	k2.Take("a2")
	clk.Add(100 * time.Millisecond)
	k2.Take("a3")
	clk.Add(400 * time.Millisecond) // a0 and a1 are over a second old; a2 and a3 are not
	if b, _ := k2.Blocked("a0"); b {
		t.Fatal("a0 should have a token")
	}
	if b, _ := k2.Blocked("a3"); !b {
		t.Fatal("a3 should still be blocked")
	}
	if !k2.Take("fresh") {
		t.Fatal("a new key was refused although unblocked slots exist")
	}
	for _, key := range []string{"a2", "a3"} {
		if b, _ := k2.Blocked(key); !b {
			t.Errorf("%s was evicted while blocked", key)
		}
	}
}

func TestIPv6KeyedBySlash64(t *testing.T) {
	a := Key(net.ParseIP("2001:db8:1:2::1"))
	b := Key(net.ParseIP("2001:db8:1:2:ffff:ffff:ffff:ffff"))
	c := Key(net.ParseIP("2001:db8:1:3::1"))
	if a != b {
		t.Errorf("same /64 got different keys: %s %s", a, b)
	}
	if a == c {
		t.Error("different /64s share a key")
	}
	if Key(net.ParseIP("::ffff:192.0.2.9")) != "192.0.2.9" || Key(net.ParseIP("192.0.2.9")) != "192.0.2.9" {
		t.Error("IPv4 / IPv4-mapped keying")
	}
	clk := &clock{t: time.Unix(0, 0)}
	k := NewKeyed(1, 1, 0, clk.Now)
	k.Take(a)
	if b, _ := k.Blocked(b); !b {
		t.Error("rotating the low 64 bits escaped the limiter")
	}
}

func TestClientIPAndXFF(t *testing.T) {
	req := func(remote string, xff ...string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		for _, x := range xff {
			r.Header.Add("X-Forwarded-For", x)
		}
		return r
	}
	// From a non-loopback peer, XFF is ignored (even when the proxy is "trusted").
	if got := ClientIP(req("203.0.113.5:1234", "198.51.100.1"), true); got != "203.0.113.5" {
		t.Errorf("non-loopback peer: %s", got)
	}
	// From loopback with trust: the last hop.
	if got := ClientIP(req("127.0.0.1:1", "6.6.6.6, 198.51.100.7"), true); got != "198.51.100.7" {
		t.Errorf("loopback last hop: %s", got)
	}
	if got := ClientIP(req("[::1]:1", "1.1.1.1", "198.51.100.8"), true); got != "198.51.100.8" {
		t.Errorf("last header: %s", got)
	}
	// Loopback but not trusted, junk XFF, or none: the peer.
	if got := ClientIP(req("127.0.0.1:1", "198.51.100.7"), false); got != "127.0.0.1" {
		t.Errorf("untrusted: %s", got)
	}
	if got := ClientIP(req("127.0.0.1:1", "not-an-ip"), true); got != "127.0.0.1" {
		t.Errorf("junk XFF: %s", got)
	}
	if got := ClientIP(req("[2001:db8::7]:9"), false); got != "2001:db8::/64" {
		t.Errorf("v6 peer: %s", got)
	}
}

// fakeListener hands out conns with chosen remote addresses.
type fakeListener struct {
	ch     chan net.Conn
	closed chan struct{}
}

func newFakeListener() *fakeListener {
	return &fakeListener{ch: make(chan net.Conn, 16), closed: make(chan struct{})}
}
func (f *fakeListener) Accept() (net.Conn, error) {
	select {
	case c := <-f.ch:
		return c, nil
	case <-f.closed:
		return nil, net.ErrClosed
	}
}
func (f *fakeListener) Close() error   { close(f.closed); return nil }
func (f *fakeListener) Addr() net.Addr { return &net.TCPAddr{} }

type fakeConn struct {
	net.Conn
	remote net.Addr
	closed chan struct{}
	once   sync.Once
}

func newFakeConn(ip string) *fakeConn {
	a, _ := net.Pipe()
	return &fakeConn{Conn: a, remote: &net.TCPAddr{IP: net.ParseIP(ip), Port: 1}, closed: make(chan struct{})}
}
func (c *fakeConn) RemoteAddr() net.Addr { return c.remote }
func (c *fakeConn) Close() error         { c.once.Do(func() { close(c.closed) }); return c.Conn.Close() }

func TestListenerPerIPCapClosesInAccept(t *testing.T) {
	fl := newFakeListener()
	l := NewListener(fl, 0, 4, nil)
	defer l.Close()
	var kept []net.Conn
	for i := 0; i < 4; i++ {
		fl.ch <- newFakeConn("203.0.113.1")
		c, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		kept = append(kept, c)
	}
	over := newFakeConn("203.0.113.1")
	other := newFakeConn("203.0.113.2")
	fl.ch <- over
	fl.ch <- other
	// The 5th from the same IP is closed inside Accept; Accept moves on and
	// returns the other IP's connection.
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-over.closed:
	default:
		t.Error("5th connection from one IP was not closed in Accept")
	}
	select {
	case <-other.closed:
		t.Error("another IP's connection was closed")
	default:
	}
	if c.RemoteAddr().String() != other.RemoteAddr().String() {
		t.Errorf("Accept returned %v", c.RemoteAddr())
	}
	if l.Active("203.0.113.1") != 4 {
		t.Errorf("active = %d", l.Active("203.0.113.1"))
	}
	// Closing one frees a slot; closing twice does not double-release.
	kept[0].Close()
	kept[0].Close()
	if l.Active("203.0.113.1") != 3 {
		t.Errorf("active after close = %d", l.Active("203.0.113.1"))
	}
	fl.ch <- newFakeConn("203.0.113.1")
	if _, err := l.Accept(); err != nil {
		t.Fatal(err)
	}
}

func TestListenerGlobalCapBlocksAndReleases(t *testing.T) {
	fl := newFakeListener()
	l := NewListener(fl, 2, 0, nil)
	fl.ch <- newFakeConn("203.0.113.1")
	fl.ch <- newFakeConn("203.0.113.2")
	fl.ch <- newFakeConn("203.0.113.3")
	c1, _ := l.Accept()
	l.Accept()
	got := make(chan struct{})
	go func() {
		if c, err := l.Accept(); err == nil {
			_ = c
			close(got)
		}
	}()
	select {
	case <-got:
		t.Fatal("Accept returned above the global cap")
	case <-time.After(100 * time.Millisecond):
	}
	c1.Close()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("Accept did not resume after a connection closed")
	}
	// Close unblocks a waiting Accept.
	errc := make(chan error, 1)
	go func() { _, err := l.Accept(); errc <- err }()
	time.Sleep(50 * time.Millisecond)
	l.Close()
	select {
	case err := <-errc:
		if err == nil {
			t.Error("Accept succeeded on a closed listener")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock Accept")
	}
}

// Real sockets: the 5th concurrent connection from 127.0.0.1 is closed by the server.
func TestListenerRealSockets(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := NewListener(raw, 0, 4, nil)
	defer l.Close()
	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	var clients []net.Conn
	for i := 0; i < 5; i++ {
		c, err := net.Dial("tcp", raw.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
		defer c.Close()
	}
	// The 5th is closed by the server: its read ends promptly with EOF/reset.
	closedByServer := 0
	for _, c := range clients {
		c.SetReadDeadline(time.Now().Add(120 * time.Millisecond))
		if _, err := c.Read(make([]byte, 1)); err != nil {
			if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
				closedByServer++
			}
		}
	}
	if closedByServer != 1 || len(accepted) != 4 {
		t.Errorf("closed by server = %d, accepted = %d; want 1 and 4", closedByServer, len(accepted))
	}
}
