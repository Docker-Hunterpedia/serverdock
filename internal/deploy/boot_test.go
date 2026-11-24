package deploy

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
)

type bootSeq struct{ seq []string }

type fakeEnsurer struct {
	s    *bootSeq
	err  error
	name string
	sub  string
	// block, if set, makes NetworkEnsure wait for it (or its context); entered
	// is signalled when the call starts; calls counts the calls.
	block   chan struct{}
	entered chan struct{}
	calls   atomic.Int32
}

func (f *fakeEnsurer) NetworkEnsure(ctx context.Context, name, subnet string) (agentops.NetworkResult, error) {
	f.calls.Add(1)
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return agentops.NetworkResult{}, ctx.Err()
		}
	}
	f.s.seq = append(f.s.seq, "network_ensure")
	f.name, f.sub = name, subnet
	return agentops.NetworkResult{Subnet: subnet}, f.err
}

type fakeSubnet struct {
	s        *bootSeq
	subnet   string
	subErr   error
	checkErr error
}

func (f *fakeSubnet) Subnet(context.Context) (netip.Prefix, error) {
	f.s.seq = append(f.s.seq, "subnet")
	if f.subErr != nil {
		return netip.Prefix{}, f.subErr
	}
	return netip.MustParsePrefix(f.subnet), nil
}

func (f *fakeSubnet) Check(context.Context) error {
	f.s.seq = append(f.s.seq, "check")
	return f.checkErr
}

const bootInstance = "0123456789abcdef0123456789abcdef"

func newBooter(t *testing.T, sync bool) (*Booter, *bootSeq, *fakeEnsurer, *fakeSubnet, *fakeSource, *Routes) {
	t.Helper()
	return newBooterAfter(t, sync, nil)
}

func newBooterAfter(t *testing.T, sync bool, panelUp <-chan struct{}) (*Booter, *bootSeq, *fakeEnsurer, *fakeSubnet, *fakeSource, *Routes) {
	t.Helper()
	s := &bootSeq{}
	st := openStore(t)
	px := &fakeProxy{events: &s.seq}
	r := NewRoutes(st, px, nil)
	en := &fakeEnsurer{s: s}
	sn := &fakeSubnet{s: s, subnet: "10.250.7.0/24"}
	src := &fakeSource{seq: &s.seq}
	b := NewBooter(BootConfig{Instance: bootInstance, AppNet: sn, Net: en, Routes: r, Src: src, SyncRoutes: sync, PanelUp: panelUp})
	return b, s, en, sn, src, r
}

// The boot order: the panel runs alone first (the local agent, outside this
// type), then network_ensure with the recorded subnet, then Sync, which opens
// the route gate. No route is sent before network_ensure.
func TestBootOrderIsNetworkEnsureThenSync(t *testing.T) {
	b, s, en, _, _, r := newBooter(t, true)
	if r.Ready() {
		t.Fatal("ready before boot")
	}
	if err := b.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.seq, ","); got != "subnet,check,network_ensure,list" {
		t.Errorf("sequence %s", got)
	}
	if en.name != agentops.NetworkName(bootInstance) || en.sub != "10.250.7.0/24" {
		t.Errorf("network_ensure(%q, %q)", en.name, en.sub)
	}
	if st := b.NetworkStatus(); !r.Ready() || st.State != NetOK || st.Reason != "" {
		t.Errorf("ready=%v status=%+v", r.Ready(), st)
	}
	// Run again: nothing is repeated once it all succeeded.
	n := len(s.seq)
	if err := b.Run(context.Background()); err != nil || len(s.seq) != n {
		t.Errorf("a second Run repeated work: %v %v", err, s.seq[n:])
	}
}

func TestBootWithTheProxyOffEnsuresTheNetworkOnly(t *testing.T) {
	b, s, _, _, _, r := newBooter(t, false)
	if err := b.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.seq, ","); got != "subnet,check,network_ensure" || r.Ready() {
		t.Errorf("%s ready=%v", got, r.Ready())
	}
}

// A network_ensure the agent refuses because of the network itself (a foreign
// network with our name, another subnet, ICC on, another gateway or range) is a
// PROBLEM with a distinct reason for new deploys, and does not stop Sync:
// containers that already run keep their routes. A later Run retries only what
// failed.
func TestBootNetworkMismatchIsAProblemAndIsRetried(t *testing.T) {
	cases := []struct {
		name, msg, code string
	}{
		{"foreign", "a network with this name exists and is not ServerDock's", agent.CodeConflict},
		{"subnet", "the network exists with another subnet (10.9.0.0/24, asked for 10.250.7.0/24)", agent.CodeConflict},
		{"icc", "the network exists with other settings: inter-container traffic is not disabled", agent.CodeConflict},
		{"gateway", "the network exists with other settings: gateway is \"10.250.7.5\", not 10.250.7.1", agent.CodeConflict},
		{"range", "the network exists with other settings: IPRange is \"\", not 10.250.7.192/26", agent.CodeConflict},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, s, en, _, _, r := newBooter(t, true)
			en.err = &agent.OpError{Code: c.code, Msg: c.msg}
			err := b.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "network_ensure") {
				t.Fatalf("%v", err)
			}
			if !r.Ready() {
				t.Error("Sync was skipped because the network failed")
			}
			st := b.NetworkStatus()
			if st.State != NetProblem || !strings.Contains(st.Reason, c.msg) {
				t.Fatalf("status %+v does not carry %q", st, c.msg)
			}
			if seen[st.Reason] {
				t.Errorf("two cases share the reason %q", st.Reason)
			}
			seen[st.Reason] = true
			en.err = nil
			s.seq = nil
			if err := b.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(s.seq, ","); got != "subnet,check,network_ensure" { // the gate is open: no second Sync
				t.Errorf("retry sequence %s", got)
			}
			if st := b.NetworkStatus(); st.State != NetOK || st.Reason != "" {
				t.Errorf("status %+v after success", st)
			}
		})
	}
}

// Docker down (or any error that is not about the network) is UNKNOWN, never a
// verdict: the deploy that sees it tries again.
func TestBootDockerFailureIsNetworkUnknownNotAProblem(t *testing.T) {
	for _, err := range []error{
		errors.New("dial unix /var/run/docker.sock: connection refused"),
		&agent.OpError{Code: agent.CodeDocker, Msg: "the Docker request failed"},
		&agent.OpError{Code: agent.CodeTimeout, Msg: "timed out"},
	} {
		b, _, en, _, _, _ := newBooter(t, true)
		en.err = err
		if b.Run(context.Background()) == nil {
			t.Fatalf("%v: no error", err)
		}
		if st := b.NetworkStatus(); st.State != NetUnknown || st.Reason != "" {
			t.Errorf("%v: status %+v", err, st)
		}
	}
	// Before any attempt it is unknown too.
	b, _, _, _, _, _ := newBooter(t, true)
	if st := b.NetworkStatus(); st.State != NetUnknown {
		t.Errorf("before the first run: %+v", st)
	}
}

// container_create (or the allocator) reporting the network missing, or a prune,
// makes the next Run ensure it again even though it was ensured before.
func TestNetworkGoneMakesTheNextRunEnsureAgain(t *testing.T) {
	b, s, en, _, _, _ := newBooter(t, true)
	if err := b.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if en.calls.Load() != 1 {
		t.Fatalf("ensure calls %d", en.calls.Load())
	}
	if err := b.Run(context.Background()); err != nil || en.calls.Load() != 1 {
		t.Fatalf("a second Run ensured again: %v %d", err, en.calls.Load())
	}
	b.NetworkGone()
	if st := b.NetworkStatus(); st.State != NetUnknown {
		t.Errorf("after NetworkGone: %+v", st)
	}
	s.seq = nil
	if err := b.Run(context.Background()); err != nil || en.calls.Load() != 2 {
		t.Fatalf("%v ensure calls %d", err, en.calls.Load())
	}
	if st := b.NetworkStatus(); st.State != NetOK {
		t.Errorf("status %+v", st)
	}
}

func TestIsNetworkMissingMatchesOnlyTheNetworkError(t *testing.T) {
	if !agentops.IsNetworkMissing(&agent.OpError{Code: agent.CodeNotFound, Msg: agentops.MsgNetworkMissing}) {
		t.Error("the network error is not recognised")
	}
	for _, err := range []error{
		&agent.OpError{Code: agent.CodeNotFound, Msg: "not found"}, // an image or container
		&agent.OpError{Code: agent.CodeConflict, Msg: agentops.MsgNetworkMissing},
		errors.New(agentops.MsgNetworkMissing), nil,
	} {
		if agentops.IsNetworkMissing(err) {
			t.Errorf("%v recognised as the missing network", err)
		}
	}
}

// OnReady is the interim start gate: it fires only once the first list has
// filled the state map, and again (harmlessly) after later runs.
func TestBootOnReadyFiresOnlyAfterTheFirstList(t *testing.T) {
	s := &bootSeq{}
	st := openStore(t)
	r := NewRoutes(st, &fakeProxy{events: &s.seq}, nil)
	src := &fakeSource{seq: &s.seq, listErr: errors.New("docker down")}
	var n atomic.Int32
	b := NewBooter(BootConfig{Instance: bootInstance, AppNet: &fakeSubnet{s: s, subnet: "10.250.7.0/24"}, Net: &fakeEnsurer{s: s},
		Routes: r, Src: src, SyncRoutes: true, OnReady: func() { n.Add(1) }})
	if b.Run(context.Background()) == nil || n.Load() != 0 {
		t.Fatalf("the gate opened with the list failing (%d)", n.Load())
	}
	src.listErr = nil
	if err := b.Run(context.Background()); err != nil || n.Load() != 1 {
		t.Fatalf("%v opened %d times", err, n.Load())
	}
	// With the proxy off there is no list: the network being ensured is the signal.
	n.Store(0)
	s2 := &bootSeq{}
	b2 := NewBooter(BootConfig{Instance: bootInstance, AppNet: &fakeSubnet{s: s2, subnet: "10.250.7.0/24"}, Net: &fakeEnsurer{s: s2},
		Routes: NewRoutes(openStore(t), &fakeProxy{events: &s2.seq}, nil), Src: &fakeSource{seq: &s2.seq}, SyncRoutes: false, OnReady: func() { n.Add(1) }})
	if err := b2.Run(context.Background()); err != nil || n.Load() != 1 {
		t.Fatalf("proxy off: %v opened %d times", err, n.Load())
	}
}

// An app subnet that now overlaps the host: the network is not created (it
// would collide), the reason is kept for new deploys, running apps are routed.
func TestBootOverlapRefusesTheNetworkButStillSyncs(t *testing.T) {
	b, s, _, sn, _, r := newBooter(t, true)
	sn.checkErr = &OverlapError{Subnet: netip.MustParsePrefix("10.250.7.0/24"), What: "the host route via tun0 (10.250.7.0/24)"}
	if err := b.Run(context.Background()); err == nil {
		t.Fatal("no error for an overlap")
	}
	for _, step := range s.seq {
		if step == "network_ensure" {
			t.Error("network_ensure ran for an overlapping subnet")
		}
	}
	if st := b.NetworkStatus(); st.State != NetProblem || !strings.Contains(st.Reason, "10.250.7.0/24") || !strings.Contains(st.Reason, "tun0") || strings.Contains(st.Reason, "--app-subnet") {
		t.Errorf("status %+v", st)
	}
	if !r.Ready() {
		t.Error("Sync was skipped")
	}
}

// A Sync failure leaves the gate closed (and the panel alone); the next Run tries again.
func TestBootSyncFailureLeavesTheGateClosed(t *testing.T) {
	b, _, _, _, src, r := newBooter(t, true)
	src.listErr = errors.New("docker down")
	if err := b.Run(context.Background()); err == nil || r.Ready() {
		t.Fatalf("%v ready=%v", err, r.Ready())
	}
	src.listErr = nil
	if err := b.Run(context.Background()); err != nil || !r.Ready() {
		t.Fatalf("%v ready=%v", err, r.Ready())
	}
}

func TestBootWithoutASubnetReportsItAndTriesAgain(t *testing.T) {
	b, _, _, sn, _, _ := newBooter(t, false)
	sn.subErr = errors.New("cannot choose the app subnet until Docker answers")
	if err := b.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "app subnet") {
		t.Fatalf("%v", err)
	}
	sn.subErr = nil
	if err := b.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNewBooterNeedsItsDependencies(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("no panic")
		}
	}()
	NewBooter(BootConfig{})
}

// Run is single-flight: concurrent callers (every action that finds the gate
// closed) start one run between them; the others wait for its result, or leave
// when their own context ends. NetworkStatus never waits behind a run.
func TestBootRunIsSingleFlight(t *testing.T) {
	b, _, en, _, _, r := newBooter(t, true)
	en.block, en.entered = make(chan struct{}), make(chan struct{}, 8)
	// The first caller starts the run, which hangs in network_ensure.
	first := make(chan error, 1)
	go func() { first <- b.Run(context.Background()) }()
	<-en.entered
	// A waiter whose context ends leaves at once, without starting a run.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := b.Run(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("a waiter: %v after %v", err, time.Since(start))
	}
	// NetworkStatus does not block while the run is in flight.
	done := make(chan NetStatus, 1)
	go func() { done <- b.NetworkStatus() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("NetworkStatus blocked behind a running boot")
	}
	// Waiters that stay get the one run's result.
	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = b.Run(context.Background()) }()
	}
	time.Sleep(50 * time.Millisecond)
	close(en.block)
	wg.Wait()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("waiter %d: %v", i, err)
		}
	}
	if en.calls.Load() != 1 || !r.Ready() {
		t.Errorf("network_ensure ran %d times, ready=%v", en.calls.Load(), r.Ready())
	}
}

// A run that hangs ends with its own caller's context, and a waiter whose own
// context is alive does not inherit that cancellation: it starts a new run.
func TestBootRunEndsWithItsCallersContextAndWaitersStartANewOne(t *testing.T) {
	b, _, en, _, _, r := newBooter(t, true)
	en.block, en.entered = make(chan struct{}), make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- b.Run(ctx) }()
	<-en.entered
	waiter := make(chan error, 1)
	go func() { waiter <- b.Run(context.Background()) }() // waits on the first
	time.Sleep(30 * time.Millisecond)
	cancel()
	if err := <-res; err == nil {
		t.Error("the cancelled run reported success")
	}
	<-en.entered // the waiter started its own run
	close(en.block)
	if err := <-waiter; err != nil || !r.Ready() {
		t.Fatalf("the waiter: %v ready=%v", err, r.Ready())
	}
	if en.calls.Load() != 2 {
		t.Errorf("network_ensure ran %d times, want 2 (the cancelled one and the waiter's)", en.calls.Load())
	}
}

// A lazy Run before the panel is up starts nothing and says so through its
// context; after PanelUp the boot goes panel, then network_ensure, then Sync.
func TestBootRunWaitsForThePanel(t *testing.T) {
	panelUp := make(chan struct{})
	b, s, en, _, _, r := newBooterAfter(t, true, panelUp)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := b.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	if len(s.seq) != 0 || en.calls.Load() != 0 || r.Ready() {
		t.Fatalf("a run started before the panel was up: %v", s.seq)
	}
	close(panelUp)
	if err := b.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.seq, ","); got != "subnet,check,network_ensure,list" || !r.Ready() {
		t.Errorf("%s ready=%v", got, r.Ready())
	}
}
