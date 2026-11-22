package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/hub"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// fakeEngine is a minimal dockerd on a unix socket: /_ping, an empty container
// list and a fixed network list.
type fakeEngine struct {
	mu       sync.Mutex
	networks string // JSON for GET /networks
	paths    []string
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
		case r.Method == "GET" && path == "containers/json":
			io.WriteString(w, `[]`)
		case r.Method == "GET" && path == "networks":
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
