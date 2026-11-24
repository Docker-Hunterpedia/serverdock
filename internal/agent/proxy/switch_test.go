package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// Plan 0004 step 6b: an upstream switch swaps a table and makes no caddy.Load.

func up(port int, addr string) AppRoute { return AppRoute{Port: port, Upstream: addr} }

// tableOf returns what the handler would answer on port, as a comparable string.
func tableOf(port int) string {
	e := lookup(table.Load(), port)
	if e.upstream != "" {
		return "up " + e.upstream
	}
	return fmt.Sprintf("%d %q ra=%s", e.status, e.body, e.retryAfter)
}

func staticEntry(s *Static) string {
	ra := ""
	if s.RetryAfter > 0 {
		ra = strconv.Itoa(s.RetryAfter)
	}
	return fmt.Sprintf("%d %q ra=%s", s.Status, s.Body, ra)
}

func TestSwitchMakesNoLoad(t *testing.T) {
	ctx := context.Background()
	f := &fakeLoad{}
	ap := fakeApplier(f)
	_ = ap.Start()
	apply := func(r ...AppRoute) {
		t.Helper()
		if failed, err := ap.Apply(ctx, r, false); err != nil || len(failed) != 0 {
			t.Fatalf("%v %v", failed, err)
		}
	}
	apply(up(9000, "10.0.0.2:80"), up(9001, "10.0.0.9:80"))
	n := len(f.loads)
	steps := []struct {
		name   string
		routes []AppRoute
		want   map[int]string
	}{
		{"upstream to upstream", []AppRoute{up(9000, "10.0.0.3:80"), up(9001, "10.0.0.9:80")}, map[int]string{9000: "up 10.0.0.3:80", 9001: "up 10.0.0.9:80"}},
		{"upstream to maintenance", []AppRoute{{Port: 9000, Static: Maintenance()}, up(9001, "10.0.0.9:80")}, map[int]string{9000: staticEntry(Maintenance())}},
		{"maintenance to upstream", []AppRoute{up(9000, "10.0.0.4:80"), up(9001, "10.0.0.9:80")}, map[int]string{9000: "up 10.0.0.4:80"}},
		{"upstream to not running", []AppRoute{up(9000, "10.0.0.4:80"), {Port: 9001, Static: NotRunning()}}, map[int]string{9001: staticEntry(NotRunning())}},
		{"not running to upstream", []AppRoute{up(9000, "10.0.0.4:80"), up(9001, "10.0.0.5:80")}, map[int]string{9001: "up 10.0.0.5:80"}},
		{"revert", []AppRoute{up(9000, "10.0.0.2:80"), up(9001, "10.0.0.9:80")}, map[int]string{9000: "up 10.0.0.2:80", 9001: "up 10.0.0.9:80"}},
	}
	for _, s := range steps {
		apply(s.routes...)
		for port, want := range s.want {
			if got := tableOf(port); got != want {
				t.Errorf("%s: port %d answers %s, want %s", s.name, port, got, want)
			}
		}
		if len(f.loads) != n {
			t.Fatalf("%s: %d load calls, want 0", s.name, len(f.loads)-n)
		}
	}
	// A port outside the table is "not running".
	if got := tableOf(9999); got != staticEntry(NotRunning()) {
		t.Errorf("unknown port: %s", got)
	}
}

func TestPortSetChangeLoadsWithBindIsolation(t *testing.T) {
	ctx := context.Background()
	f := &fakeLoad{}
	ap := fakeApplier(f)
	_ = ap.Start()
	loads := func() int { return len(f.loads) }
	n := loads()
	// Add.
	if failed, err := ap.Apply(ctx, routes(9000), false); err != nil || len(failed) != 0 || loads() != n+1 {
		t.Fatalf("add: %v %v %d", failed, err, loads()-n)
	}
	// Change (one port replaced by another).
	if failed, err := ap.Apply(ctx, routes(9001), false); err != nil || len(failed) != 0 || loads() != n+2 {
		t.Fatalf("change: %v %v %d", failed, err, loads()-n)
	}
	if tableOf(9000) != staticEntry(NotRunning()) || len(*table.Load()) != 1 {
		t.Errorf("the table kept the old port: %v", *table.Load())
	}
	// A taken port is isolated and left out of the table (and of the listen list).
	f.failOn = map[int]bool{9005: true}
	n = loads()
	failed, err := ap.Apply(ctx, []AppRoute{up(9001, "10.0.0.2:80"), up(9005, "10.0.0.3:80"), up(9006, "10.0.0.4:80")}, false)
	if err != nil || !reflect.DeepEqual(failed, []int{9005}) || loads() != n+2 {
		t.Fatalf("isolation: %v %v %d loads", failed, err, loads()-n)
	}
	if _, ok := (*table.Load())[9005]; ok || tableOf(9006) != "up 10.0.0.4:80" {
		t.Errorf("table after isolation: %v", *table.Load())
	}
	for _, c := range f.loads[n+1:] {
		for _, a := range c.Apps {
			if a.Port == 9005 {
				t.Errorf("the isolated load still lists the taken port")
			}
		}
	}
	// Clear.
	n = loads()
	if failed, err := ap.Apply(ctx, nil, false); err != nil || len(failed) != 0 || loads() != n+1 {
		t.Fatalf("clear: %v %v %d", failed, err, loads()-n)
	}
	if table.Load() != nil {
		t.Errorf("a table with no app routes: %v", *table.Load())
	}
}

func TestSwitchWithAFailedPortMakesNoLoad(t *testing.T) {
	ctx := context.Background()
	f := &fakeLoad{failOn: map[int]bool{9001: true}}
	ap := fakeApplier(f)
	_ = ap.Start()
	failed, err := ap.Apply(ctx, []AppRoute{up(9000, "10.0.0.2:80"), up(9001, "10.0.0.3:80")}, false)
	if err != nil || !reflect.DeepEqual(failed, []int{9001}) {
		t.Fatalf("%v %v", failed, err)
	}
	n := len(f.loads)
	// Switches of the other service, with the failed port still in the list, and
	// the 8b commit shape (retry_failed false), reload nothing and still report it.
	for i, a := range []string{"10.0.0.4:80", "10.0.0.5:80", "10.0.0.2:80"} {
		failed, err := ap.Apply(ctx, []AppRoute{up(9000, a), up(9001, "10.0.0.3:80")}, false)
		if err != nil || !reflect.DeepEqual(failed, []int{9001}) || len(f.loads) != n {
			t.Fatalf("switch %d: %v %v, %d loads", i, failed, err, len(f.loads)-n)
		}
		if tableOf(9000) != "up "+a {
			t.Fatalf("switch %d: the table says %s", i, tableOf(9000))
		}
		if _, ok := (*table.Load())[9001]; ok {
			t.Fatal("the failed port is in the table")
		}
	}
	// The failed port's own page changing (it has no listener) is a switch too.
	if _, err := ap.Apply(ctx, []AppRoute{up(9000, "10.0.0.2:80"), {Port: 9001, Static: Maintenance()}}, false); err != nil || len(f.loads) != n {
		t.Fatalf("%v, %d loads", err, len(f.loads)-n)
	}
}

func TestTryAgainStillLoads(t *testing.T) {
	ctx := context.Background()
	f := &fakeLoad{failOn: map[int]bool{9001: true}}
	ap := fakeApplier(f)
	_ = ap.Start()
	want := []AppRoute{up(9000, "10.0.0.2:80"), up(9001, "10.0.0.3:80")}
	if failed, err := ap.Apply(ctx, want, false); err != nil || !reflect.DeepEqual(failed, []int{9001}) {
		t.Fatalf("%v %v", failed, err)
	}
	// Still taken: one failed load, no second load of a config identical to the
	// running one, and the port is reported failed again.
	n := len(f.loads)
	failed, err := ap.Apply(ctx, want, true)
	if err != nil || !reflect.DeepEqual(failed, []int{9001}) {
		t.Fatalf("retry while taken: %v %v", failed, err)
	}
	if got := len(f.loads) - n; got != 1 {
		t.Fatalf("%d loads for a retry of a still-taken port, want 1 (the failed one)", got)
	}
	if !reflect.DeepEqual(ap.Applied(), []int{9000}) || tableOf(9000) != "up 10.0.0.2:80" {
		t.Fatalf("applied %v, table %v", ap.Applied(), *table.Load())
	}
	// Free again: the retry loads and binds it.
	f.failOn = nil
	n = len(f.loads)
	failed, err = ap.Apply(ctx, want, true)
	if err != nil || len(failed) != 0 || len(f.loads)-n != 1 {
		t.Fatalf("retry after release: %v %v, %d loads", failed, err, len(f.loads)-n)
	}
	if !reflect.DeepEqual(ap.Applied(), []int{9000, 9001}) || tableOf(9001) != "up 10.0.0.3:80" {
		t.Fatalf("applied %v, table %v", ap.Applied(), *table.Load())
	}
	// And a retry with nothing failed and nothing changed loads nothing.
	n = len(f.loads)
	if _, err := ap.Apply(ctx, want, true); err != nil || len(f.loads) != n {
		t.Fatalf("%v, %d loads", err, len(f.loads)-n)
	}
}

func TestTableAndConfigNeverDivergeAfterAFailedLoad(t *testing.T) {
	ctx := context.Background()
	// A port-set change whose load fails keeps the previous table, applied and failed.
	f := &fakeLoad{failOn: map[int]bool{9001: true}}
	ap := fakeApplier(f)
	_ = ap.Start()
	if _, err := ap.Apply(ctx, []AppRoute{up(9000, "10.0.0.2:80"), up(9001, "10.0.0.3:80")}, false); err != nil {
		t.Fatal(err)
	}
	f.failAll = errors.New("boom")
	if failed, err := ap.Apply(ctx, []AppRoute{up(9000, "10.0.0.7:80"), up(9002, "10.0.0.8:80")}, false); err == nil || failed != nil {
		t.Fatalf("%v %v", failed, err)
	}
	if !reflect.DeepEqual(ap.Applied(), []int{9000}) || tableOf(9000) != "up 10.0.0.2:80" || len(*table.Load()) != 1 || !ap.failed[9001] {
		t.Fatalf("state moved on a failed load: applied %v table %v failed %v", ap.Applied(), *table.Load(), ap.failed)
	}
	f.failAll = nil

	// A bind-isolation sequence that loads part-way and then hits ctx.Err(): the
	// table matches the last successful load.
	f = &fakeLoad{failOn: map[int]bool{9001: true, 9002: true}}
	ap = fakeApplier(f)
	_ = ap.Start()
	cctx, cancel := context.WithCancel(ctx)
	base := ap.load
	ap.load = func(c Config) error {
		err := base(c)
		if err == nil && len(c.Apps) == 1 { // the first successful load of the sequence: 9000 alone
			cancel()
		}
		return err
	}
	desired := []AppRoute{up(9000, "10.0.0.2:80"), up(9001, "10.0.0.3:80"), up(9002, "10.0.0.4:80")}
	if _, err := ap.Apply(cctx, desired, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("want ctx.Err(), got %v", err)
	}
	if !reflect.DeepEqual(ap.Applied(), []int{9000}) || tableOf(9000) != "up 10.0.0.2:80" || len(*table.Load()) != 1 {
		t.Fatalf("after the partial sequence: applied %v table %v", ap.Applied(), *table.Load())
	}
	// The control plane cleared its hash and resends: it lands (the ports are free now).
	f.failOn = nil
	ap.load = base
	n := len(f.loads)
	if failed, err := ap.Apply(ctx, desired, false); err != nil || len(failed) != 0 || len(f.loads) != n+1 {
		t.Fatalf("resend: %v %v, %d loads", failed, err, len(f.loads)-n)
	}
	if len(*table.Load()) != 3 {
		t.Fatalf("table after the resend: %v", *table.Load())
	}
}

// ---- the handler ----

// handlerServer serves Handler on a loopback listener the way Caddy's server
// does (net/http sets LocalAddrContextKey); next reports the variable it saw.
func handlerServer(t *testing.T) (port int, calls *atomic.Int64) {
	t.Helper()
	calls = new(atomic.Int64)
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		calls.Add(1)
		fmt.Fprint(w, "next:"+fmt.Sprint(caddyhttp.GetVar(r.Context(), upstreamVar)))
		return nil
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), caddyhttp.VarsCtxKey, map[string]any{})
		_ = Handler{}.ServeHTTP(w, r.WithContext(ctx), next)
	}))
	t.Cleanup(srv.Close)
	_, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ = strconv.Atoi(p)
	return port, calls
}

func TestHandlerUnderConcurrentSwaps(t *testing.T) {
	t.Cleanup(func() { table.Store(nil) })
	portA, _ := handlerServer(t)
	portB, _ := handlerServer(t)
	portNone, _ := handlerServer(t)
	// Three whole tables; each response on a port must match one of them whole.
	pageA := &Static{Status: 503, Body: "page A\n", RetryAfter: 7}
	pageB := &Static{Status: 429, Body: "page B\n", RetryAfter: 9}
	tables := []*appTable{
		newTable([]AppRoute{{Port: portA, Static: pageA}, {Port: portB, Static: pageA}}),
		newTable([]AppRoute{{Port: portA, Static: pageB}, {Port: portB, Static: pageB}}),
		newTable([]AppRoute{up(portA, "10.1.1.1:80"), up(portB, "10.1.1.2:80")}),
		nil,
	}
	// What each port may answer: any of its own table entries, never another
	// port's upstream.
	notRunning := "503|" + BodyNotRunning + "|"
	valid := map[int]map[string]bool{
		portA:    {"503|page A\n|7": true, "429|page B\n|9": true, "200|next:10.1.1.1:80|": true, notRunning: true},
		portB:    {"503|page A\n|7": true, "429|page B\n|9": true, "200|next:10.1.1.2:80|": true, notRunning: true},
		portNone: {notRunning: true},
	}
	table.Store(tables[0])
	stop := make(chan struct{})
	var swaps sync.WaitGroup
	swaps.Add(1)
	go func() {
		defer swaps.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			table.Store(tables[i%len(tables)])
		}
	}()
	var wg sync.WaitGroup
	var bad atomic.Int64
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 150; i++ {
				port := []int{portA, portB, portNone}[i%3]
				resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
				if err != nil {
					bad.Add(1)
					t.Error(err)
					return
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				key := fmt.Sprintf("%d|%s|%s", resp.StatusCode, b, resp.Header.Get("Retry-After"))
				if !valid[port][key] {
					bad.Add(1)
					t.Errorf("port %d answered %q (half a table, or another port's entry)", port, key)
				}
				if resp.StatusCode != 200 && (resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8") {
					bad.Add(1)
					t.Errorf("headers: %v", resp.Header)
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	swaps.Wait()
}

func TestHandlerNeverCallsNextWithoutTheVar(t *testing.T) {
	t.Cleanup(func() { table.Store(nil) })
	port, calls := handlerServer(t)
	check := func(tbl *appTable, wantNext bool, wantBody string) {
		t.Helper()
		table.Store(tbl)
		before := calls.Load()
		_, body := get(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/")
		if (calls.Load() != before) != wantNext || !strings.HasPrefix(body, wantBody) {
			t.Errorf("next called %v, body %q", calls.Load() != before, body)
		}
	}
	check(nil, false, BodyNotRunning)
	check(newTable(nil), false, BodyNotRunning)
	check(newTable([]AppRoute{{Port: port, Static: Maintenance()}}), false, BodyMaintenance)
	check(newTable([]AppRoute{up(port, "10.2.2.2:80")}), true, "next:10.2.2.2:80")
}

// ---- real Caddy ----

// countingApplier returns a started Applier over real Caddy that counts loads.
func countingApplier(t *testing.T) (*Applier, *atomic.Int64) {
	t.Helper()
	ap := NewApplier(panelConfig(t))
	loads := new(atomic.Int64)
	ap.load = func(c Config) error { loads.Add(1); return Load(c) }
	if err := ap.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ap.Stop() })
	return ap, loads
}

// recorder is a loopback listener that counts the connections it accepts and
// answers every request with its name.
type recorder struct {
	addr  string
	conns atomic.Int64
	srv   *httptest.Server
}

func newRecorder(t *testing.T, name string) *recorder {
	t.Helper()
	r := &recorder{}
	r.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { fmt.Fprint(w, name) }))
	r.srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			r.conns.Add(1)
		}
	}
	r.srv.Start()
	t.Cleanup(r.srv.Close)
	r.addr = hostPort(r.srv)
	return r
}

func TestClientCannotInjectUpstream(t *testing.T) {
	ap, _ := countingApplier(t)
	good, evil := newRecorder(t, "good app"), newRecorder(t, "EVIL")
	other := newRecorder(t, "other app")
	pGood, pOther := freePort(t), freePort(t)
	if failed, err := ap.Apply(context.Background(), []AppRoute{up(pGood, good.addr), up(pOther, other.addr)}, false); err != nil || len(failed) != 0 {
		t.Fatalf("%v %v", failed, err)
	}
	if _, body := get(t, "http://127.0.0.1:"+strconv.Itoa(pGood)+"/"); body != "good app" {
		t.Fatalf("baseline: %q", body)
	}
	for name, hdr := range map[string]string{
		"sd_upstream": evil.addr, "Sd-Upstream": evil.addr, "X-Sd-Upstream": evil.addr, "X-Sd_upstream": evil.addr,
		"X-Forwarded-Host": evil.addr, "Via": "{http.vars.sd_upstream}", "X-Upstream": "{http.vars.sd_upstream}",
		"Cookie": "sd_upstream=" + evil.addr, "Referer": "http://" + evil.addr + "/{http.vars.sd_upstream}",
	} {
		for _, target := range []string{
			"/", "/sd_upstream=" + evil.addr, "/{http.vars.sd_upstream}", "/?sd_upstream=" + evil.addr + "&x={http.vars.sd_upstream}",
			"/?{http.vars.sd_upstream}=" + evil.addr,
		} {
			req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(pGood)+target, nil)
			req.Header.Set(name, hdr)
			if name == "Cookie" || name == "Via" {
				req.Header.Add(name, "sd_upstream="+evil.addr)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", name, target, err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(b) != "good app" {
				t.Errorf("header %s, target %s: got %q", name, target, b)
			}
		}
	}
	// Host named or valued as the variable or its placeholder.
	for _, host := range []string{"sd_upstream", "{http.vars.sd_upstream}", evil.addr, "sd_upstream." + evil.addr} {
		req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(pGood)+"/", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Host %q: %v", host, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		// Either the right app, or Go's server refuses the malformed Host (400);
		// never anything from the address the client named.
		if string(b) != "good app" && resp.StatusCode != 400 {
			t.Errorf("Host %q: %d %q", host, resp.StatusCode, b)
		}
	}
	// The other app's port never reaches the first app's upstream either.
	if _, body := get(t, "http://127.0.0.1:"+strconv.Itoa(pOther)+"/?sd_upstream="+good.addr); body != "other app" {
		t.Errorf("other app: %q", body)
	}
	if n := evil.conns.Load(); n != 0 {
		t.Fatalf("an address outside the table was dialed %d times", n)
	}
}

// A reverse_proxy reached without the variable (the handler bypassed) fails
// closed: nothing is dialed and the client gets an error status.
func TestReverseProxyWithoutTheVarFailsClosed(t *testing.T) {
	evil := newRecorder(t, "EVIL")
	port := freePort(t)
	cfg := fmt.Sprintf(`{"admin":{"disabled":true,"config":{"persist":false}},"apps":{"http":{"servers":{"x":{
		"listen":[":%d"],"automatic_https":{"disable":true},
		"routes":[{"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"{http.vars.sd_upstream}"}]}]}]}}}}}`, port)
	if err := caddy.Load([]byte(cfg), true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = caddy.Stop() })
	req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(port)+"/?sd_upstream="+evil.addr, nil)
	req.Header.Set("sd_upstream", evil.addr)
	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ {
		if resp, err = http.DefaultClient.Do(req); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("status %d, want 502", resp.StatusCode)
	}
	if n := evil.conns.Load(); n != 0 {
		t.Errorf("%d connections reached an address the client named", n)
	}
}

// one ticker = one goroutine running the reverse_proxy metrics updater.
func TestOneTickerForAllAppRoutes(t *testing.T) {
	ap, loads := countingApplier(t)
	waitStable := func() int {
		prev := -1
		for i := 0; i < 100; i++ {
			n := tickers()
			if n == prev {
				return n
			}
			prev = n
			time.Sleep(20 * time.Millisecond)
		}
		return prev
	}
	base := waitStable() // the panel's own, plus any a previous test left
	mk := func(n int, upstream bool) []AppRoute {
		out := make([]AppRoute, n)
		var held []net.Listener // hold them all so no port repeats
		for range out {
			ln, err := net.Listen("tcp", ":0")
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, ln)
		}
		for i := range out {
			out[i] = AppRoute{Port: held[i].Addr().(*net.TCPAddr).Port, Static: NotRunning()}
			if upstream {
				out[i] = AppRoute{Port: out[i].Port, Upstream: "10.200.0." + strconv.Itoa(2+i%200) + ":8080"}
			}
		}
		for _, ln := range held {
			ln.Close()
		}
		return out
	}
	ctx := context.Background()
	for _, n := range []int{1, 10, 100} {
		for _, upstream := range []bool{false, true} {
			routes := mk(n, upstream)
			if failed, err := ap.Apply(ctx, routes, false); err != nil || len(failed) != 0 {
				t.Fatalf("%d routes: %v %v", n, failed, err)
			}
			waitTickers(t, base+1)
			// A switch provisions no new handler: no load, and the count is unchanged.
			before := loads.Load()
			sw := append([]AppRoute(nil), routes...)
			sw[0] = AppRoute{Port: sw[0].Port, Static: Maintenance()}
			if _, err := ap.Apply(ctx, sw, false); err != nil || loads.Load() != before {
				t.Fatalf("%d routes: switch %v, %d loads", n, err, loads.Load()-before)
			}
			waitTickers(t, base+1)
			if failed, err := ap.Apply(ctx, nil, false); err != nil || len(failed) != 0 {
				t.Fatal(failed, err)
			}
			waitTickers(t, base) // no public port: no app server, no app ticker
		}
	}
}

func TestFailingAppDoesNotAffectAnother(t *testing.T) {
	ap, _ := countingApplier(t)
	good := newRecorder(t, "good")
	dead := freePort(t) // nothing listens: connection refused
	pBad, pGood := freePort(t), freePort(t)
	if failed, err := ap.Apply(context.Background(), []AppRoute{up(pBad, "127.0.0.1:"+strconv.Itoa(dead)), up(pGood, good.addr)}, false); err != nil || len(failed) != 0 {
		t.Fatalf("%v %v", failed, err)
	}
	var wg sync.WaitGroup
	var badOK, goodFail atomic.Int64
	for w := 0; w < 4; w++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 60; i++ {
				resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(pBad) + "/")
				if err == nil {
					if resp.StatusCode < 500 {
						badOK.Add(1)
					}
					resp.Body.Close()
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 60; i++ {
				resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(pGood) + "/")
				if err != nil {
					goodFail.Add(1)
					continue
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 || string(b) != "good" {
					goodFail.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if badOK.Load() != 0 {
		t.Errorf("%d answers from a dead upstream", badOK.Load())
	}
	if goodFail.Load() != 0 {
		t.Errorf("%d failed requests on the healthy app while the other one was failing", goodFail.Load())
	}
}
