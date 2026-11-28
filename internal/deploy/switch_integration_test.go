//go:build integration

package deploy

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/hub"
	"github.com/Docker-Hunterpedia/serverdock/internal/pki"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
)

// switchRig is the real chain of TestIntegrationTwentyReloadsUnderLoadCauseNoNon2xx
// (real Docker, real agent ops over a hub session, the real embedded Caddy,
// Routes), for tests that switch routes.
type switchRig struct {
	ctx       context.Context
	sess      *hub.Session // the local agent's session (a Caller), for a client that counts ops
	cl        *agentops.Client
	applier   *proxy.Applier
	panelPort int
	netName   string
	subnet    string
	inst      string
}

// startApp runs a container of the test image on the app network at ip whose
// /cgi-bin/id answers 200 "app-<name>" to GET and to POST (the image's plain
// httpd answers a POST with 501), through the docker CLI: the chain under test
// is the route path, not container creation. The script reads the request body
// before it answers, like the test image's own script (see its Dockerfile).
//
// extra are further docker run arguments (the 8b tests pass the labels that make the
// fixture a container of this install, so a deploy finds and stops it).
func (g *switchRig) startApp(t *testing.T, ref, name, ip string, extra ...string) {
	t.Helper()
	script := `mkdir -p /www/cgi-bin && printf '#!/bin/sh\nhead -c "${CONTENT_LENGTH:-0}" >/dev/null\necho "Content-type: text/plain"\necho\necho app-` + name + `\n' > /www/cgi-bin/id && chmod +x /www/cgi-bin/id && exec httpd -f -p 8080 -h /www`
	cname := "sd-it-" + g.inst[:8] + "-" + strings.ToLower(name)
	args := []string{"run", "-d", "--name", cname, "--network", g.netName, "--ip", ip, "--label", "serverdock.instance=" + g.inst}
	args = append(args, extra...)
	args = append(args, "--entrypoint", "sh", ref, "-c", script)
	itDocker(t, args...)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", cname).Run() })
	var probe agentops.HealthResult
	var err error
	for i := 0; i < 30 && !probe.OK; i++ {
		if probe, err = g.cl.HealthCheck(g.ctx, agentops.HealthArgs{Target: ip + ":8080", Kind: agentops.KindHTTP, Path: "/cgi-bin/id", TimeoutMS: 1000}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !probe.OK {
		if os.Getenv("CI") != "" {
			t.Fatalf("the app %s is not reachable from the host in CI: %+v", name, probe)
		}
		t.Skipf("the host cannot reach the bridge network (Docker Desktop?): %+v", probe)
	}
}

func newSwitchRig(t *testing.T) (*switchRig, string) {
	t.Helper()
	ref := os.Getenv("SD_TEST_APP_IMAGE")
	if ref == "" {
		t.Skip("SD_TEST_APP_IMAGE is not set")
	}
	sock := os.Getenv("DOCKER_SOCK")
	if sock == "" {
		sock = dockerapi.DefaultSocket
	}
	docker := dockerapi.New(sock)
	t.Cleanup(docker.Close)
	rb := make([]byte, 16)
	_, _ = rand.Read(rb)
	inst := hex.EncodeToString(rb)
	g := &switchRig{inst: inst, netName: agentops.NetworkName(inst), subnet: itSubnet(t)}
	t.Cleanup(func() {
		if ids, err := exec.Command("docker", "ps", "-aq", "--filter", "label=serverdock.instance="+inst).Output(); err == nil {
			for _, id := range strings.Fields(string(ids)) {
				_ = exec.Command("docker", "rm", "-f", "-v", id).Run()
			}
		}
		_ = exec.Command("docker", "network", "rm", g.netName).Run()
	})

	dir := t.TempDir()
	leaf, err := pki.EnsurePanelLeaf(pki.PanelOptions{Dir: filepath.Join(dir, "pki")})
	if err != nil {
		t.Fatal(err)
	}
	panelUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		io.WriteString(w, "panel")
	}))
	t.Cleanup(panelUp.Close)
	g.panelPort = itFreePort(t)
	g.applier = proxy.NewApplier(proxy.Config{
		DataDir: dir, Listen: "127.0.0.1:" + strconv.Itoa(itFreePort(t)), Upstream: strings.TrimPrefix(panelUp.URL, "http://"),
		Panel: &proxy.Panel{Listen: "127.0.0.1:" + strconv.Itoa(g.panelPort), CertFile: leaf.CertFile},
	})
	if err := g.applier.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.applier.Stop() })

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ops, err := agentops.Handlers(agentops.Deps{Docker: docker, Instance: inst, CheckCreate: policy.CheckCreateAgent,
		AllowLoopbackRegistry: true, Proxy: g.applier, Log: quiet,
		AppSubnet: func(context.Context) (string, error) { return g.subnet, nil }})
	if err != nil {
		t.Fatal(err)
	}
	actx, acancel := context.WithCancel(context.Background())
	h := hub.New(actx, "", quiet)
	pa, pb := net.Pipe()
	var wg sync.WaitGroup
	wg.Go(func() { _ = h.Attach(actx, hub.LocalID, pa, 0) })
	wg.Go(func() {
		_ = agent.Serve(actx, pb, agent.Config{Logs: docker, Version: "t", Mode: agent.ModeLocal, Ops: ops}, 0, quiet)
	})
	t.Cleanup(func() { acancel(); wg.Wait() })
	var sess *hub.Session
	for i := 0; i < 500 && sess == nil; i++ {
		if s, ok := h.Get(hub.LocalID); ok {
			sess = s
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sess == nil {
		t.Fatal("agent did not attach")
	}
	select {
	case <-sess.HelloDone():
	case <-time.After(10 * time.Second):
		t.Fatal("no hello")
	}
	g.sess = sess
	g.cl = agentops.NewClient(sess)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	g.ctx = ctx
	if _, err := g.cl.NetworkEnsure(ctx, g.netName, g.subnet); err != nil {
		t.Fatal(err)
	}
	if _, err := g.cl.ImagePull(ctx, ref, 0, nil); err != nil {
		t.Fatal(err)
	}
	return g, ref
}

// caddyLoaded reports whether any Caddy load succeeded since before was read: a
// successful load installs a new root context (caddy.ActiveContext) and keeps
// it until the next one, so a comparison of two readings is exact for "zero
// loads" and needs no goroutine.
func caddyLoaded(before caddy.Context) bool { return caddy.ActiveContext().Context != before.Context }

// TestIntegrationTwentySwitchesUnderLoadDropNothing: 20 route switches (upstream
// to upstream, maintenance and not-running flips) of one app, with constant
// POST load on that app, on a second app and on the panel (TLS), over
// keep-alive and new connections. No Caddy load happens, so nothing is dropped
// and nothing is retried; the switched app answers a 2xx from an app container
// or exactly one of the two 503 pages; the second app and the panel answer 2xx
// only.
func TestIntegrationTwentySwitchesUnderLoadDropNothing(t *testing.T) {
	g, ref := newSwitchRig(t)
	ctx := g.ctx
	subnet := netip.MustParsePrefix(g.subnet).Addr()
	ipN := func(n int) string { // .n of the subnet
		a := subnet.As4()
		a[3] = byte(n)
		return netip.AddrFrom4(a).String()
	}
	ipA1, ipA2, ipB := ipN(2), ipN(3), ipN(4)
	g.startApp(t, ref, "A1", ipA1)
	g.startApp(t, ref, "A2", ipA2)
	g.startApp(t, ref, "B", ipB)

	st := openStore(t)
	svcA, svcB := addService(t, st, "switched"), addService(t, st, "second")
	dA1 := addDeploy(t, st, svcA, 1, "succeeded", ipA1)
	dA2 := addDeploy(t, st, svcA, 2, "succeeded", ipA2)
	dB := addDeploy(t, st, svcB, 1, "succeeded", ipB)
	portA, portB := itFreePort(t), itFreePort(t)
	mustExec(t, st, `UPDATE services SET public_port = ?, container_port = 8080, active_deploy_id = ? WHERE id = ?`, portA, dA1, svcA)
	mustExec(t, st, `UPDATE services SET public_port = ?, container_port = 8080, active_deploy_id = ? WHERE id = ?`, portB, dB, svcB)
	r := NewRoutes(st, g.cl, nil)
	// The state map as a resync would fill it: these containers are running at their addresses.
	if err := r.ApplyRoutes(ctx, func() error {
		r.state.replace([]Container{
			{ID: "cA1", Service: svcA, Deploy: dA1, State: StateRunning, IP: ipA1},
			{ID: "cA2", Service: svcA, Deploy: dA2, State: StateRunning, IP: ipA2},
			{ID: "cB", Service: svcB, Deploy: dB, State: StateRunning, IP: ipB},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := g.applier.Applied(); len(got) != 2 {
		t.Fatalf("applied %v", got)
	}

	type class struct {
		name                                    string
		total, drops, retries, fail             atomic.Int64
		fromApp, maintenance, notRunning, secOK atomic.Int64
	}
	keep, fresh := &class{name: "keep-alive"}, &class{name: "new-connection"}
	var bad atomic.Int64
	var firstBad atomic.Value
	flag := func(msg string) {
		bad.Add(1)
		firstBad.CompareAndSwap(nil, msg)
	}
	switched := func(c *class, status int, hdr http.Header, body string) {
		switch {
		case status == 200 && strings.HasPrefix(body, "app-A"):
			c.fromApp.Add(1)
		case status == 503 && body == proxy.BodyMaintenance && hdr.Get("Retry-After") == "10" && hdr.Get("Cache-Control") == "no-store" && hdr.Get("Content-Type") == "text/plain; charset=utf-8":
			c.maintenance.Add(1)
		case status == 503 && body == proxy.BodyNotRunning && hdr.Get("Retry-After") == "" && hdr.Get("Cache-Control") == "no-store" && hdr.Get("Content-Type") == "text/plain; charset=utf-8":
			c.notRunning.Add(1)
		default:
			flag(fmt.Sprintf("switched app (%s): %d %q %v", c.name, status, body, hdr))
		}
	}
	other := func(c *class, status int, _ http.Header, body string, want string) {
		if status/100 == 2 && strings.HasPrefix(body, want) {
			c.secOK.Add(1)
		} else {
			flag(fmt.Sprintf("%s (%s): %d %q", want, c.name, status, body))
		}
	}
	hit := func(c *http.Client, url string, cl *class, check func(int, http.Header, string)) {
		cl.total.Add(1)
		var resp *http.Response
		var err error
		for attempt := 0; attempt < 2; attempt++ {
			var gotConns atomic.Int64
			req, _ := http.NewRequest("POST", url, strings.NewReader("payload=1"))
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
				GotConn: func(httptrace.GotConnInfo) { gotConns.Add(1) },
			}))
			resp, err = c.Do(req)
			if n := gotConns.Load(); n > 1 {
				cl.retries.Add(n - 1)
			}
			if err == nil {
				break
			}
			if attempt == 0 {
				cl.drops.Add(1)
				firstBad.CompareAndSwap(nil, fmt.Sprintf("%s (%s): %v", url, cl.name, err))
			}
		}
		if err != nil {
			cl.fail.Add(1)
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		check(resp.StatusCode, resp.Header, string(b))
	}
	stop := make(chan struct{})
	var load sync.WaitGroup
	type target struct {
		url   string
		check func(c *class) func(int, http.Header, string)
	}
	targets := []target{
		{"http://127.0.0.1:" + strconv.Itoa(portA) + "/cgi-bin/id", func(c *class) func(int, http.Header, string) {
			return func(s int, h http.Header, b string) { switched(c, s, h, b) }
		}},
		{"http://127.0.0.1:" + strconv.Itoa(portB) + "/cgi-bin/id", func(c *class) func(int, http.Header, string) {
			return func(s int, h http.Header, b string) { other(c, s, h, b, "app-B") }
		}},
		{"https://127.0.0.1:" + strconv.Itoa(g.panelPort) + "/", func(c *class) func(int, http.Header, string) {
			return func(s int, h http.Header, b string) { other(c, s, h, b, "panel") }
		}},
	}
	for _, tg := range targets {
		for _, c := range []*class{keep, fresh} {
			tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: c == fresh, MaxIdleConnsPerHost: 4}
			cli := &http.Client{Transport: tr, Timeout: 10 * time.Second}
			check := tg.check(c)
			load.Go(func() {
				defer tr.CloseIdleConnections()
				for {
					select {
					case <-stop:
						return
					default:
						hit(cli, tg.url, c, check)
						time.Sleep(2 * time.Millisecond)
					}
				}
			})
		}
	}
	time.Sleep(300 * time.Millisecond) // load is flowing before the first switch
	ctxBefore := caddy.ActiveContext()

	type step struct {
		name string
		do   func() error
	}
	toA1 := func() error {
		return r.SetOverride(ctx, svcA, Override{Kind: OverrideUpstream, Deploy: dA1, IP: ipA1, Port: 8080})
	}
	toA2 := func() error {
		return r.SetOverride(ctx, svcA, Override{Kind: OverrideUpstream, Deploy: dA2, IP: ipA2, Port: 8080})
	}
	toMaint := func() error { return r.SetOverride(ctx, svcA, Override{Kind: OverrideMaintenance}) }
	// A target no container is at: the route falls back to the not-running page.
	toNone := func() error {
		return r.SetOverride(ctx, svcA, Override{Kind: OverrideUpstream, Deploy: dA2 + 100, IP: ipN(9), Port: 8080})
	}
	seq := []step{{"A2", toA2}, {"maintenance", toMaint}, {"A1", toA1}, {"not running", toNone}, {"A2", toA2}, {"maintenance", toMaint},
		{"A2", toA2}, {"A1", toA1}, {"not running", toNone}, {"A1", toA1}, {"maintenance", toMaint}, {"A2", toA2}, {"not running", toNone},
		{"A2", toA2}, {"A1", toA1}, {"maintenance", toMaint}, {"A1", toA1}, {"A2", toA2}, {"not running", toNone}, {"A1", toA1}}
	if len(seq) != 20 {
		t.Fatalf("%d switches", len(seq))
	}
	appliedBefore := g.applier.Applied()
	for i, s := range seq {
		if err := s.do(); err != nil {
			t.Fatalf("switch %d (%s): %v", i, s.name, err)
		}
		time.Sleep(60 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	loaded := caddyLoaded(ctxBefore)
	close(stop)
	load.Wait()

	for _, c := range []*class{keep, fresh} {
		t.Logf("%s: %d requests; switched app: %d from an app container, %d maintenance 503, %d not-running 503; second app and panel 2xx: %d; dropped %d, Go-internal retries %d, failed after a client retry %d",
			c.name, c.total.Load(), c.fromApp.Load(), c.maintenance.Load(), c.notRunning.Load(), c.secOK.Load(), c.drops.Load(), c.retries.Load(), c.fail.Load())
		if c.total.Load() < 200 || c.fromApp.Load() == 0 || c.maintenance.Load() == 0 || c.notRunning.Load() == 0 {
			t.Errorf("%s: the load did not see every kind of response", c.name)
		}
		if c.drops.Load() != 0 || c.retries.Load() != 0 || c.fail.Load() != 0 {
			t.Errorf("%s: %d dropped, %d retries, %d failed (first: %v)", c.name, c.drops.Load(), c.retries.Load(), c.fail.Load(), firstBad.Load())
		}
	}
	t.Logf("non-conforming responses %d; a Caddy load happened during the switches: %v", bad.Load(), loaded)
	if bad.Load() != 0 {
		t.Errorf("%d unexpected responses (first: %v)", bad.Load(), firstBad.Load())
	}
	if loaded {
		t.Errorf("Caddy loaded a config during the 20 switches, want none")
	}
	if got := g.applier.Applied(); fmt.Sprint(got) != fmt.Sprint(appliedBefore) {
		t.Errorf("applied ports changed: %v -> %v", appliedBefore, got)
	}
}
