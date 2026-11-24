package agentops

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
)

type fakeApplier struct {
	mu     sync.Mutex
	got    [][]proxy.AppRoute
	retry  []bool
	failed []int
	err    error
}

func (f *fakeApplier) Apply(_ context.Context, routes []proxy.AppRoute, retryFailed bool) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, routes)
	f.retry = append(f.retry, retryFailed)
	return f.failed, f.err
}

func proxyRig(t *testing.T, f *fakeApplier, reserved ...int) *rig {
	return newRig(t, func(d *Deps) {
		if f != nil {
			d.Proxy = f
		}
		d.ReservedPorts = reserved
	})
}

func TestProxyApplyOpSpec(t *testing.T) {
	sp, ok := agent.SpecFor(agent.OpProxyApply)
	if !ok || sp.RemoteOK || sp.Timeout != 20*time.Second || sp.Timeout <= proxy.GracePeriod {
		t.Fatalf("%+v %v", sp, ok)
	}
}

func TestProxyApplyConvertsRoutesAndReportsFailedPorts(t *testing.T) {
	f := &fakeApplier{failed: []int{9003, 9001}}
	rg := proxyRig(t, f)
	res, err := rg.cl.ProxyApply(ctx5(t), []Route{
		{Port: 9000, Upstream: "10.250.7.2:8080"},
		{Port: 9001, Page: PageNotRunning},
		{Port: 9002, Page: PageMaintenance},
		{Port: 9003, Upstream: "10.250.7.9:80"},
	}, false)
	if err != nil || !res.Applied || !reflect.DeepEqual(res.FailedPorts, []int{9001, 9003}) {
		t.Fatalf("%+v %v", res, err)
	}
	want := []proxy.AppRoute{
		{Port: 9000, Upstream: "10.250.7.2:8080"},
		{Port: 9001, Static: proxy.NotRunning()},
		{Port: 9002, Static: proxy.Maintenance()},
		{Port: 9003, Upstream: "10.250.7.9:80"},
	}
	if len(f.got) != 1 || !reflect.DeepEqual(f.got[0], want) {
		t.Fatalf("%+v", f.got)
	}
	// An empty list is valid (no exposed service) and a nil failed list is [].
	f.failed = nil
	if res, err := rg.cl.ProxyApply(ctx5(t), nil, false); err != nil || res.FailedPorts == nil || len(res.FailedPorts) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestProxyApplyRefusesBadRoutes(t *testing.T) {
	f := &fakeApplier{}
	rg := proxyRig(t, f, 7443)
	many := make([]Route, MaxRoutes+1)
	for i := range many {
		many[i] = Route{Port: 20000 + i, Page: PageNotRunning}
	}
	for name, routes := range map[string][]Route{
		"port 80":            {{Port: 80, Page: PageNotRunning}},
		"port 443":           {{Port: 443, Page: PageNotRunning}},
		"below 1024":         {{Port: 1023, Page: PageNotRunning}},
		"above 65535":        {{Port: 65536, Page: PageNotRunning}},
		"the agent listener": {{Port: 7443, Page: PageNotRunning}},
		"neither":            {{Port: 9000}},
		"both":               {{Port: 9000, Upstream: "10.0.0.2:80", Page: PageNotRunning}},
		"an unknown page":    {{Port: 9000, Page: "custom"}},
		"a host name":        {{Port: 9000, Upstream: "app.internal:80"}},
		"ipv6":               {{Port: 9000, Upstream: "[fd00::2]:80"}},
		"a unix socket":      {{Port: 9000, Upstream: "unix//run/x.sock"}},
		"a zero port":        {{Port: 9000, Upstream: "10.0.0.2:0"}},
		"a padded port":      {{Port: 9000, Upstream: "10.0.0.2:0080"}},
		"no port":            {{Port: 9000, Upstream: "10.0.0.2"}},
		"duplicate ports":    {{Port: 9000, Page: PageNotRunning}, {Port: 9000, Page: PageNotRunning}},
		"more than the cap":  many,
	} {
		_, err := rg.cl.ProxyApply(ctx5(t), routes, false)
		if !IsCode(err, agent.CodeInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(f.got) != 0 {
		t.Fatalf("%d bad lists reached the proxy", len(f.got))
	}
	// A host address of the static range passes.
	for _, up := range []string{"10.250.7.2:80", "10.250.7.100:8080", "10.250.7.191:80"} {
		if _, err := rg.cl.ProxyApply(ctx5(t), []Route{{Port: 9000, Upstream: up}}, false); err != nil {
			t.Errorf("%s: %v", up, err)
		}
	}
}

func TestProxyApplyRefusesUpstreamOutsideTheStaticRange(t *testing.T) {
	f := &fakeApplier{}
	rg := proxyRig(t, f)
	for name, up := range map[string]string{
		"the gateway":              "10.250.7.1:80",
		"the network address":      "10.250.7.0:80",
		"the broadcast address":    "10.250.7.255:80",
		"the dynamic range":        "10.250.7.200:80",
		"another RFC 1918 address": "10.0.0.2:80",
		"another 172.16 address":   "172.16.5.2:80",
		"a CGNAT address outside":  "100.64.0.2:80",
		"loopback":                 "127.0.0.1:7070",
		"unspecified":              "0.0.0.0:80",
		"link-local (metadata)":    "169.254.169.254:80",
		"a public address":         "8.8.8.8:80",
	} {
		_, err := rg.cl.ProxyApply(ctx5(t), []Route{{Port: 9001, Page: PageNotRunning}, {Port: 9000, Upstream: up}}, false)
		if !IsCode(err, agent.CodePolicy) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(f.got) != 0 {
		t.Fatalf("%d lists reached the proxy", len(f.got))
	}
	// Static pages alone need no recorded subnet at all.
	rg = newRig(t, func(d *Deps) { d.Proxy = f; d.AppSubnet = func(context.Context) (string, error) { return "", nil } })
	if _, err := rg.cl.ProxyApply(ctx5(t), []Route{{Port: 9001, Page: PageNotRunning}}, false); err != nil {
		t.Fatalf("static pages before the subnet is recorded: %v", err)
	}
	if _, err := rg.cl.ProxyApply(ctx5(t), []Route{{Port: 9000, Upstream: "10.250.7.2:80"}}, false); !IsCode(err, agent.CodeNotFound) {
		t.Fatalf("an upstream without a recorded subnet: %v", err)
	}
}

// proxy_apply never calls Docker: with network_inspect failing a valid switch
// applies and an address outside the recorded static range is refused.
func TestProxyApplyNeedsNoDocker(t *testing.T) {
	f := &fakeApplier{}
	rg := proxyRig(t, f)
	rg.fd.mu.Lock()
	rg.fd.netFails = true
	rg.fd.mu.Unlock()
	before := rg.fd.netInspects()
	if _, err := rg.cl.ProxyApply(ctx5(t), []Route{{Port: 9000, Upstream: "10.250.7.2:8080"}}, false); err != nil {
		t.Fatalf("a valid switch with Docker failing: %v", err)
	}
	if _, err := rg.cl.ProxyApply(ctx5(t), []Route{{Port: 9000, Page: PageMaintenance}}, false); err != nil {
		t.Fatal(err)
	}
	for _, up := range []string{"10.250.7.1:80", "10.250.7.200:80", "10.250.8.2:80", "127.0.0.1:80"} {
		if _, err := rg.cl.ProxyApply(ctx5(t), []Route{{Port: 9000, Upstream: up}}, false); !IsCode(err, agent.CodePolicy) {
			t.Errorf("%s: %v", up, err)
		}
	}
	if len(f.got) != 2 {
		t.Fatalf("%d lists reached the proxy, want 2", len(f.got))
	}
	if rg.fd.netInspects() != before {
		t.Fatalf("proxy_apply called network inspect %d times", rg.fd.netInspects()-before)
	}
}

func TestProxyApplyFailureModes(t *testing.T) {
	routes := []Route{{Port: 9000, Page: PageNotRunning}}
	// No proxy (--proxy off).
	rg := proxyRig(t, nil)
	if _, err := rg.cl.ProxyApply(ctx5(t), routes, false); !IsCode(err, agent.CodeConflict) {
		t.Fatalf("no proxy: %v", err)
	}
	// Caddy's message (it can name paths) stays in the agent: the reply is fixed.
	f := &fakeApplier{err: errors.New("load: open /var/lib/serverdock/caddy/secret-thing: permission denied")}
	rg = proxyRig(t, f)
	_, err := rg.cl.ProxyApply(ctx5(t), routes, false)
	if !IsCode(err, agent.CodeConflict) || strings.Contains(err.Error(), "secret-thing") || strings.Contains(err.Error(), "/var/lib") {
		t.Fatalf("%v", err)
	}
	f.err = proxy.ErrNotStarted
	if _, err := rg.cl.ProxyApply(ctx5(t), routes, false); !IsCode(err, agent.CodeConflict) {
		t.Fatalf("not started: %v", err)
	}
	f.err = context.DeadlineExceeded
	if _, err := rg.cl.ProxyApply(ctx5(t), routes, false); !IsCode(err, agent.CodeTimeout) {
		t.Fatalf("deadline: %v", err)
	}
	// A hostile or buggy agent naming a port we never sent, or one twice: the
	// control plane refuses the reply.
	f.err, f.failed = nil, []int{9999}
	if _, err := rg.cl.ProxyApply(ctx5(t), routes, false); !errors.Is(err, ErrBadReply) {
		t.Fatalf("unknown failed port: %v", err)
	}
	f.failed = []int{9000, 9000}
	if _, err := rg.cl.ProxyApply(ctx5(t), routes, false); !errors.Is(err, ErrBadReply) {
		t.Fatalf("duplicate failed port: %v", err)
	}
}

// retry_failed is additive: absent means false, and it reaches the applier.
func TestProxyApplyPassesRetryFailed(t *testing.T) {
	f := &fakeApplier{}
	rg := proxyRig(t, f)
	routes := []Route{{Port: 9000, Page: PageNotRunning}}
	if _, err := rg.cl.ProxyApply(ctx5(t), routes, false); err != nil {
		t.Fatal(err)
	}
	if _, err := rg.cl.ProxyApply(ctx5(t), routes, true); err != nil {
		t.Fatal(err)
	}
	// A request from a control plane that never heard of the field.
	if _, err := rg.rawCall(agent.OpProxyApply, map[string]any{"routes": []Route{{Port: 9000, Page: PageNotRunning}}}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.retry, []bool{false, true, false}) {
		t.Fatalf("retry_failed seen by the applier: %v", f.retry)
	}
}
