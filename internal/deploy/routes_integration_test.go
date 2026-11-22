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

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/hub"
	"github.com/Docker-Hunterpedia/serverdock/internal/pki"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
)

// These need a running Docker, the app image in a registry on loopback (the CI
// "integration" job provides both) and, for the app route, a host that can
// reach the bridge network (Linux). Without SD_TEST_APP_IMAGE they skip.

func itDocker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// itSubnet returns a /24 in 10.250.0.0/16 that no Docker network uses.
func itSubnet(t *testing.T) string {
	t.Helper()
	used := itDocker(t, "network", "ls", "-q")
	var inspected string
	if used != "" {
		inspected = itDocker(t, append([]string{"network", "inspect", "-f", "{{range .IPAM.Config}}{{.Subnet}} {{end}}"}, strings.Fields(used)...)...)
	}
	for i := 0; i < 50; i++ {
		b := make([]byte, 1)
		_, _ = rand.Read(b)
		cand := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 250, 100 + b[0]%100, 0}), 24).String()
		if !strings.Contains(inspected, cand) {
			return cand
		}
	}
	t.Skip("no free test subnet")
	return ""
}

func itFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestIntegrationTwentyReloadsUnderLoadCauseNoNon2xx runs the real chain: the
// real Docker, the real agent ops over the hub session, the real embedded
// Caddy, Routes.ApplyRoutes. Twenty config-changing reloads happen while the
// panel and an app take constant load over keep-alive and new connections;
// every response must be 2xx.
func TestIntegrationTwentyReloadsUnderLoadCauseNoNon2xx(t *testing.T) {
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
	netName := agentops.NetworkName(inst)
	subnet := itSubnet(t)
	t.Cleanup(func() {
		if ids, err := exec.Command("docker", "ps", "-aq", "--filter", "label=serverdock.instance="+inst).Output(); err == nil {
			for _, id := range strings.Fields(string(ids)) {
				_ = exec.Command("docker", "rm", "-f", "-v", id).Run()
			}
		}
		_ = exec.Command("docker", "network", "rm", netName).Run()
	})

	// The panel: a real leaf, an upstream that always answers 200.
	dir := t.TempDir()
	leaf, err := pki.EnsurePanelLeaf(pki.PanelOptions{Dir: filepath.Join(dir, "pki")})
	if err != nil {
		t.Fatal(err)
	}
	panelUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "panel") }))
	defer panelUp.Close()
	panelPort := itFreePort(t)
	applier := proxy.NewApplier(proxy.Config{
		DataDir: dir, Listen: "127.0.0.1:" + strconv.Itoa(itFreePort(t)), Upstream: strings.TrimPrefix(panelUp.URL, "http://"),
		Panel: &proxy.Panel{Listen: "127.0.0.1:" + strconv.Itoa(panelPort), CertFile: leaf.CertFile},
	})
	if err := applier.Start(); err != nil { // the panel alone first, as the server does
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = applier.Stop() })

	// The agent (real handlers, real Docker, the real applier) behind a hub session.
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ops, err := agentops.Handlers(agentops.Deps{Docker: docker, Instance: inst, CheckCreate: policy.CheckCreateAgent,
		AllowLoopbackRegistry: true, Proxy: applier, Log: quiet})
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
	cl := agentops.NewClient(sess)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// An app container with a static address on the app network.
	if _, err := cl.NetworkEnsure(ctx, netName, subnet); err != nil {
		t.Fatal(err)
	}
	pulled, err := cl.ImagePull(ctx, ref, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ip := netip.MustParsePrefix(subnet).Addr().Next().Next().String() // .2
	st := openStore(t)
	svc := addService(t, st, "web")
	dep := addDeploy(t, st, svc, 1, "succeeded", ip)
	args, err := BuildCreate(CreateInput{ProjectName: "it" + inst[:8], ServiceName: "web", ProjectID: 1, ServiceID: svc, DeployID: dep, Attempt: 1,
		ImageID: pulled.ImageID, IP: ip, ContainerPort: 8080, Network: netName, Subnet: subnet})
	if err != nil {
		t.Fatal(err)
	}
	created, err := cl.ContainerCreate(ctx, args.Name, args.Spec, args.IP, args.ImageID)
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.ContainerStart(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	var probe agentops.HealthResult
	for i := 0; i < 20 && !probe.OK; i++ {
		if probe, err = cl.HealthCheck(ctx, agentops.HealthArgs{Target: ip + ":8080", Kind: agentops.KindHTTP, Path: "/", TimeoutMS: 1000}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !probe.OK {
		if os.Getenv("CI") != "" {
			t.Fatalf("the app is not reachable from the host in CI: %+v", probe)
		}
		t.Skipf("the host cannot reach the bridge network (Docker Desktop?): %+v", probe)
	}

	// Routes: the service exposed on appPort, plus a spare service whose port is
	// toggled to force a real config change (a reload) each time.
	appPort := itFreePort(t)
	mustExec(t, st, `UPDATE services SET public_port = ?, container_port = 8080, active_deploy_id = ? WHERE id = ?`, appPort, dep, svc)
	spare := addService(t, st, "spare")
	sparePort := itFreePort(t)
	r := NewRoutes(st, cl, nil)
	if px := r.Ready(); px {
		t.Fatal("ready before the first list and inspect")
	}
	if err := r.Sync(ctx, cl); err != nil {
		t.Fatal(err)
	}
	if got := applier.Applied(); len(got) != 1 || got[0] != appPort {
		t.Fatalf("applied %v", got)
	}

	// Load: keep-alive and new-connection clients on the panel and on the app.
	//
	// Hard assertions: no non-2xx status, and no request that still fails after
	// one client retry. A request whose connection is closed with no response
	// (never read, so never processed, so safe to retry) can happen at the
	// instant of a reload, for kept-alive and new connections alike: Go's
	// net/http closes a connection the retiring server accepted but whose request
	// arrives after its Shutdown began. That is stock Caddy behaviour (plan 0004
	// Revision 9). Such drops are counted per connection class and bounded at 5 %
	// of that class's requests. Go's own transparent retries of reused
	// connections are counted too (httptrace GotConn attempts), so "kept-alive"
	// is not claimed to mean "never dropped".
	type class struct {
		name                          string
		total, drops, autoRetry, fail atomic.Int64
	}
	keep, fresh := &class{name: "keep-alive"}, &class{name: "new-connection"}
	var badStatus atomic.Int64
	var firstBad atomic.Value
	stop := make(chan struct{})
	var load sync.WaitGroup
	hit := func(c *http.Client, url string, cl *class) {
		cl.total.Add(1)
		var err error
		var resp *http.Response
		for attempt := 0; attempt < 2; attempt++ {
			var gotConns atomic.Int64
			req, _ := http.NewRequest("GET", url, nil)
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
				GotConn: func(httptrace.GotConnInfo) { gotConns.Add(1) },
			}))
			resp, err = c.Do(req)
			if n := gotConns.Load(); n > 1 {
				cl.autoRetry.Add(n - 1)
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
			cl.fail.Add(1) // failed even on the retry
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			badStatus.Add(1)
			firstBad.CompareAndSwap(nil, url+": "+resp.Status)
		}
	}
	for _, target := range []string{"https://127.0.0.1:" + strconv.Itoa(panelPort) + "/", "http://127.0.0.1:" + strconv.Itoa(appPort) + "/"} {
		for _, cl := range []*class{keep, fresh} {
			tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: cl == fresh, MaxIdleConnsPerHost: 4}
			c := &http.Client{Transport: tr, Timeout: 10 * time.Second}
			load.Go(func() {
				defer tr.CloseIdleConnections()
				for {
					select {
					case <-stop:
						return
					default:
						hit(c, target, cl)
						time.Sleep(2 * time.Millisecond)
					}
				}
			})
		}
	}
	time.Sleep(300 * time.Millisecond) // load is flowing before the first reload

	for i := 0; i < 20; i++ {
		var port any
		if i%2 == 0 {
			port = sparePort
		}
		before := len(applier.Applied())
		err := r.ApplyRoutes(ctx, func() error {
			mustExec(t, st, `UPDATE services SET public_port = ? WHERE id = ?`, port, spare)
			return nil
		})
		if err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
		if after := len(applier.Applied()); (i%2 == 0 && after != before+1) || (i%2 == 1 && after != before-1) {
			t.Fatalf("reload %d did not change the config: %d -> %d routes", i, before, after)
		}
		time.Sleep(60 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	load.Wait()

	for _, cl := range []*class{keep, fresh} {
		t.Logf("%s: %d requests, %d dropped (no response, retried by the client), %d Go-internal retries, %d failed after the client retry",
			cl.name, cl.total.Load(), cl.drops.Load(), cl.autoRetry.Load(), cl.fail.Load())
		if cl.total.Load() < 100 {
			t.Fatalf("only %d %s requests were made; the load did not run", cl.total.Load(), cl.name)
		}
		if cl.fail.Load() != 0 {
			t.Errorf("%s: %d requests failed even after one retry (first: %v)", cl.name, cl.fail.Load(), firstBad.Load())
		}
		if cl.drops.Load()*20 > cl.total.Load() {
			t.Errorf("%s: %d of %d requests were dropped (over 5 %%)", cl.name, cl.drops.Load(), cl.total.Load())
		}
	}
	if badStatus.Load() != 0 {
		t.Fatalf("%d non-2xx responses across the reloads (first: %v)", badStatus.Load(), firstBad.Load())
	}
	if t.Failed() {
		t.FailNow()
	}

	// The app stopping flips the route to the page (event-driven), and the spare
	// port, never deployed, answers the not-running page.
	if err := cl.ContainerStop(ctx, created.ID, 1); err != nil {
		t.Fatal(err)
	}
	insp, err := cl.ContainerInspect(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := ContainerFromInspect(insp)
	if !ok || c.State == StateRunning {
		t.Fatalf("%+v %v", c, ok)
	}
	if err := r.SetContainer(ctx, c); err != nil {
		t.Fatal(err)
	}
	var code int
	for i := 0; i < 20; i++ {
		resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(appPort) + "/")
		if err == nil {
			code = resp.StatusCode
			resp.Body.Close()
			if code == 503 {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code != 503 {
		t.Fatalf("a stopped app answers %d, want the 503 page", code)
	}
}
