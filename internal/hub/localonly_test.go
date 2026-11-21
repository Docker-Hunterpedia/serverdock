package hub

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
)

// testSpecs adds a local-only op, an op that is RemoteOK, and nothing else.
func testSpecs(name string) (agent.OpSpec, bool) {
	switch name {
	case "test-write-op":
		return agent.OpSpec{Name: name}, true // zero value: local-only
	case "test-read-op":
		return agent.OpSpec{Name: name, RemoteOK: true}, true
	}
	return agent.SpecFor(name)
}

// An op that is not RemoteOK is refused on a real remote session (no stream is
// even opened) and allowed on the in-process local session; an unknown op is
// refused everywhere. logs, the only read-only op, works on both.
func TestLocalOnlyOpRefusedOnRemoteAllowedOnLocal(t *testing.T) {
	e := newEnv(t, withSpecFor(testSpecs))

	startAgent(t, e.wsURL(), "remote1", newFakeLogs(), nil)
	remote := waitSession(t, e.hub, "remote1", 0)

	if st, err := remote.open("test-write-op"); !errors.Is(err, ErrLocalOnly) || st != nil {
		t.Fatalf("remote local-only op: stream=%v err=%v", st, err)
	}
	if n := remote.ys.NumStreams(); n != 0 {
		t.Errorf("a stream was opened on the remote session (%d)", n)
	}
	if st, err := remote.open("no-such-op"); !errors.Is(err, ErrUnknownOp) || st != nil {
		t.Errorf("unknown op on remote: %v", err)
	}
	if st, err := remote.open("test-read-op"); err != nil {
		t.Errorf("RemoteOK op refused remotely: %v", err)
	} else {
		st.Close()
	}
	// Case variants and lookalikes of the local id are not local.
	for _, id := range []string{"Local", "local ", "locall"} {
		s := &Session{ID: id, specFor: testSpecs}
		if _, err := s.open("test-write-op"); !errors.Is(err, ErrLocalOnly) {
			t.Errorf("session id %q treated as local: %v", id, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := New(ctx, "", quiet, withSpecFor(testSpecs))
	a, b := net.Pipe()
	go func() { _ = h.Attach(ctx, LocalID, a, 0) }()
	go func() { _ = agent.Serve(ctx, b, agent.Config{Logs: newFakeLogs()}, 0, quiet) }()
	local := waitSession(t, h, LocalID, 0)

	st, err := local.open("test-write-op")
	if err != nil {
		t.Fatalf("local op refused: %v", err)
	}
	st.Close()
	if _, err := local.open("no-such-op"); !errors.Is(err, ErrUnknownOp) {
		t.Errorf("unknown op on local: %v", err)
	}

	rc, err := remote.Logs(ctx, "c1", 5)
	if err != nil {
		t.Fatalf("remote logs: %v", err)
	}
	rc.Close()
}
