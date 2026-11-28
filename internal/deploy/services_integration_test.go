//go:build integration

package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/hub"
	"github.com/Docker-Hunterpedia/serverdock/internal/pki"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
)

// staticSubnet is the recorded app subnet of the test install.
type staticSubnet struct{ p netip.Prefix }

func (s staticSubnet) Subnet(context.Context) (netip.Prefix, error) { return s.p, nil }
func (s staticSubnet) Check(context.Context) error                  { return nil }

// TestIntegrationServiceActionsAgainstDockerAndCaddy runs step 7's actions over
// the real chain (real Docker, the real agent ops over a hub session, the real
// embedded Caddy): the boot order creates the app network (enable_icc off, the
// recorded subnet) and opens the route gate; a service with a public port is
// served (the "not running" page) once the action says it is applied; a port
// another program holds is reported, not claimed, and "Try again" opens it once
// that program lets go; and system.exposure lists a container ServerDock did not
// create that publishes a port.
func TestIntegrationServiceActionsAgainstDockerAndCaddy(t *testing.T) {
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
	expoName := "sd-it-expo-" + inst[:8]
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", "-v", expoName).Run()
		_ = exec.Command("docker", "network", "rm", netName).Run()
	})

	dir := t.TempDir()
	leaf, err := pki.EnsurePanelLeaf(pki.PanelOptions{Dir: filepath.Join(dir, "pki")})
	if err != nil {
		t.Fatal(err)
	}
	panelUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "panel") }))
	defer panelUp.Close()
	httpsPort := itFreePort(t)
	listenPort := itFreePort(t)
	applier := proxy.NewApplier(proxy.Config{
		DataDir: dir, Listen: "127.0.0.1:" + strconv.Itoa(listenPort), Upstream: strings.TrimPrefix(panelUp.URL, "http://"),
		Panel: &proxy.Panel{Listen: "127.0.0.1:" + strconv.Itoa(httpsPort), CertFile: leaf.CertFile},
	})
	if err := applier.Start(); err != nil { // the panel alone first
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = applier.Stop() })

	reserved := []int{httpsPort, listenPort}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ops, err := agentops.Handlers(agentops.Deps{Docker: docker, Instance: inst, CheckCreate: policy.CheckCreateAgent,
		AllowLoopbackRegistry: true, Proxy: applier, ReservedPorts: reserved, Log: quiet,
		AppSubnet: func(context.Context) (string, error) { return subnet, nil }})
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	st := openStore(t)
	routes := NewRoutes(st, cl, nil)
	booter := NewBooter(BootConfig{Instance: inst, AppNet: staticSubnet{netip.MustParsePrefix(subnet)}, Net: cl, Routes: routes, Sync: syncVia(routes, cl), SyncRoutes: true})
	al, err := audit.New(st, quiet)
	if err != nil {
		t.Fatal(err)
	}
	reg := actions.New(al, quiet)
	loopback := netip.MustParseAddr("127.0.0.1")
	RegisterServiceActions(reg, ServiceDeps{DB: st, Routes: routes, Boot: booter.Run, Policy: policy.Options{AllowLoopbackRegistry: true},
		ReservedPorts: reserved, Exposure: cl, PanelAddrs: func() []netip.Addr { return []netip.Addr{loopback} }, Log: quiet})
	admin := actions.UserActor("1", actions.ScopeAdmin, time.Now().Add(time.Hour))
	call := func(name string, in any) (any, error) {
		raw, _ := json.Marshal(in)
		res, err := reg.Invoke(ctx, admin, name, raw, actions.Options{})
		return res.Out, err
	}

	// Boot order: network_ensure, then Sync (the gate opens).
	if routes.Ready() {
		t.Fatal("ready before boot")
	}
	if err := booter.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !routes.Ready() {
		t.Fatal("the gate is closed after boot")
	}
	raw := itDocker(t, "network", "inspect", "-f", "{{range .IPAM.Config}}{{.Subnet}}|{{.Gateway}}|{{.IPRange}}{{end}}|{{index .Options \"com.docker.network.bridge.enable_icc\"}}|{{index .Labels \"serverdock.instance\"}}", netName)
	gw := netip.MustParsePrefix(subnet).Addr().Next().String()
	if want := fmt.Sprintf("%s|%s|%s|false|%s", subnet, gw, dynamicRangeOf(t, subnet), inst); raw != want {
		t.Errorf("network: %s, want %s", raw, want)
	}

	// A service with a public port: applied, and served (nothing runs yet, so the "not running" page).
	port := itFreePort(t)
	out, err := call("services.create", m{"project": "it", "name": "web", "image_ref": ref, "container_port": 8080, "public_port": port})
	if err != nil {
		t.Fatal(err)
	}
	ch := out.(ServiceChangeOut)
	if !ch.Applied || len(ch.Service.Warnings) != 1 || ch.Service.Warnings[0].Code != WarnSharesPanelAddress {
		t.Fatalf("%+v", ch)
	}
	if code := getStatus(t, port); code != http.StatusServiceUnavailable {
		t.Errorf("GET :%d = %d, want the 503 not-running page", port, code)
	}

	// A port another program holds: reported, not claimed.
	held, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	heldPort := held.Addr().(*net.TCPAddr).Port
	out, err = call("services.set_public_port", m{"service": "it/web", "public_port": heldPort})
	if err != nil {
		held.Close()
		t.Fatal(err)
	}
	ch = out.(ServiceChangeOut)
	if ch.Applied || !strings.Contains(ch.Notice, "could not be opened") || ch.Service.RouteError != BindMessage(heldPort) {
		held.Close()
		t.Fatalf("a port held by another program was reported as applied: %+v", ch)
	}
	got, _ := call("services.get", m{"service": "it/web"})
	if got.(ServiceOut).RouteError != BindMessage(heldPort) {
		t.Errorf("get: %+v", got)
	}
	// Still held: Try again says so. The program lets go: Try again opens it.
	out, _ = call("services.retry_port", m{"service": "it/web"})
	if out.(ServiceChangeOut).Applied {
		t.Error("retry_port claimed success while the port was still held")
	}
	held.Close()
	out, err = call("services.retry_port", m{"service": "it/web"})
	if err != nil {
		t.Fatal(err)
	}
	if ch = out.(ServiceChangeOut); !ch.Applied || ch.Service.RouteError != "" {
		t.Fatalf("after the port was freed: %+v", ch)
	}
	if code := getStatus(t, heldPort); code != http.StatusServiceUnavailable {
		t.Errorf("GET :%d = %d after Try again", heldPort, code)
	}
	if applied := applier.Applied(); len(applied) != 1 || applied[0] != heldPort {
		t.Errorf("the proxy serves %v, want only %d (the old port is gone)", applied, heldPort)
	}

	// system.exposure: a container ServerDock did not create that publishes a port.
	expoPort := itFreePort(t)
	itDocker(t, "run", "-d", "--name", expoName, "-p", fmt.Sprintf("127.0.0.1:%d:8080", expoPort), ref)
	ex, err := call("system.exposure", m{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range ex.(ExposureOut).Containers {
		if c.Name == expoName && len(c.Ports) == 1 && c.Ports[0].HostPort == expoPort {
			found = true
		}
	}
	if !found {
		t.Errorf("exposure %+v does not list %s on %d", ex, expoName, expoPort)
	}
}

func dynamicRangeOf(t *testing.T, subnet string) string {
	t.Helper()
	r, err := dockerapi.DynamicRange(subnet)
	if err != nil {
		t.Fatal(err)
	}
	return r.String()
}

func getStatus(t *testing.T, port int) int {
	t.Helper()
	var last error
	for i := 0; i < 20; i++ {
		resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
		if err == nil {
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			return resp.StatusCode
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("GET :%d: %v", port, last)
	return 0
}
