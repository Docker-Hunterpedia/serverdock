package deploy

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
)

type bootSeq struct{ seq []string }

type fakeEnsurer struct {
	s    *bootSeq
	err  error
	name string
	sub  string
}

func (f *fakeEnsurer) NetworkEnsure(_ context.Context, name, subnet string) (agentops.NetworkResult, error) {
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
	s := &bootSeq{}
	st := openStore(t)
	px := &fakeProxy{events: &s.seq}
	r := NewRoutes(st, px, nil)
	en := &fakeEnsurer{s: s}
	sn := &fakeSubnet{s: s, subnet: "10.250.7.0/24"}
	src := &fakeSource{seq: &s.seq}
	b := NewBooter(BootConfig{Instance: bootInstance, AppNet: sn, Net: en, Routes: r, Src: src, SyncRoutes: sync})
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
	if !r.Ready() || b.NetworkProblem() != "" {
		t.Errorf("ready=%v problem=%q", r.Ready(), b.NetworkProblem())
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

// A failed network_ensure (Docker down, a foreign network of that name) is
// reported and remembered for deploys, and does not stop Sync: containers that
// already run keep their routes. A later Run retries only what failed.
func TestBootNetworkFailureDoesNotStopSyncAndIsRetried(t *testing.T) {
	b, s, en, _, _, r := newBooter(t, true)
	en.err = errors.New("dockerapi: a network with this name exists and is not ServerDock's /var/run/x")
	err := b.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "network_ensure") {
		t.Fatalf("%v", err)
	}
	if !r.Ready() {
		t.Error("Sync was skipped because the network failed")
	}
	if p := b.NetworkProblem(); p == "" || strings.Contains(p, "/var/run") {
		t.Errorf("problem %q (must be worded for the user, not Docker's text)", p)
	}
	en.err = nil
	s.seq = nil
	if err := b.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.seq, ","); got != "subnet,check,network_ensure" { // the gate is open: no second Sync
		t.Errorf("retry sequence %s", got)
	}
	if b.NetworkProblem() != "" {
		t.Errorf("problem %q after success", b.NetworkProblem())
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
	if p := b.NetworkProblem(); !strings.Contains(p, "10.250.7.0/24") || !strings.Contains(p, "tun0") || strings.Contains(p, "--app-subnet") {
		t.Errorf("problem %q", p)
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
