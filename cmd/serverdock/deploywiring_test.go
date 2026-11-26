package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/deploy"
	"github.com/Docker-Hunterpedia/serverdock/internal/hub"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// fakeEngine is a minimal dockerd on a unix socket: /_ping, an empty container
// list and a fixed network list.
type fakeEngine struct {
	mu       sync.Mutex
	networks string // JSON for GET /networks
	paths    []string
	// created holds the networks made through POST /networks/create, by name,
	// as the inspect JSON GET /networks/<name> then returns.
	created map[string]string
	// listFails makes GET /containers/json answer 500 (Docker "down" for Sync).
	listFails atomic.Bool
	// netsFail makes GET /networks answer 500 (the subnet cannot be chosen yet).
	netsFail atomic.Bool
}

func startFakeEngine(t *testing.T, networks string) (string, *fakeEngine) {
	t.Helper()
	fe := &fakeEngine{networks: networks}
	dir, err := os.MkdirTemp("", "sdw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			io.WriteString(w, "OK")
			return
		}
		_, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v"), "/")
		fe.mu.Lock()
		fe.paths = append(fe.paths, r.Method+" /"+path)
		nets := fe.networks
		fe.mu.Unlock()
		switch {
		case r.Method == "GET" && path == "events":
			// The reconciler's stream (step 10): open, no events, until the client leaves.
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		case r.Method == "GET" && path == "containers/json":
			if fe.listFails.Load() {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			io.WriteString(w, `[]`)
		case r.Method == "POST" && path == "networks/create":
			var c struct {
				Name   string
				IPAM   struct{ Config []map[string]string }
				Labels map[string]string
				Option map[string]string `json:"Options"`
			}
			if err := json.NewDecoder(r.Body).Decode(&c); err != nil || len(c.IPAM.Config) != 1 {
				http.Error(w, "bad create", http.StatusBadRequest)
				return
			}
			lb, _ := json.Marshal(c.Labels)
			ob, _ := json.Marshal(c.Option)
			cfg, _ := json.Marshal(c.IPAM.Config)
			fe.mu.Lock()
			if fe.created == nil {
				fe.created = map[string]string{}
			}
			fe.created[c.Name] = `{"Id":"` + strings.Repeat("a", 64) + `","Name":"` + c.Name + `","Driver":"bridge","Options":` + string(ob) + `,"Labels":` + string(lb) + `,"IPAM":{"Config":` + string(cfg) + `},"Containers":{}}`
			fe.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"Id":"`+strings.Repeat("a", 64)+`"}`)
		case r.Method == "GET" && strings.HasPrefix(path, "networks/"):
			fe.mu.Lock()
			body, ok := fe.created[strings.TrimPrefix(path, "networks/")]
			fe.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, body)
		case r.Method == "GET" && path == "networks":
			if fe.netsFail.Load() {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			io.WriteString(w, nets)
		case r.Method == "GET" && path == "version":
			io.WriteString(w, `{"Version":"25.0.3","ApiVersion":"1.45"}`)
		default:
			http.NotFound(w, r)
		}
	})
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h}}
	srv.Start()
	t.Cleanup(srv.Close)
	return sock, fe
}

func readMeta(t *testing.T, data, key string) (string, bool) {
	t.Helper()
	ro, err := store.OpenReadOnly(data, store.ReadOnlyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var v string
	err = ro.DB.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	return v, err == nil
}

// The server picks the app subnet at first start (clear of Docker's networks),
// records it in meta, and a later start with another flag keeps it.
func TestServerRecordsTheAppSubnetAtFirstStartOnly(t *testing.T) {
	sock, _ := startFakeEngine(t, `[{"Name":"bridge","IPAM":{"Config":[{"Subnet":"172.17.0.0/16"}]}},{"Name":"other","IPAM":{"Config":[{"Subnet":"10.250.0.0/24"}]}}]`)
	data := newData(t)
	_, stop := startServer(t, serverConfig{Data: data, ControlSocket: shortSock(t), DockerSocket: sock})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if v, ok := readMeta(t, data, "app_subnet"); !ok || v != "10.250.1.0/24" {
		t.Fatalf("meta.app_subnet = %q (%v), want 10.250.1.0/24", v, ok)
	}
	id, ok := readMeta(t, data, "install_id")
	if !ok || len(id) != 32 {
		t.Fatalf("meta.install_id %q", id)
	}
	// Second start with a different flag: the recorded subnet stays, the install id too.
	_, stop = startServer(t, serverConfig{Data: data, ControlSocket: shortSock(t), DockerSocket: sock, AppSubnet: "10.99.0.0/24"})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if v, _ := readMeta(t, data, "app_subnet"); v != "10.250.1.0/24" {
		t.Errorf("app_subnet changed to %q on a later start", v)
	}
	if again, _ := readMeta(t, data, "install_id"); again != id {
		t.Errorf("install_id changed: %q -> %q", id, again)
	}
}

func TestServerAppSubnetFlagAtFirstStart(t *testing.T) {
	sock, fe := startFakeEngine(t, `[]`)
	data := newData(t)
	_, stop := startServer(t, serverConfig{Data: data, ControlSocket: shortSock(t), DockerSocket: sock, AppSubnet: "10.77.0.0/22"})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if v, _ := readMeta(t, data, "app_subnet"); v != "10.77.0.0/22" {
		t.Errorf("meta.app_subnet = %q", v)
	}
	_ = fe
}

// An --app-subnet that overlaps a Docker network refuses to start and saves nothing.
func TestServerRefusesAnOverlappingAppSubnetFlag(t *testing.T) {
	sock, _ := startFakeEngine(t, `[{"Name":"vpn","IPAM":{"Config":[{"Subnet":"10.77.0.0/24"}]}}]`)
	data := newData(t)
	err := runServer(context.Background(), serverConfig{Data: data, ControlSocket: shortSock(t), DockerSocket: sock, AppSubnet: "10.77.0.0/22"}, quietLog(), nil)
	if err == nil || !strings.Contains(err.Error(), "vpn") || !strings.Contains(err.Error(), "--app-subnet") {
		t.Fatalf("runServer: %v", err)
	}
	if v, ok := readMeta(t, data, "app_subnet"); ok {
		t.Errorf("a refused subnet %q was saved", v)
	}
}

// Without Docker the server still starts: the subnet is chosen later.
func TestServerStartsWithoutDockerAndRecordsNoSubnet(t *testing.T) {
	data := newData(t)
	_, stop := startServer(t, serverConfig{Data: data, ControlSocket: shortSock(t), DockerSocket: filepath.Join(t.TempDir(), "none.sock")})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if v, ok := readMeta(t, data, "app_subnet"); ok {
		t.Errorf("a subnet %q was recorded without Docker", v)
	}
}

func TestAppSubnetFlagIsValidatedBeforeAnythingStarts(t *testing.T) {
	var buf bytes.Buffer
	for _, bad := range []string{"10.0.0.0/25", "10.0.0.0/19", "10.0.0.5/24", "fd00::/64", "nonsense", "8.8.8.0/24"} {
		if _, err := parseServerFlags([]string{"--app-subnet", bad}, &buf); err == nil || !strings.Contains(err.Error(), "--app-subnet") {
			t.Errorf("%q: %v", bad, err)
		}
		data := newData(t)
		err := runServer(context.Background(), serverConfig{Data: data, ControlSocket: shortSock(t), AppSubnet: bad}, quietLog(), nil)
		if err == nil || !strings.Contains(err.Error(), "--app-subnet") {
			t.Errorf("runServer %q: %v", bad, err)
		}
		if ents, _ := os.ReadDir(data); len(ents) != 0 {
			t.Errorf("%q: runServer touched the data directory before validating the flag", bad)
		}
	}
	c, err := parseServerFlags([]string{"--app-subnet", "10.250.16.0/20"}, &buf)
	if err != nil || c.AppSubnet != "10.250.16.0/20" {
		t.Errorf("%+v %v", c, err)
	}
}

// The local agent serves the step-3 ops; a remote agent refuses every one of them.
func TestLocalAgentServesTheStep3OpsAndARemoteAgentRefusesThem(t *testing.T) {
	sock, _ := startFakeEngine(t, `[]`)
	addrs, stop := startServer(t, serverConfig{Data: newData(t), ControlSocket: shortSock(t), DockerSocket: sock,
		AgentListen: "127.0.0.1:0", AgentSANs: []string{"127.0.0.1"}, AgentToken: testToken})
	defer stop()

	local := waitSession(t, addrs.hub, hub.LocalID)
	res, err := agentops.NewClient(local).ContainerList(context.Background())
	if err != nil || len(res.Containers) != 0 {
		t.Fatalf("local container_list: %+v %v", res, err)
	}
	// The agent-side half (a remote agent process serves no local-only op even if
	// handed the handlers) is tested in internal/agent: TestOpsGivenToARemoteAgentAreNotServed
	// and TestRemoteAgentProcessServesOnlyRemoteOKOps. Here the hub's refusal.
	// A refused body proves the handler itself ran: create without a valid name
	// is answered by the handler, not by "unknown op".
	_, err = local.Call(context.Background(), agent.OpContainerStart, map[string]string{"id": "short"}, nil)
	var oe *agent.OpError
	if !errors.As(err, &oe) || oe.Code != agent.CodeInvalid {
		t.Fatalf("local container_start with a short id: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = agent.RunClient(ctx, agent.ClientConfig{URL: "wss://" + addrs.Agent + "/agent/connect", CAFingerprint: addrs.CAFingerprint,
			Token: testToken, ID: "remote1", Logs: noLogs{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	}()
	defer func() { cancel(); <-done }()
	remote := waitSession(t, addrs.hub, "remote1")
	for _, sp := range agent.Specs() {
		if sp.RemoteOK || sp.Name == agent.OpLogs {
			continue
		}
		if _, err := remote.Call(context.Background(), sp.Name, struct{}{}, nil); !errors.Is(err, hub.ErrLocalOnly) && !errors.Is(err, hub.ErrUnknownOp) {
			t.Errorf("remote %s: %v, want it refused", sp.Name, err)
		}
	}
	if _, err := agentops.NewClient(remote).ContainerList(context.Background()); err == nil {
		t.Error("a remote agent listed containers")
	}
}

func waitSession(t *testing.T, h *hub.Hub, id string) *hub.Session {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s, ok := h.Get(id); ok {
			select {
			case <-s.HelloDone():
				return s
			case <-time.After(50 * time.Millisecond):
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no session %q", id)
	return nil
}

// --allow-loopback-registry is off by default and reaches the policy options;
// the loopback rule itself is tested in internal/policy.
func TestAllowLoopbackRegistryFlag(t *testing.T) {
	var buf strings.Builder
	c, err := parseServerFlags(nil, &buf)
	if err != nil || c.AllowLoopbackRegistry {
		t.Fatalf("default: %+v %v", c, err)
	}
	if _, err := policy.ValidateImageRef("127.0.0.1:5000/app:1", c.policyOptions()); err == nil {
		t.Error("a loopback registry was accepted by default")
	}
	c, err = parseServerFlags([]string{"--allow-loopback-registry"}, &buf)
	if err != nil || !c.AllowLoopbackRegistry {
		t.Fatalf("flag: %+v %v", c, err)
	}
	if _, err := policy.ValidateImageRef("127.0.0.1:5000/app:1", c.policyOptions()); err != nil {
		t.Errorf("a loopback registry was refused with the flag: %v", err)
	}
	if _, err := policy.ValidateImageRef("169.254.169.254/app", c.policyOptions()); err == nil {
		t.Error("a metadata registry was accepted with the flag")
	}
}

// ---- step 7: the boot order, the reserved ports and the registry ----

func (fe *fakeEngine) sawPaths() []string {
	fe.mu.Lock()
	defer fe.mu.Unlock()
	return append([]string(nil), fe.paths...)
}

func indexOf(list []string, want string) int {
	for i, p := range list {
		if p == want {
			return i
		}
	}
	return -1
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Boot order (plan 0004 step 7, Revision 9): the panel alone first (the server is
// ready and serving), then network_ensure with the recorded subnet (the network
// is created with the app subnet and inter-container traffic off), and the first
// resync (the reconciler's, step 10: the event stream first, then the list), which
// opens the route gate.
func TestServerBootCreatesTheAppNetworkThenOpensTheRouteGate(t *testing.T) {
	sock, fe := startFakeEngine(t, `[]`)
	data := newData(t)
	addrs, stop := startServer(t, serverConfig{Data: data, ControlSocket: shortSock(t), DockerSocket: sock, Proxy: true,
		Listen: testbinFree(t), HTTPSListen: testbinFree(t), AppSubnet: "10.77.0.0/24"})
	defer func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	}()
	waitFor(t, "the route gate", func() bool { return addrs.routes.Ready() })
	id, _ := readMeta(t, data, "install_id")
	name := agentops.NetworkName(id)
	paths := fe.sawPaths()
	// Since step 10 the reconciler owns the first list and the Booter owns network_ensure;
	// neither waits for the other (a deploy ensures the network itself before it
	// allocates an address), so the order of the two is not fixed: both must happen.
	create, list, events := indexOf(paths, "POST /networks/create"), indexOf(paths, "GET /containers/json"), indexOf(paths, "GET /events")
	if create < 0 || list < 0 || events < 0 || events > list {
		t.Fatalf("network_ensure (%d), the first container list (%d) and the event stream (%d, which must be open before the list): %v", create, list, events, paths)
	}
	fe.mu.Lock()
	body := fe.created[name]
	fe.mu.Unlock()
	if !strings.Contains(body, `"Subnet":"10.77.0.0/24"`) || !strings.Contains(body, `"com.docker.network.bridge.enable_icc":"false"`) ||
		!strings.Contains(body, `"serverdock.instance":"`+id+`"`) {
		t.Errorf("the network was created as %s", body)
	}
	if st := addrs.booter.NetworkStatus(); st.State != deploy.NetOK {
		t.Errorf("network status %+v", st)
	}
}

// With Docker failing at start the server still runs (the panel alone) and the
// gate stays closed; a service action then opens it lazily once Docker answers,
// and reports the port as applied only after that.
func TestServerKeepsTheGateClosedUntilDockerAnswers(t *testing.T) {
	sock, fe := startFakeEngine(t, `[]`)
	fe.listFails.Store(true)
	addrs, stop := startServer(t, serverConfig{Data: newData(t), ControlSocket: shortSock(t), Proxy: true,
		Listen: testbinFree(t), HTTPSListen: testbinFree(t), DockerSocket: sock})
	defer stop()
	waitFor(t, "the boot attempt", func() bool { return indexOf(fe.sawPaths(), "GET /containers/json") >= 0 })
	if addrs.routes.Ready() {
		t.Fatal("the gate opened although the container list failed")
	}
	admin := actions.UserActor("1", actions.ScopeAdmin, time.Now().Add(time.Hour))
	_, port, _ := net.SplitHostPort(testbinFree(t))
	in := json.RawMessage(`{"project":"p","name":"web","image_ref":"nginx","container_port":80,"public_port":` + port + `}`)
	res, err := addrs.reg.Invoke(context.Background(), admin, "services.create", in, actions.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out := res.Out.(deploy.ServiceChangeOut); out.Applied || out.Service.RoutesReady || !strings.Contains(out.Notice, "not applied yet") {
		t.Fatalf("reported as applied with the gate closed: %+v", out)
	}
	// Docker answers again: "Try again" (retry_port) opens the gate and applies the port.
	fe.listFails.Store(false)
	res, err = addrs.reg.Invoke(context.Background(), admin, "services.retry_port", json.RawMessage(`{"service":"p/web"}`), actions.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out := res.Out.(deploy.ServiceChangeOut); !out.Applied || !out.Service.RoutesReady || !addrs.routes.Ready() {
		t.Fatalf("%+v", out)
	}
}

// One list of ServerDock's own ports feeds both the agent's proxy_apply and the
// service actions: --agent-listen, --listen and --https-listen (80 and 443 are
// refused by policy itself).
func TestReservedPortsAreOneListForTheAgentAndTheServiceActions(t *testing.T) {
	c := serverConfig{AgentListen: "127.0.0.1:7443", Listen: ":8080", HTTPSListen: "0.0.0.0:8443"}
	if got := c.reservedPorts(); len(got) != 3 || got[0] != 7443 || got[1] != 8080 || got[2] != 8443 {
		t.Errorf("%v", got)
	}
	if got := (serverConfig{Listen: "bad", HTTPSListen: ":0", AgentListen: ""}).reservedPorts(); len(got) != 0 {
		t.Errorf("%v", got)
	}

	// A named port is resolved, not dropped; one that does not resolve is refused
	// at validation, so none can be left out of the list silently.
	if got := (serverConfig{Listen: ":http", HTTPSListen: "0.0.0.0:https"}).reservedPorts(); len(got) != 2 || got[0] != 80 || got[1] != 443 {
		t.Errorf("named ports: %v", got)
	}
	for _, c := range []serverConfig{{Listen: ":nosuchservice"}, {HTTPSListen: "0.0.0.0:nosuchservice"}, {AgentListen: "127.0.0.1:nosuchservice", AgentSANs: []string{"x"}}, {Listen: "noport"}} {
		if err := c.validate(); err == nil || !strings.Contains(err.Error(), "--") {
			t.Errorf("%+v: %v", c, err)
		}
	}

	sock, _ := startFakeEngine(t, `[]`)
	agentAddr, listen, https := testbinFree(t), testbinFree(t), testbinFree(t)
	addrs, stop := startServer(t, serverConfig{Data: newData(t), ControlSocket: shortSock(t), DockerSocket: sock, Proxy: true,
		Listen: listen, HTTPSListen: https, AgentListen: agentAddr, AgentSANs: []string{"127.0.0.1"}, AgentToken: testToken})
	defer stop()
	local := waitSession(t, addrs.hub, hub.LocalID)
	admin := actions.UserActor("1", actions.ScopeAdmin, time.Now().Add(time.Hour))
	for what, addr := range map[string]string{"agent-listen": agentAddr, "listen": listen, "https-listen": https} {
		_, p, _ := net.SplitHostPort(addr)
		port, _ := strconv.Atoi(p)
		// The service action refuses it ...
		in := `{"project":"p","name":"` + strings.ReplaceAll(what, "-", "") + `","image_ref":"nginx","container_port":80,"public_port":` + p + `}`
		if _, err := addrs.reg.Invoke(context.Background(), admin, "services.create", json.RawMessage(in), actions.Options{}); !errors.Is(err, actions.ErrInvalidInput) {
			t.Errorf("%s (%d): services.create: %v", what, port, err)
		}
		// ... and so does the agent's proxy_apply, from the same list.
		_, err := local.Call(context.Background(), agent.OpProxyApply, agentops.ApplyArgs{Routes: []agentops.Route{{Port: port, Page: agentops.PageNotRunning}}}, nil)
		var oe *agent.OpError
		if !errors.As(err, &oe) || oe.Code != agent.CodeInvalid {
			t.Errorf("%s (%d): proxy_apply: %v", what, port, err)
		}
	}
}

// The server's registry carries the step 7 actions (and the sentinel suite, run
// unchanged, covers the rest of the server).
func TestServerRegistryHasTheServiceActions(t *testing.T) {
	sock, _ := startFakeEngine(t, `[]`)
	addrs, stop := startServer(t, serverConfig{Data: newData(t), ControlSocket: shortSock(t), DockerSocket: sock})
	defer stop()
	for _, n := range []string{"projects.list", "services.list", "services.get", "services.create", "services.update", "services.set_source",
		"services.set_public_port", "services.retry_port", "system.exposure", "services.delete", "services.restart", "services.redeploy", "services.rollback"} {
		if _, ok := addrs.reg.Lookup(n); !ok {
			t.Errorf("%s is not registered", n)
		}
	}
	if info, _ := addrs.reg.Lookup("services.delete"); info.Risk != actions.RiskDestructive {
		t.Errorf("services.delete: %+v", info)
	}
	// This server runs with --proxy=false: a public port is refused, clearly.
	admin := actions.UserActor("1", actions.ScopeAdmin, time.Now().Add(time.Hour))
	in := json.RawMessage(`{"project":"p","name":"web","image_ref":"nginx","container_port":80,"public_port":9000}`)
	if _, err := addrs.reg.Invoke(context.Background(), admin, "services.create", in, actions.Options{}); !errors.Is(err, actions.ErrConflict) || !strings.Contains(err.Error(), "embedded proxy is off") {
		t.Errorf("public port with the proxy off: %v", err)
	}
}

func TestPanelAddrsReturnsTheBoundAddressOrEveryInterface(t *testing.T) {
	if got := panelAddrs("203.0.113.5:443")(); len(got) != 1 || got[0].String() != "203.0.113.5" {
		t.Errorf("%v", got)
	}
	if got := panelAddrs(":443")(); len(got) == 0 {
		t.Error("no interface addresses for a panel on every interface")
	}
	lo := false
	for _, a := range panelAddrs("0.0.0.0:443")() {
		lo = lo || a.IsLoopback()
	}
	if !lo {
		t.Error("loopback is missing: an SSH tunnel reaches the panel there")
	}
}

// recordingApplier is the proxy_apply side of TestSubnetRecordedAfterBoot.
type recordingApplier struct {
	mu  sync.Mutex
	got [][]proxy.AppRoute
}

func (r *recordingApplier) Apply(_ context.Context, routes []proxy.AppRoute, _ bool) ([]int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, routes)
	return nil, nil
}

// meta.app_subnet is recorded lazily, after the agent starts: before it exists an
// upstream route is refused and static pages apply; after deploy.AppNet records
// it, with no restart, the same upstream applies (the getter reads on each call).
func TestSubnetRecordedAfterBoot(t *testing.T) {
	sock, fe := startFakeEngine(t, `[]`)
	fe.netsFail.Store(true) // Docker "down" for the subnet choice
	ctx := context.Background()
	st, err := store.Open(ctx, newData(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	docker := dockerapi.New(sock)
	defer docker.Close()
	instance := strings.Repeat("ab", 16)
	an, err := recordAppSubnet(ctx, st, docker, instance, "", quietLog())
	if err != nil {
		t.Fatal(err)
	}
	app := &recordingApplier{}
	ops, err := agentops.Handlers(agentops.Deps{Docker: docker, Instance: instance, CheckCreate: policy.CheckCreateAgent,
		Proxy: app, AppSubnet: an.Recorded, Log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	apply := func(routes ...agentops.Route) error {
		raw, _ := json.Marshal(agentops.ApplyArgs{Routes: routes})
		_, err := ops[agent.OpProxyApply](ctx, raw, nil)
		return err
	}
	up := agentops.Route{Port: 9000, Upstream: "10.250.0.2:8080"} // the first choice with no Docker networks
	page := agentops.Route{Port: 9001, Page: agentops.PageMaintenance}
	if err := apply(up); !agentops.IsCode(err, agent.CodeNotFound) {
		t.Fatalf("an upstream before the subnet is recorded: %v", err)
	}
	if err := apply(page); err != nil {
		t.Fatalf("a static page before the subnet is recorded: %v", err)
	}
	if len(app.got) != 1 {
		t.Fatalf("%d applies reached the proxy, want only the static page", len(app.got))
	}
	// Docker answers again; the first Subnet call records it. No restart.
	fe.netsFail.Store(false)
	if _, err := an.Subnet(ctx); err != nil {
		t.Fatal(err)
	}
	if err := apply(up, page); err != nil {
		t.Fatalf("the same upstream after the subnet is recorded: %v", err)
	}
	if len(app.got) != 2 {
		t.Fatalf("%d applies reached the proxy", len(app.got))
	}
	// And the recorded range, not Docker, bounds it.
	if v, _ := an.Recorded(ctx); v != "10.250.0.0/24" {
		t.Fatalf("recorded %q", v)
	}
	if err := apply(agentops.Route{Port: 9000, Upstream: "10.250.0.1:80"}); !agentops.IsCode(err, agent.CodePolicy) {
		t.Fatalf("the gateway: %v", err)
	}
}

// A damaged or edited meta.app_subnet row never widens what an upstream may be:
// Recorded applies the --app-subnet rule, and proxy_apply then refuses every
// upstream ("could not be read") while static pages still apply.
func TestProxyApplyRefusesAMalformedOrOutOfPolicyRecordedSubnet(t *testing.T) {
	sock, _ := startFakeEngine(t, `[]`)
	ctx := context.Background()
	st, err := store.Open(ctx, newData(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	docker := dockerapi.New(sock)
	defer docker.Close()
	instance := strings.Repeat("cd", 16)
	an, err := deploy.NewAppNet(deploy.AppNetOptions{Store: st, Instance: instance, Docker: docker, Log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	app := &recordingApplier{}
	ops, err := agentops.Handlers(agentops.Deps{Docker: docker, Instance: instance, CheckCreate: policy.CheckCreateAgent,
		Proxy: app, AppSubnet: an.Recorded, Log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	apply := func(routes ...agentops.Route) error {
		raw, _ := json.Marshal(agentops.ApplyArgs{Routes: routes})
		_, err := ops[agent.OpProxyApply](ctx, raw, nil)
		return err
	}
	for _, bad := range []struct{ row, up string }{
		{"127.0.0.0/24", "127.0.0.2:80"}, {"172.17.0.0/16", "172.17.0.2:80"}, {"8.8.8.0/24", "8.8.8.8:80"},
		{"10.250.0.0/25", "10.250.0.2:80"}, {"10.250.0.5/24", "10.250.0.9:80"}, {"garbage", "10.250.0.2:80"}, {" 10.250.0.0/24", "10.250.0.2:80"},
	} {
		if _, err := st.W.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES ('app_subnet', ?)`, bad.row); err != nil {
			t.Fatal(err)
		}
		if err := apply(agentops.Route{Port: 9000, Upstream: bad.up}); !agentops.IsCode(err, agent.CodeConflict) {
			t.Errorf("row %q: %v, want the conflict refusal", bad.row, err)
		}
		if err := apply(agentops.Route{Port: 9001, Page: agentops.PageMaintenance}); err != nil {
			t.Errorf("row %q: a static page: %v", bad.row, err)
		}
	}
	if n := len(app.got); n != 7 { // only the static pages reached the proxy
		t.Fatalf("%d applies reached the proxy, want 7 (the static pages)", n)
	}
	if _, err := st.W.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES ('app_subnet', '10.250.0.0/24')`); err != nil {
		t.Fatal(err)
	}
	if err := apply(agentops.Route{Port: 9000, Upstream: "10.250.0.2:80"}); err != nil {
		t.Fatalf("a valid row: %v", err)
	}
}
