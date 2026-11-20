package agent

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"time"
)

// yamux answers every ping SYN in a new goroutine that waits up to
// ConnectionWriteTimeout to queue its reply. A peer that sends pings and never
// reads makes those goroutines (and their memory) pile up without limit; a
// scratch program reached 640k goroutines and 2 GiB in one second. The guard
// parses the frames the peer sends, counts ping SYNs, and ends the connection
// past a modest rate. Legitimate peers ping once per keep-alive interval (30 s).
const (
	// MaxPingsPerSecond is the sustained ping SYN rate a peer may send.
	MaxPingsPerSecond = 4
	// PingBurst is how many ping SYNs may arrive back to back.
	PingBurst = 16

	yamuxHeaderLen = 12
	yamuxTypeData  = 0
	yamuxTypePing  = 2
	yamuxFlagSYN   = 1
)

// ErrPingFlood is returned (and the connection closed) when a peer exceeds the ping rate.
var ErrPingFlood = errors.New("agent: peer is flooding yamux pings; connection closed")

// GuardPings wraps conn so that reading more than the allowed rate of yamux
// ping SYN frames closes it. Reads of one connection must not be concurrent
// (yamux has a single receive loop).
func GuardPings(conn net.Conn) net.Conn { return newPingGuard(conn, time.Now) }

type pingGuard struct {
	net.Conn
	now func() time.Time

	// Frame parser state (touched only by Read).
	hdr  [yamuxHeaderLen]byte
	hn   int   // bytes of the current header collected
	skip int64 // payload bytes of a data frame still to pass over

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newPingGuard(c net.Conn, now func() time.Time) *pingGuard {
	return &pingGuard{Conn: c, now: now, tokens: PingBurst, last: now()}
}

func (g *pingGuard) Read(p []byte) (int, error) {
	n, err := g.Conn.Read(p)
	if n > 0 && g.feed(p[:n]) {
		_ = g.Conn.Close()
		return 0, ErrPingFlood
	}
	return n, err
}

// feed parses b and reports whether the ping budget was exceeded.
func (g *pingGuard) feed(b []byte) (flood bool) {
	for len(b) > 0 {
		if g.skip > 0 {
			k := min(int64(len(b)), g.skip)
			g.skip -= k
			b = b[k:]
			continue
		}
		k := copy(g.hdr[g.hn:], b)
		g.hn += k
		b = b[k:]
		if g.hn < yamuxHeaderLen {
			continue
		}
		g.hn = 0
		typ := g.hdr[1]
		flags := binary.BigEndian.Uint16(g.hdr[2:4])
		length := binary.BigEndian.Uint32(g.hdr[8:12])
		switch {
		case typ == yamuxTypeData:
			g.skip = int64(length)
		case typ == yamuxTypePing && flags&yamuxFlagSYN != 0:
			if !g.takePing() {
				return true
			}
		}
	}
	return false
}

func (g *pingGuard) takePing() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.tokens = min(PingBurst, g.tokens+now.Sub(g.last).Seconds()*MaxPingsPerSecond)
	g.last = now
	if g.tokens < 1 {
		return false
	}
	g.tokens--
	return true
}
