package hub

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
)

// A LocalOnly op is refused on a real remote session (no stream is even
// opened) and allowed on the in-process local session. logs, the only
// read-only op, works on both.
func TestLocalOnlyOpRefusedOnRemoteAllowedOnLocal(t *testing.T) {
	spec := agent.OpSpec{Name: "test-write-op", LocalOnly: true}

	e := newEnv(t)
	startAgent(t, e.wsURL(), "remote1", newFakeLogs(), nil)
	remote := waitSession(t, e.hub, "remote1", 0)
	if st, err := remote.open(spec); !errors.Is(err, ErrLocalOnly) || st != nil {
		t.Fatalf("remote LocalOnly op: stream=%v err=%v", st, err)
	}
	if n := remote.ys.NumStreams(); n != 0 {
		t.Errorf("a stream was opened on the remote session (%d)", n)
	}
	// Case variants and lookalikes of the local id are not local.
	for _, id := range []string{"Local", "local ", "locall"} {
		if s := (&Session{ID: id}); !errors.Is(func() error { _, err := s.open(spec); return err }(), ErrLocalOnly) {
			t.Errorf("session id %q treated as local", id)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := New(ctx, "", quiet)
	a, b := net.Pipe()
	go func() { _ = h.Attach(ctx, LocalID, a, 0) }()
	go func() { _ = agent.ServeSession(ctx, b, newFakeLogs(), 0, quiet) }()
	local := waitSession(t, h, LocalID, 0)
	st, err := local.open(spec)
	if err != nil {
		t.Fatalf("local LocalOnly op refused: %v", err)
	}
	st.Close()

	// The read-only op is allowed remotely.
	rc, err := remote.Logs(ctx, "c1", 5)
	if err != nil {
		t.Fatalf("remote logs: %v", err)
	}
	rc.Close()
}
