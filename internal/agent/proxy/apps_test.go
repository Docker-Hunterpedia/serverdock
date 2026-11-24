package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/pki"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// panelConfig is a server-mode config with a real panel leaf, listening on free
// ports.
func panelConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	leaf, err := pki.EnsurePanelLeaf(pki.PanelOptions{Dir: filepath.Join(dir, "pki")})
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(up.Close)
	return Config{
		DataDir: dir, Listen: freeAddr(t), Upstream: hostPort(up),
		Panel: &Panel{Listen: freeAddr(t), CertFile: leaf.CertFile},
	}
}

func servers(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)
}

func TestPanelConfigUnchangedWithApps(t *testing.T) {
	base := Config{DataDir: "/data", Listen: ":80", Upstream: UnixUpstream("/data/run/cp.sock"),
		Panel: &Panel{Listen: ":443", CertFile: "/data/pki/panel.pem", Hosts: []string{"panel.example"}}}
	without, err := BuildConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	// An empty Apps is byte-identical to none.
	base.Apps = []AppRoute{}
	if empty, _ := BuildConfig(base); !bytes.Equal(empty, without) {
		t.Fatal("an empty Apps changed the config")
	}
	base.Apps = []AppRoute{
		{Port: 8081, Upstream: "10.200.0.2:8080"},
		{Port: 8080, Static: NotRunning()},
	}
	with, err := BuildConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]any
	_ = json.Unmarshal(without, &a)
	_ = json.Unmarshal(with, &b)
	sa, sb := servers(t, without), servers(t, with)
	for _, k := range []string{"main", "panel"} {
		if !reflect.DeepEqual(sa[k], sb[k]) {
			t.Errorf("server %q changed when apps were added", k)
		}
	}
	if len(sb) != 3 || sb["apps"] == nil || !reflect.DeepEqual(sb["apps"].(map[string]any)["listen"], []any{":8080", ":8081"}) {
		t.Errorf("servers: %v", sb)
	}
	// Everything outside the servers (TLS policy, storage, logging, admin) too.
	delete(a["apps"].(map[string]any)["http"].(map[string]any), "servers")
	delete(b["apps"].(map[string]any)["http"].(map[string]any), "servers")
	if !reflect.DeepEqual(a, b) {
		t.Error("the config outside the servers changed when apps were added")
	}
	// The remote agent's config has no panel and never takes apps.
	if _, err := BuildConfig(Config{DataDir: "/d", Listen: ":80", Upstream: "127.0.0.1:1", Apps: base.Apps}); err == nil {
		t.Error("app routes were accepted without a panel")
	}
}

func TestAppRouteGolden(t *testing.T) {
	b, err := BuildConfig(Config{DataDir: "/data", Listen: ":80", Upstream: UnixUpstream("/data/run/cp.sock"),
		Panel: &Panel{Listen: ":443", CertFile: "/data/pki/panel.pem"},
		Apps: []AppRoute{
			{Port: 9002, Static: Maintenance()},
			{Port: 9000, Upstream: "10.200.0.2:8080"},
			{Port: 9001, Static: NotRunning()},
		}})
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := json.Indent(&got, b, "", "  "); err != nil {
		t.Fatal(err)
	}
	got.WriteByte('\n')
	const path = "testdata/app_routes.golden.json"
	if *update {
		if err := os.WriteFile(path, got.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Fatalf("app route config changed (go test -update to accept):\n%s", got.String())
	}
	// The properties the golden is there for.
	s := servers(t, b)
	if len(s) != 3 {
		t.Errorf("want the panel's two servers and ONE app server, got %d", len(s))
	}
	for _, k := range []string{"apps"} {
		srv := s[k].(map[string]any)
		if l := srv["listen"].([]any); !reflect.DeepEqual(l, []any{":9000", ":9001", ":9002"}) {
			t.Errorf("%s listen %v", k, l)
		}
		// Exactly one reverse_proxy, behind the serverdock_app handler, dialing the
		// per-request variable, with no health checks, limits or retries.
		h := srv["routes"].([]any)[0].(map[string]any)["handle"].([]any)
		if len(h) != 2 || h[0].(map[string]any)["handler"] != "serverdock_app" || h[1].(map[string]any)["handler"] != "reverse_proxy" {
			t.Errorf("%s handlers %v", k, h)
		}
		rp := h[1].(map[string]any)
		for _, bad := range []string{"health_checks", "load_balancing", "dynamic_upstreams", "circuit_breaker"} {
			if _, ok := rp[bad]; ok {
				t.Errorf("reverse_proxy has %s", bad)
			}
		}
		if up := rp["upstreams"].([]any); len(up) != 1 || !reflect.DeepEqual(up[0], map[string]any{"dial": "{http.vars.sd_upstream}"}) {
			t.Errorf("upstreams %v", up)
		}
		if p := srv["protocols"].([]any); len(p) != 1 || p[0] != "h1" {
			t.Errorf("%s protocols %v", k, p)
		}
		if srv["automatic_https"].(map[string]any)["disable"] != true || srv["read_header_timeout"] != "10s" ||
			srv["idle_timeout"] != "2m" || srv["max_header_bytes"] != float64(64<<10) {
			t.Errorf("%s limits %v", k, srv)
		}
		if _, ok := srv["tls_connection_policies"]; ok {
			t.Errorf("%s is not plain HTTP", k)
		}
	}
}

func TestAppRouteValidation(t *testing.T) {
	good := Config{DataDir: "/d", Listen: ":80", Upstream: "127.0.0.1:1", Panel: &Panel{Listen: ":443", CertFile: "/x"}}
	for name, apps := range map[string][]AppRoute{
		"port too low":     {{Port: 1023, Static: NotRunning()}},
		"port too high":    {{Port: 65536, Static: NotRunning()}},
		"the http port":    {{Port: 80, Static: NotRunning()}},
		"the https port":   {{Port: 443, Static: NotRunning()}},
		"duplicate":        {{Port: 9000, Static: NotRunning()}, {Port: 9000, Static: NotRunning()}},
		"neither":          {{Port: 9000}},
		"both":             {{Port: 9000, Upstream: "10.0.0.2:80", Static: NotRunning()}},
		"a name":           {{Port: 9000, Upstream: "app:80"}},
		"a unix socket":    {{Port: 9000, Upstream: "unix//run/x.sock"}},
		"ipv6":             {{Port: 9000, Upstream: "[fd00::2]:80"}},
		"no port":          {{Port: 9000, Upstream: "10.0.0.2"}},
		"port zero":        {{Port: 9000, Upstream: "10.0.0.2:0"}},
		"leading zero":     {{Port: 9000, Upstream: "10.0.0.2:080"}},
		"padded address":   {{Port: 9000, Upstream: "010.0.0.2:80"}},
		"a placeholder":    {{Port: 9000, Static: &Static{Status: 503, Body: "{http.request.header.Cookie}"}}},
		"a 200 page":       {{Port: 9000, Static: &Static{Status: 200}}},
		"a huge retry":     {{Port: 9000, Static: &Static{Status: 503, RetryAfter: 99999}}},
		"too many (101)":   manyRoutes(101),
		"a huge static":    {{Port: 9000, Static: &Static{Status: 503, Body: strings.Repeat("x", 2000)}}},
		"negative retry":   {{Port: 9000, Static: &Static{Status: 503, RetryAfter: -1}}},
		"a status of 1000": {{Port: 9000, Static: &Static{Status: 1000}}},
	} {
		c := good
		c.Apps = apps
		if _, err := BuildConfig(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := good
	c.Apps = manyRoutes(100)
	if _, err := BuildConfig(c); err != nil {
		t.Errorf("100 routes: %v", err)
	}
}

func manyRoutes(n int) []AppRoute {
	out := make([]AppRoute, n)
	for i := range out {
		out[i] = AppRoute{Port: 20000 + i, Static: NotRunning()}
	}
	return out
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	var last error
	for i := 0; i < 50; i++ {
		resp, err := http.Get(url)
		if err == nil {
			defer resp.Body.Close()
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(resp.Body)
			return resp, buf.String()
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("GET %s: %v", url, last)
	return nil, ""
}

func startApplier(t *testing.T) *Applier {
	t.Helper()
	ap := NewApplier(panelConfig(t))
	if err := ap.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Stop() })
	return ap
}

func TestStaticNotRunningAndMaintenancePages(t *testing.T) {
	ap := startApplier(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "from the app") }))
	defer up.Close()
	pNot, pMaint, pUp := freePort(t), freePort(t), freePort(t)
	failed, err := ap.Apply(context.Background(), []AppRoute{
		{Port: pNot, Static: NotRunning()},
		{Port: pMaint, Static: Maintenance()},
		{Port: pUp, Upstream: hostPort(up)},
	}, false)
	if err != nil || len(failed) != 0 {
		t.Fatalf("%v %v", failed, err)
	}
	url := func(p int) string { return "http://127.0.0.1:" + strconv.Itoa(p) + "/anything" }

	resp, body := get(t, url(pNot))
	if resp.StatusCode != 503 || !strings.Contains(body, "This app is not running (ServerDock)") || resp.Header.Get("Retry-After") != "" {
		t.Errorf("not running: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	resp, body = get(t, url(pMaint))
	if resp.StatusCode != 503 || !strings.Contains(body, "Deploying, back in a moment") || resp.Header.Get("Retry-After") != "10" {
		t.Errorf("maintenance: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp, body = get(t, url(pUp)); resp.StatusCode != 200 || body != "from the app" {
		t.Errorf("upstream: %d %q", resp.StatusCode, body)
	}
	// The panel keeps working beside the apps.
	cfg := ap.base
	if conn, err := net.DialTimeout("tcp", cfg.Panel.Listen, time.Second); err != nil {
		t.Errorf("panel listener: %v", err)
	} else {
		conn.Close()
	}
}

// Caddy starts one upstream-health ticker per reverse_proxy handler (plan 0004,
// the rule 3 exception); a static route has no such handler.
func tickers() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") { // one block per goroutine
		if strings.Contains(g, "metricsUpstreamsHealthyUpdater).init.func1") {
			n++
		}
	}
	return n
}

func waitTickers(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := tickers()
		if n == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d upstream-health tickers, want %d", n, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---- Applier logic over a fake load ----

type fakeLoad struct {
	mu      sync.Mutex
	loads   []Config
	failOn  map[int]bool // loading any config that has these ports fails
	failAll error
}

func (f *fakeLoad) load(c Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads = append(f.loads, c)
	if f.failAll != nil {
		return f.failAll
	}
	for _, a := range c.Apps {
		if f.failOn[a.Port] {
			return errors.New("listen tcp :" + strconv.Itoa(a.Port) + ": bind: address already in use")
		}
	}
	return nil
}

func fakeApplier(f *fakeLoad) *Applier {
	ap := NewApplier(Config{DataDir: "/d", Listen: ":80", Upstream: "127.0.0.1:1", Panel: &Panel{Listen: ":443", CertFile: "/x"}})
	ap.load, ap.stop = f.load, func() error { return nil }
	return ap
}

func routes(ports ...int) []AppRoute {
	var out []AppRoute
	for _, p := range ports {
		out = append(out, AppRoute{Port: p, Static: NotRunning()})
	}
	return out
}

func TestBootLoadsPanelBeforeApps(t *testing.T) {
	f := &fakeLoad{}
	ap := fakeApplier(f)
	// Before Start nothing can be applied, and nothing was loaded.
	if _, err := ap.Apply(context.Background(), routes(9000), false); !errors.Is(err, ErrNotStarted) || len(f.loads) != 0 {
		t.Fatalf("%v, %d loads", err, len(f.loads))
	}
	if err := ap.Start(); err != nil {
		t.Fatal(err)
	}
	if len(f.loads) != 1 || len(f.loads[0].Apps) != 0 || f.loads[0].Panel == nil {
		t.Fatalf("the first load is not the panel alone: %+v", f.loads)
	}
	b, _ := BuildConfig(f.loads[0])
	if strings.Contains(string(b), `"app-`) {
		t.Fatal("an app server in the boot config")
	}
	if _, err := ap.Apply(context.Background(), routes(9000), false); err != nil || len(f.loads) != 2 || len(f.loads[1].Apps) != 1 {
		t.Fatalf("%v %d", err, len(f.loads))
	}
	// A base that carried Apps does not leak them into the first load.
	f2 := &fakeLoad{}
	base := Config{DataDir: "/d", Listen: ":80", Upstream: "127.0.0.1:1", Panel: &Panel{Listen: ":443", CertFile: "/x"}, Apps: routes(9100)}
	ap2 := NewApplier(base)
	ap2.load = f2.load
	if err := ap2.Start(); err != nil || len(f2.loads[0].Apps) != 0 {
		t.Fatalf("%v %+v", err, f2.loads)
	}
}

func TestApplierIsolatesFailuresInTheLogic(t *testing.T) {
	ctx := context.Background()
	// One new port fails: the others are applied, the failure is named.
	f := &fakeLoad{failOn: map[int]bool{9001: true}}
	ap := fakeApplier(f)
	_ = ap.Start()
	failed, err := ap.Apply(ctx, routes(9000, 9001, 9002), false)
	if err != nil || !reflect.DeepEqual(failed, []int{9001}) || !reflect.DeepEqual(ap.Applied(), []int{9000, 9002}) {
		t.Fatalf("%v %v %v", failed, err, ap.Applied())
	}
	// Two fail: the good one still gets in.
	f = &fakeLoad{failOn: map[int]bool{9000: true, 9002: true}}
	ap = fakeApplier(f)
	_ = ap.Start()
	failed, err = ap.Apply(ctx, routes(9000, 9001, 9002), false)
	if err != nil || !reflect.DeepEqual(failed, []int{9000, 9002}) || !reflect.DeepEqual(ap.Applied(), []int{9001}) {
		t.Fatalf("%v %v %v", failed, err, ap.Applied())
	}
	// A failure with no new port (a removal) is an error, and what ran stays recorded.
	f = &fakeLoad{}
	ap = fakeApplier(f)
	_ = ap.Start()
	_, _ = ap.Apply(ctx, routes(9000, 9001), false)
	f.failAll = errors.New("boom")
	n := len(f.loads)
	if _, err := ap.Apply(ctx, routes(9000), false); err == nil || len(f.loads) != n+1 || !reflect.DeepEqual(ap.Applied(), []int{9000, 9001}) {
		t.Fatalf("%v %d", err, len(f.loads))
	}
	// A new port when everything fails: error, nothing recorded as applied.
	if failed, err := ap.Apply(ctx, routes(9000, 9001, 9002), false); err == nil || failed != nil || !reflect.DeepEqual(ap.Applied(), []int{9000, 9001}) {
		t.Fatalf("%v %v %v", failed, err, ap.Applied())
	}
	// A bad route list never reaches Caddy.
	n = len(f.loads)
	if _, err := ap.Apply(ctx, routes(80), false); err == nil || len(f.loads) != n {
		t.Fatalf("%v", err)
	}
}

func TestBindFailureIsolatedToOnePort(t *testing.T) {
	ap := startApplier(t)
	ctx := context.Background()
	pOld, pGood, pBad := freePort(t), freePort(t), freePort(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "old app") }))
	defer up.Close()
	if failed, err := ap.Apply(ctx, []AppRoute{{Port: pOld, Upstream: hostPort(up)}}, false); err != nil || len(failed) != 0 {
		t.Fatalf("%v %v", failed, err)
	}
	// Another program holds pBad.
	hold, err := net.Listen("tcp", ":"+strconv.Itoa(pBad))
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()

	failed, err := ap.Apply(ctx, []AppRoute{
		{Port: pOld, Upstream: hostPort(up)},
		{Port: pBad, Static: NotRunning()},
		{Port: pGood, Static: Maintenance()},
	}, false)
	if err != nil || !reflect.DeepEqual(failed, []int{pBad}) {
		t.Fatalf("failed %v, err %v", failed, err)
	}
	for port, want := range map[int]string{pOld: "old app", pGood: "Deploying, back in a moment"} {
		if _, body := get(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/"); !strings.Contains(body, want) {
			t.Errorf("port %d: %q", port, body)
		}
	}
	// The held port still answers with the other program's listener, not ours.
	if got := ap.Applied(); !reflect.DeepEqual(got, sortedInts(pOld, pGood)) {
		t.Errorf("applied %v", got)
	}
	// Once the other program lets go, the same list applies in full.
	hold.Close()
	failed, err = ap.Apply(ctx, []AppRoute{
		{Port: pOld, Upstream: hostPort(up)},
		{Port: pBad, Static: NotRunning()},
		{Port: pGood, Static: Maintenance()},
	}, true)
	if err != nil || len(failed) != 0 {
		t.Fatalf("after release: %v %v", failed, err)
	}
	if resp, _ := get(t, "http://127.0.0.1:"+strconv.Itoa(pBad)+"/"); resp.StatusCode != 503 {
		t.Errorf("released port: %d", resp.StatusCode)
	}
}

func sortedInts(v ...int) []int {
	out := append([]int(nil), v...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// hostIPv4 returns a non-loopback IPv4 address of this machine ("" if none):
// Go's ProxyFromEnvironment never proxies loopback, so the test needs one.
func hostIPv4() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			return ipn.IP.String()
		}
	}
	return ""
}

// TestAppRouteIgnoresProxyEnvironment: an app route must dial its container
// directly even when HTTP_PROXY is set. Go reads the environment once per
// process, so the proxied request runs in a child process that starts with it.
func TestAppRouteIgnoresProxyEnvironment(t *testing.T) {
	if os.Getenv("SD_PROXY_ENV_CHILD") == "1" {
		return // the child runs childAppRouteProxyEnv below
	}
	ip := hostIPv4()
	if ip == "" {
		t.Skip("no non-loopback IPv4 address")
	}
	var hits atomic.Int64
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Close()
	go func() {
		for {
			c, err := pl.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			c.Close()
		}
	}()
	upLn, err := net.Listen("tcp", ip+":0")
	if err != nil {
		t.Skip(err)
	}
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "direct") }))
	up.Listener.Close()
	up.Listener = upLn
	up.Start()
	defer up.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestChildAppRouteProxyEnv$", "-test.count=1")
	cmd.Env = append(os.Environ(), "SD_PROXY_ENV_CHILD=1", "SD_PROXY_ENV_UPSTREAM="+hostPort(up),
		"HTTP_PROXY=http://"+pl.Addr().String(), "http_proxy=http://"+pl.Addr().String(),
		"HTTPS_PROXY=http://"+pl.Addr().String(), "NO_PROXY=", "no_proxy=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the proxy named by HTTP_PROXY received %d connections", n)
	}
}

func TestChildAppRouteProxyEnv(t *testing.T) {
	if os.Getenv("SD_PROXY_ENV_CHILD") != "1" {
		t.Skip("run by TestAppRouteIgnoresProxyEnvironment")
	}
	upstream := os.Getenv("SD_PROXY_ENV_UPSTREAM")
	// Control: the environment really would proxy a request to this address.
	req, _ := http.NewRequest("GET", "http://"+upstream+"/", nil)
	if u, err := http.ProxyFromEnvironment(req); err != nil || u == nil {
		t.Fatalf("the control failed: the environment does not select a proxy (%v, %v)", u, err)
	}
	ap := startApplier(t)
	port := freePort(t)
	if failed, err := ap.Apply(context.Background(), []AppRoute{{Port: port, Upstream: upstream}}, false); err != nil || len(failed) != 0 {
		t.Fatalf("%v %v", failed, err)
	}
	// The request to Caddy itself is on loopback, which is never proxied.
	if _, body := get(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/"); body != "direct" {
		t.Fatalf("body %q", body)
	}
}

func TestBindErrorPortIsTriedFirst(t *testing.T) {
	// The fake names the port like Caddy does: one reload without it, not a loop.
	f := &fakeLoad{failOn: map[int]bool{9002: true}}
	ap := fakeApplier(f)
	_ = ap.Start()
	n := len(f.loads)
	failed, err := ap.Apply(context.Background(), routes(9000, 9001, 9002, 9003), false)
	if err != nil || !reflect.DeepEqual(failed, []int{9002}) {
		t.Fatalf("%v %v", failed, err)
	}
	if got := len(f.loads) - n; got != 2 { // the full list, then without 9002
		t.Fatalf("%d loads", got)
	}
	if p, ok := bindErrorPort(errors.New("loading new config: http app module: start: listening on :9001: listen tcp :9001: bind: address already in use")); !ok || p != 9001 {
		t.Fatalf("%d %v", p, ok)
	}
	for _, e := range []string{"permission denied", "listen tcp :9001: bind: permission denied"} {
		if _, ok := bindErrorPort(errors.New(e)); ok {
			t.Errorf("%q parsed as a bind error", e)
		}
	}
}
