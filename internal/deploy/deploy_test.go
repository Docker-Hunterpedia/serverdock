package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

const instance = "0123456789abcdef0123456789abcdef"

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustExec(t *testing.T, st *store.Store, q string, args ...any) {
	t.Helper()
	if _, err := st.W.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func addService(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	mustExec(t, st, `INSERT OR IGNORE INTO projects(id, name, created_at) VALUES (1, 'p', 'x')`)
	res, err := st.W.Exec(`INSERT INTO services(project_id, name, image_ref, health_kind, health_timeout_s, health_passes, strategy, drain_s, stop_grace_s, created_at, updated_at)
		VALUES (1, ?, 'nginx:1', 'tcp', 60, 3, 'blue_green', 10, 10, 'x', 'x')`, name)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func addDeploy(t *testing.T, st *store.Store, svc int64, number int, status string, ip any) int64 {
	t.Helper()
	res, err := st.W.Exec(`INSERT INTO deploys(service_id, number, kind, snapshot_json, ip, status, actor_kind, actor_id, created_at)
		VALUES (?, ?, 'deploy', '{}', ?, ?, 'user', '1', 'x')`, svc, number, ip, status)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// ---- the app subnet ----

type fakeNets struct {
	mu    sync.Mutex
	nets  []dockerapi.NetworkSubnet
	err   error
	calls int
}

func (f *fakeNets) NetworkSubnets(context.Context) ([]dockerapi.NetworkSubnet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.nets, f.err
}

func net1(name, id string, subnets ...string) dockerapi.NetworkSubnet {
	n := dockerapi.NetworkSubnet{Name: name, ID: id}
	for _, s := range subnets {
		n.Subnets = append(n.Subnets, netip.MustParsePrefix(s))
	}
	return n
}

func noHost(string) ([]Used, error) { return nil, nil }

func newAppNet(t *testing.T, st *store.Store, d DockerNetworks, flag string, host func(string) ([]Used, error)) *AppNet {
	t.Helper()
	a, err := NewAppNet(AppNetOptions{Store: st, Instance: instance, Flag: flag, Docker: d, Host: host, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAppSubnetFlagIsValidatedFromSlash24ToSlash20(t *testing.T) {
	ok := []string{"100.64.0.0/24", "100.127.255.0/24", "172.16.0.0/24", "172.31.240.0/20", "10.250.0.0/24", "10.250.0.0/23", "10.250.0.0/22", "10.250.16.0/20", "192.168.100.0/24", "172.30.0.0/20"}
	for _, s := range ok {
		if _, err := ParseAppSubnet(s); err != nil {
			t.Errorf("%s refused: %v", s, err)
		}
	}
	bad := []string{"", "10.250.0.0/25", "10.250.0.0/26", "10.250.0.0/19", "10.250.0.0/16", "10.250.0.0/8", "10.250.0.5/24", "10.250.0.0",
		"fd00::/64", "::ffff:10.0.0.0/120", "127.0.0.0/24", "169.254.1.0/24", "224.0.0.0/20", "0.0.0.0/24",
		"8.8.8.0/24", "172.32.0.0/24", "172.15.255.0/24", "192.169.0.0/24", "100.128.0.0/24", "11.0.0.0/24", "193.168.0.0/24", " 10.250.0.0/24", "10.250.0.0/24 ", "010.250.0.0/24", "10.250.0.0/024"}
	for _, s := range bad {
		if _, err := ParseAppSubnet(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	st := openStore(t)
	if _, err := NewAppNet(AppNetOptions{Store: st, Docker: &fakeNets{}, Flag: "10.0.0.0/25"}); err == nil || !strings.Contains(err.Error(), "--app-subnet") {
		t.Errorf("NewAppNet with a /25: %v", err)
	}
}

func TestAppSubnetAvoidsExistingNetworks(t *testing.T) {
	st := openStore(t)
	d := &fakeNets{nets: []dockerapi.NetworkSubnet{
		net1("bridge", "b1", "172.17.0.0/16"),
		net1("a", "a1", "10.250.0.0/24"),
		net1("wide", "w1", "10.250.1.0/24"),
		net1("wider", "w2", "10.250.2.0/23"), // covers .2 and .3
		net1("v6", "v1"),
	}}
	host := func(string) ([]Used, error) {
		return []Used{{What: "a VPN route", Prefix: netip.MustParsePrefix("10.250.4.0/24")}}, nil
	}
	a := newAppNet(t, st, d, "", host)
	got, err := a.Subnet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "10.250.5.0/24" {
		t.Fatalf("chose %s, want the first /24 clear of the Docker networks and the host route", got)
	}
	var v string
	st.R.QueryRow(`SELECT value FROM meta WHERE key = 'app_subnet'`).Scan(&v)
	if v != "10.250.5.0/24" {
		t.Errorf("meta.app_subnet %q", v)
	}
	// Recorded once: a new instance reads it back without asking Docker, and a
	// network that appeared later does not change it.
	d.mu.Lock()
	d.nets = append(d.nets, net1("late", "l1", "10.250.5.0/24"))
	before := d.calls
	d.mu.Unlock()
	again, err := newAppNet(t, st, d, "10.9.0.0/24", noHost).Subnet(context.Background()) // flag after first start: ignored
	if err != nil || again.String() != "10.250.5.0/24" {
		t.Fatalf("second start: %s %v", again, err)
	}
	if d.calls != before {
		t.Error("the recorded subnet was recomputed")
	}
}

func TestAppSubnetFlagWinsAtFirstStart(t *testing.T) {
	st := openStore(t)
	d := &fakeNets{err: errors.New("docker down")} // a flag needs no Docker to be recorded
	got, err := newAppNet(t, st, d, "192.168.77.0/24", noHost).Subnet(context.Background())
	if err != nil || got.String() != "192.168.77.0/24" {
		t.Fatalf("%s %v", got, err)
	}
}

// An explicit --app-subnet that overlaps the host or a Docker network refuses
// the start and records nothing; a Docker that does not answer does not block it.
func TestAppSubnetFlagOverlapRefusesAndSavesNothing(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	d := &fakeNets{nets: []dockerapi.NetworkSubnet{net1("vpn", "v1", "192.168.77.0/25")}}
	_, err := newAppNet(t, st, d, "192.168.77.0/24", noHost).Subnet(ctx)
	var oe *OverlapError
	if !errors.As(err, &oe) || !oe.FirstStart || !strings.Contains(err.Error(), "vpn") || !strings.Contains(err.Error(), "choose another") {
		t.Fatalf("overlap with a Docker network: %v", err)
	}
	host := func(string) ([]Used, error) {
		return []Used{{What: "the host route via tun0", Prefix: netip.MustParsePrefix("192.168.0.0/16")}}, nil
	}
	if _, err := newAppNet(t, st, &fakeNets{}, "192.168.77.0/24", host).Subnet(ctx); !errors.As(err, &oe) || !strings.Contains(err.Error(), "tun0") {
		t.Fatalf("overlap with a host route: %v", err)
	}
	var n int
	st.R.QueryRow(`SELECT count(*) FROM meta WHERE key = 'app_subnet'`).Scan(&n)
	if n != 0 {
		t.Fatal("a refused subnet was saved")
	}
	if got, err := newAppNet(t, st, &fakeNets{err: errors.New("down")}, "192.168.77.0/24", noHost).Subnet(ctx); err != nil || got.String() != "192.168.77.0/24" {
		t.Fatalf("Docker down must not block an explicit subnet: %v %v", got, err)
	}
}

func TestAppSubnetNotRecordedWhileDockerIsUnreachable(t *testing.T) {
	st := openStore(t)
	d := &fakeNets{err: errors.New("dial unix /var/run/docker.sock: connect: no such file")}
	a := newAppNet(t, st, d, "", noHost)
	if _, err := a.Subnet(context.Background()); err == nil {
		t.Fatal("a subnet was chosen without knowing Docker's networks")
	}
	var n int
	st.R.QueryRow(`SELECT count(*) FROM meta WHERE key = 'app_subnet'`).Scan(&n)
	if n != 0 {
		t.Fatal("a guess was recorded")
	}
	d.mu.Lock()
	d.err = nil
	d.mu.Unlock()
	if got, err := a.Subnet(context.Background()); err != nil || got.String() != "10.250.0.0/24" {
		t.Fatalf("after Docker came back: %s %v", got, err)
	}
}

func TestChooseSubnetWhenThePoolIsFull(t *testing.T) {
	all := []Used{{What: "x", Prefix: netip.MustParsePrefix(DefaultPool)}}
	if _, err := ChooseSubnet(all); err == nil || !strings.Contains(err.Error(), "--app-subnet") {
		t.Errorf("full pool: %v", err)
	}
}

func TestAppSubnetOverlapRefusesDeploysWithAHint(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustExec(t, st, `INSERT INTO meta(key, value) VALUES ('app_subnet', '10.250.0.0/24')`)
	ours := agentops.NetworkName(instance)
	d := &fakeNets{nets: []dockerapi.NetworkSubnet{net1(ours, "abcdef0123456789", "10.250.0.0/24"), net1("bridge", "b", "172.17.0.0/16")}}
	var excluded []string
	host := func(ex string) ([]Used, error) { excluded = append(excluded, ex); return nil, nil }
	a := newAppNet(t, st, d, "", host)
	if err := a.Check(ctx); err != nil {
		t.Fatalf("our own network is not an overlap: %v", err)
	}
	if len(excluded) == 0 || excluded[len(excluded)-1] != "br-abcdef012345" {
		t.Errorf("our bridge was not excluded from the host check: %v", excluded)
	}

	// Another Docker network on the same range.
	d.mu.Lock()
	d.nets = append(d.nets, net1("vpn-net", "v", "10.250.0.128/25"))
	d.mu.Unlock()
	err := a.Check(ctx)
	var oe *OverlapError
	if !errors.As(err, &oe) || !strings.Contains(err.Error(), "not supported yet") || !strings.Contains(err.Error(), "vpn-net") || !strings.Contains(err.Error(), "10.250.0.0/24") {
		t.Fatalf("overlap with a Docker network: %v", err)
	}

	// A host interface or route added later.
	d.mu.Lock()
	d.nets = d.nets[:2]
	d.mu.Unlock()
	host2 := func(string) ([]Used, error) {
		return []Used{{What: "the host route via tun0", Prefix: netip.MustParsePrefix("10.250.0.0/16")}}, nil
	}
	err = newAppNet(t, st, d, "", host2).Check(ctx)
	if !errors.As(err, &oe) || !strings.Contains(err.Error(), "tun0") || !strings.Contains(err.Error(), "plan 0009") {
		t.Fatalf("overlap with a host route: %v", err)
	}

	// The allocator turns it into a refusal with a code, not a retry.
	al := NewAllocator(st, &fakeDocker{}, newAppNet(t, st, d, "", host2))
	svc := addService(t, st, "web")
	dep := addDeploy(t, st, svc, 1, "running", nil)
	_, err = al.Allocate(ctx, dep)
	var de *Error
	if !errors.As(err, &de) || de.Code != CodeAppSubnet || de.Retryable || !strings.Contains(err.Error(), "not supported yet") {
		t.Fatalf("allocation under an overlap: %v", err)
	}
}

func TestParseRoutesReadsProcNetRoute(t *testing.T) {
	const table = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	00000000	0102A8C0	0003	0	0	100	00000000	0	0	0
eth0	0000A8C0	00000000	0001	0	0	100	0000FFFF	0	0	0
br-aaaaaaaaaaaa	0000FA0A	00000000	0001	0	0	0	00FFFFFF	0	0	0
docker0	000011AC	00000000	0001	0	0	0	0000FFFF	0	0	0
weird	0000FA0A	00000000	0001	0	0	0	00FF00FF	0	0	0
short	1 2
`
	got, err := parseRoutes(strings.NewReader(table), "br-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	var ps []string
	for _, u := range got {
		ps = append(ps, u.Prefix.String())
	}
	if fmt.Sprint(ps) != "[192.168.0.0/16 172.17.0.0/16]" {
		t.Errorf("routes %v: the default route, our bridge, a non-contiguous mask and short lines must be skipped", ps)
	}
	all, _ := parseRoutes(strings.NewReader(table), "")
	found := false
	for _, u := range all {
		found = found || u.Prefix.String() == "10.250.0.0/24"
	}
	if !found {
		t.Errorf("10.250.0.0/24 (0000FA0A / 00FFFFFF) was not decoded: %v", all)
	}
	if _, err := hostNetworks("/nonexistent/route", ""); err != nil {
		t.Errorf("a host without /proc/net/route: %v", err)
	}
}

// ---- the allocator ----

type fakeDocker struct {
	mu         sync.Mutex
	list       []agentops.ListedContainer
	listErr    error
	used       []string
	inspectErr error
	subnet     string
	listCalls  int
	onList     func()
}

func (f *fakeDocker) ContainerList(context.Context) (agentops.ListResult, error) {
	f.mu.Lock()
	f.listCalls++
	hook := f.onList
	list, err := append([]agentops.ListedContainer(nil), f.list...), f.listErr
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return agentops.ListResult{Containers: list}, err
}

func (f *fakeDocker) NetworkInspect(context.Context) (agentops.UsedAddrs, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		return agentops.UsedAddrs{}, f.inspectErr
	}
	subnet := f.subnet
	if subnet == "" {
		subnet = "10.250.0.0/24"
	}
	return agentops.UsedAddrs{Subnet: subnet, Gateway: "10.250.0.1", IPRange: "10.250.0.192/26", Used: append([]string(nil), f.used...)}, nil
}

type fixedSubnet struct{ p netip.Prefix }

func (f fixedSubnet) Subnet(context.Context) (netip.Prefix, error) { return f.p, nil }
func (f fixedSubnet) Check(context.Context) error                  { return nil }

func newAlloc(t *testing.T, st *store.Store, d *fakeDocker) *Allocator {
	t.Helper()
	return NewAllocator(st, d, fixedSubnet{netip.MustParsePrefix("10.250.0.0/24")})
}

func running(t *testing.T, st *store.Store, svc int64, n int) int64 {
	t.Helper()
	return addDeploy(t, st, svc, n, "running", nil)
}

func TestIPAllocatorSkipsNetworkGatewayBroadcastAndTheDynamicRange(t *testing.T) {
	st := openStore(t)
	svc := addService(t, st, "web")
	d := &fakeDocker{}
	a := newAlloc(t, st, d)
	ctx := context.Background()
	first, err := a.Allocate(ctx, running(t, st, svc, 1))
	if err != nil || first.String() != "10.250.0.2" {
		t.Fatalf("first address %v %v: not .0 (network), not .1 (gateway)", first, err)
	}
	// Everything from .2 to .191 in use: the next is exhausted, never .192-.254
	// (Docker's dynamic slice) and never .255 (broadcast).
	for i := 3; i <= 191; i++ {
		d.used = append(d.used, fmt.Sprintf("10.250.0.%d", i))
	}
	got, err := a.Allocate(ctx, running(t, st, svc, 2))
	if err == nil {
		t.Fatalf("got %v with only the dynamic range and the broadcast address left", got)
	}
	if ErrorCode(err) != CodeIPPoolExhausted {
		t.Fatalf("error %v", err)
	}
	// .191 free again: it is the last static address and is handed out.
	d.used = d.used[:len(d.used)-1]
	got, err = a.Allocate(ctx, running(t, st, svc, 3))
	if err != nil || got.String() != "10.250.0.191" {
		t.Fatalf("last static address: %v %v", got, err)
	}
}

func TestIPAllocatorHeldRule(t *testing.T) {
	ctx := context.Background()
	type tc struct {
		name  string
		setup func(t *testing.T, st *store.Store, d *fakeDocker, svc int64)
	}
	cases := []tc{
		{"active deploy", func(t *testing.T, st *store.Store, _ *fakeDocker, svc int64) {
			id := addDeploy(t, st, svc, 10, "succeeded", "10.250.0.2")
			mustExec(t, st, `UPDATE services SET active_deploy_id = ? WHERE id = ?`, id, svc)
		}},
		{"queued deploy", func(t *testing.T, st *store.Store, _ *fakeDocker, svc int64) {
			addDeploy(t, st, svc, 10, "queued", "10.250.0.2")
		}},
		{"running deploy", func(t *testing.T, st *store.Store, _ *fakeDocker, svc int64) {
			addDeploy(t, st, svc, 10, "running", "10.250.0.2")
		}},
		{"exited container with our label", func(_ *testing.T, _ *store.Store, d *fakeDocker, _ int64) {
			d.list = []agentops.ListedContainer{{ID: strings.Repeat("a", 64), Name: "sd-p-web-d1-a1", State: "exited", ConfiguredIP: "10.250.0.2"}}
		}},
		{"stopped old container in a stop-first gap", func(_ *testing.T, _ *store.Store, d *fakeDocker, _ int64) {
			d.list = []agentops.ListedContainer{{ID: strings.Repeat("b", 64), State: "created", ConfiguredIP: "10.250.0.2"}}
		}},
		{"unowned (odd labels) container", func(_ *testing.T, _ *store.Store, d *fakeDocker, _ int64) {
			d.list = []agentops.ListedContainer{{ID: strings.Repeat("c", 64), State: "running", Unowned: true, ConfiguredIP: "10.250.0.2"}}
		}},
		{"running container with only a live address", func(_ *testing.T, _ *store.Store, d *fakeDocker, _ int64) {
			d.list = []agentops.ListedContainer{{ID: strings.Repeat("d", 64), State: "running", IP: "10.250.0.2"}}
		}},
		{"network_inspect entry", func(_ *testing.T, _ *store.Store, d *fakeDocker, _ int64) { d.used = []string{"10.250.0.2"} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := openStore(t)
			svc := addService(t, st, "web")
			d := &fakeDocker{}
			c.setup(t, st, d, svc)
			got, err := newAlloc(t, st, d).Allocate(ctx, running(t, st, svc, 99))
			if err != nil || got.String() != "10.250.0.3" {
				t.Fatalf("got %v %v, want .3 because .2 is held", got, err)
			}
		})
	}
	t.Run("a finished deploy that is not active holds nothing by itself", func(t *testing.T) {
		st := openStore(t)
		svc := addService(t, st, "web")
		addDeploy(t, st, svc, 10, "failed", "10.250.0.2")
		got, err := newAlloc(t, st, &fakeDocker{}).Allocate(ctx, running(t, st, svc, 99))
		if err != nil || got.String() != "10.250.0.2" {
			t.Fatalf("got %v %v: the address is free (its container, if any, would be in the list)", got, err)
		}
	})
	t.Run("a retry of the same deploy does not hold its own earlier address", func(t *testing.T) {
		st := openStore(t)
		svc := addService(t, st, "web")
		id := addDeploy(t, st, svc, 1, "running", "10.250.0.2")
		got, err := newAlloc(t, st, &fakeDocker{}).Allocate(ctx, id)
		if err != nil || got.String() != "10.250.0.2" {
			t.Fatalf("got %v %v", got, err)
		}
	})
	t.Run("the address is recorded", func(t *testing.T) {
		st := openStore(t)
		svc := addService(t, st, "web")
		id := running(t, st, svc, 1)
		got, _ := newAlloc(t, st, &fakeDocker{}).Allocate(ctx, id)
		var ip string
		st.R.QueryRow(`SELECT ip FROM deploys WHERE id = ?`, id).Scan(&ip)
		if ip != got.String() {
			t.Errorf("deploys.ip %q, allocated %v", ip, got)
		}
	})
	t.Run("only a queued or running deploy gets an address", func(t *testing.T) {
		st := openStore(t)
		svc := addService(t, st, "web")
		id := addDeploy(t, st, svc, 1, "succeeded", nil)
		if _, err := newAlloc(t, st, &fakeDocker{}).Allocate(ctx, id); err == nil {
			t.Error("a finished deploy was given an address")
		}
	})
}

func TestIPAllocatorAsksDockerFreshEveryTime(t *testing.T) {
	st := openStore(t)
	svc := addService(t, st, "web")
	d := &fakeDocker{}
	a := newAlloc(t, st, d)
	ctx := context.Background()
	a.Allocate(ctx, running(t, st, svc, 1))
	// A container appears between two allocations (with no DB row): the second
	// allocation must see it, because the list is not cached.
	d.mu.Lock()
	d.list = []agentops.ListedContainer{{ID: strings.Repeat("e", 64), State: "exited", ConfiguredIP: "10.250.0.3"}}
	d.mu.Unlock()
	got, err := a.Allocate(ctx, running(t, st, svc, 2))
	if err != nil || got.String() != "10.250.0.4" {
		t.Fatalf("got %v %v, want .4 (.2 is a live deploy, .3 an exited container)", got, err)
	}
	if d.listCalls != 2 {
		t.Errorf("container_list called %d times for 2 allocations", d.listCalls)
	}
}

func TestPoolExhaustedError(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	svc := addService(t, st, "web")
	var many []string
	for i := 2; i <= 191; i++ {
		many = append(many, fmt.Sprintf("10.250.0.%d", i))
	}
	_, err := newAlloc(t, st, &fakeDocker{used: many}).Allocate(ctx, running(t, st, svc, 1))
	var de *Error
	if !errors.As(err, &de) || de.Code != CodeIPPoolExhausted || de.Retryable || !strings.Contains(err.Error(), "10.250.0.0/24") {
		t.Fatalf("%v", err)
	}
	// network_inspect refusing (more addresses than it will report: conflict)
	// is also pool exhaustion, never a silent partial list.
	inspect := &agent.OpError{Code: agent.CodeConflict, Msg: "more than 1024 addresses in use"}
	_, err = newAlloc(t, st, &fakeDocker{inspectErr: inspect}).Allocate(ctx, running(t, st, svc, 2))
	if !errors.As(err, &de) || de.Code != CodeIPPoolExhausted || de.Retryable {
		t.Fatalf("network_inspect conflict: %v", err)
	}
}

func TestAllocatorFailsRetryableDockerWhenDockerIsUnreachable(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	svc := addService(t, st, "web")
	n := 0
	for name, d := range map[string]*fakeDocker{
		"container_list":  {listErr: &agent.OpError{Code: agent.CodeDocker, Msg: "the Docker request failed"}},
		"network_inspect": {inspectErr: &agent.OpError{Code: agent.CodeDocker, Msg: "the Docker request failed"}},
		"timeout":         {listErr: &agent.OpError{Code: agent.CodeTimeout, Msg: "timed out"}},
		"network missing": {inspectErr: &agent.OpError{Code: agent.CodeNotFound, Msg: "the app network does not exist"}},
	} {
		n++
		id := running(t, st, svc, n)
		_, err := newAlloc(t, st, d).Allocate(ctx, id)
		var de *Error
		if !errors.As(err, &de) || de.Code != CodeDocker || !de.Retryable {
			t.Errorf("%s: %v, want a retryable docker error", name, err)
		}
		var ip *string
		st.R.QueryRow(`SELECT ip FROM deploys WHERE id = ?`, id).Scan(&ip)
		if ip != nil {
			t.Errorf("%s: an address was recorded although Docker failed", name)
		}
	}
	// A cancelled context is not a Docker failure.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	d := &fakeDocker{listErr: context.Canceled}
	if _, err := newAlloc(t, st, d).Allocate(cctx, running(t, st, svc, 77)); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestAllocatorRefusesANetworkWithAnotherSubnetThanRecorded(t *testing.T) {
	st := openStore(t)
	svc := addService(t, st, "web")
	_, err := newAlloc(t, st, &fakeDocker{subnet: "10.99.0.0/24"}).Allocate(context.Background(), running(t, st, svc, 1))
	if ErrorCode(err) != CodeAppSubnet {
		t.Fatalf("%v", err)
	}
}

func TestTwoConcurrentAllocationsNeverShareAnAddress(t *testing.T) {
	defer lockCheckExempt()() // the test interleaves two allocators on one goroutine
	ctx := context.Background()
	st := openStore(t)
	s1, s2 := addService(t, st, "a"), addService(t, st, "b")
	d := &fakeDocker{onList: func() { time.Sleep(20 * time.Millisecond) }}
	a := newAlloc(t, st, d)
	ids := []int64{running(t, st, s1, 1), running(t, st, s2, 1)}
	got := make([]netip.Addr, 2)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ip, err := a.Allocate(ctx, ids[i])
			if err != nil {
				t.Error(err)
			}
			got[i] = ip
		}()
	}
	wg.Wait()
	if got[0] == got[1] {
		t.Fatalf("both deploys got %v", got[0])
	}
	// With the lock bypassed (two allocators), the unique index still refuses the
	// second: the loser moves on to the next address instead of sharing one.
	st2 := openStore(t)
	sa, sb := addService(t, st2, "a"), addService(t, st2, "b")
	ia, ib := running(t, st2, sa, 1), running(t, st2, sb, 1)
	a1, a2 := newAlloc(t, st2, &fakeDocker{}), newAlloc(t, st2, &fakeDocker{})
	// a2 takes .2 for deploy B right after a1 decided on .2 but before it wrote.
	a1.beforeWrite = func(ip netip.Addr) {
		if ip.String() == "10.250.0.2" && a1.beforeWrite != nil {
			a1.beforeWrite = nil
			if _, err := a2.Allocate(ctx, ib); err != nil {
				t.Error(err)
			}
		}
	}
	got1, err := a1.Allocate(ctx, ia)
	if err != nil {
		t.Fatal(err)
	}
	var other string
	st2.R.QueryRow(`SELECT ip FROM deploys WHERE id = ?`, ib).Scan(&other)
	if other != "10.250.0.2" || got1.String() != "10.250.0.3" {
		t.Fatalf("B has %s, A has %v: the index must have forced A to .3", other, got1)
	}
}

// ---- retention ----

func TestDeployRowsPrunedToTwoHundredKeepingTheActive(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	svc, other := addService(t, st, "web"), addService(t, st, "db")
	tx, err := st.W.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 250; n++ {
		status := "succeeded"
		if n%2 == 0 {
			status = "failed"
		}
		if _, err := tx.Exec(`INSERT INTO deploys(service_id, number, kind, snapshot_json, status, actor_kind, actor_id, created_at) VALUES (?, ?, 'deploy', '{}', ?, 'user', '1', 'x')`, svc, n, status); err != nil {
			t.Fatal(err)
		}
	}
	for n := 1; n <= 5; n++ {
		tx.Exec(`INSERT INTO deploys(service_id, number, kind, snapshot_json, status, actor_kind, actor_id, created_at) VALUES (?, ?, 'deploy', '{}', 'succeeded', 'user', '1', 'x')`, other, n)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var activeID, runningID int64
	st.R.QueryRow(`SELECT id FROM deploys WHERE service_id = ? AND number = 3`, svc).Scan(&activeID)
	st.R.QueryRow(`SELECT id FROM deploys WHERE service_id = ? AND number = 4`, svc).Scan(&runningID)
	mustExec(t, st, `UPDATE services SET active_deploy_id = ? WHERE id = ?`, activeID, svc)
	mustExec(t, st, `UPDATE deploys SET status = 'running' WHERE id = ?`, runningID)
	mustExec(t, st, `INSERT INTO deploy_stages(deploy_id, stage, state) SELECT id, 'pull', 'ok' FROM deploys WHERE service_id = ? AND number IN (1, 2)`, svc)

	n, err := PruneDeploys(ctx, st, svc)
	if err != nil {
		t.Fatal(err)
	}
	// 250 rows: the newest 200 stay (numbers 51..250), plus the active (#3) and the
	// running one (#4) which are old; 48 finished rows go.
	if n != 48 {
		t.Errorf("deleted %d rows, want 48", n)
	}
	var total, oldKept int
	st.R.QueryRow(`SELECT count(*) FROM deploys WHERE service_id = ?`, svc).Scan(&total)
	st.R.QueryRow(`SELECT count(*) FROM deploys WHERE service_id = ? AND number IN (3, 4)`, svc).Scan(&oldKept)
	if total != 202 || oldKept != 2 {
		t.Errorf("%d rows left, %d of the old protected ones", total, oldKept)
	}
	var minKept int
	st.R.QueryRow(`SELECT min(number) FROM deploys WHERE service_id = ? AND number > 4`, svc).Scan(&minKept)
	if minKept != 51 {
		t.Errorf("the newest 200 start at #%d, want #51", minKept)
	}
	var stages int
	st.R.QueryRow(`SELECT count(*) FROM deploy_stages`).Scan(&stages)
	if stages != 0 {
		t.Errorf("%d stage rows left for pruned deploys (cascade)", stages)
	}
	var otherLeft int
	st.R.QueryRow(`SELECT count(*) FROM deploys WHERE service_id = ?`, other).Scan(&otherLeft)
	if otherLeft != 5 {
		t.Errorf("another service lost rows: %d", otherLeft)
	}
	if n2, err := PruneDeploys(ctx, st, svc); err != nil || n2 != 0 {
		t.Errorf("second prune: %d %v", n2, err)
	}
}
