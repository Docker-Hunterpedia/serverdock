//go:build integration

package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// addLiveDeploy inserts a deploy row that was once live and carries a real snapshot
// (the stop-first gate of the old version reads its health check and port).
func addLiveDeploy(t *testing.T, st *store.Store, svc int64, number int, ip string, snap Snapshot) int64 {
	t.Helper()
	raw, err := snap.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.W.Exec(`INSERT INTO deploys(service_id, number, kind, snapshot_json, ip, status, committed_at, actor_kind, actor_id, created_at)
		VALUES (?, ?, 'deploy', ?, ?, 'succeeded', 'x', 'user', '1', 'x')`, svc, number, string(raw), ip)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// stopFirstUnderLoad runs a real stop-first deploy (real runner, agent ops, Docker
// and embedded Caddy) of an app that serves under constant POST load, with a second
// app and the panel under load too, over keep-alive and new connections.
//
// Strict (plan 0004 step 9, Revision 11): on the deploying app every response is a
// 2xx from an app container or exactly the maintenance 503 page with its Retry-After
// (the gap); the second app and the panel only 2xx; zero dropped connections, zero
// Go-internal retries, zero failures after a client retry; and ZERO Caddy loads (the
// exact caddy.ActiveContext comparison) across the whole deploy: upstream to
// maintenance to upstream, and, when failing, the restart-the-old path. Exactly two
// proxy_apply calls are sent (the page, then the new or the old upstream).
func stopFirstUnderLoad(t *testing.T, failing bool) {
	g, ref := newSwitchRig(t)
	ctx := g.ctx
	x := newITRunner(t, g, nil)
	base := g.subnet[:strings.LastIndex(g.subnet, ".")]
	ipOld, ipB := base+".2", base+".4"

	svcB := addService(t, x.st, "second")
	oldSnap := Snapshot{V: 1, ImageRef: ref, ContainerPort: 8080, Health: SnapHealth{Kind: "http", Path: "/cgi-bin/id", TimeoutS: 30, Passes: 2},
		Strategy: "stop_first", DrainS: 10, StopGraceS: 1}
	oldDeploy := addLiveDeploy(t, x.st, x.svc, 1, ipOld, oldSnap)
	dB := addDeploy(t, x.st, svcB, 1, "succeeded", ipB)
	g.startApp(t, ref, "OLD", ipOld, itOwnedLabels(1, x.svc, oldDeploy)...)
	g.startApp(t, ref, "B", ipB, itOwnedLabels(1, svcB, dB)...)
	portA, portB := itFreePort(t), itFreePort(t)
	mustExec(t, x.st, `UPDATE services SET public_port = ?, container_port = 8080, active_deploy_id = ? WHERE id = ?`, portA, oldDeploy, x.svc)
	mustExec(t, x.st, `UPDATE services SET public_port = ?, container_port = 8080, active_deploy_id = ? WHERE id = ?`, portB, dB, svcB)
	oldID, bID := g.containerID(t, "OLD"), g.containerID(t, "B")
	if err := x.rt.ApplyRoutes(ctx, func() error {
		x.rt.state.replace([]Container{
			{ID: oldID, Service: x.svc, Deploy: oldDeploy, State: StateRunning, IP: ipOld},
			{ID: bID, Service: svcB, Deploy: dB, State: StateRunning, IP: ipB},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := g.applier.Applied(); len(got) != 2 {
		t.Fatalf("applied %v", got)
	}

	only := func(want string) func(int, string) string {
		return func(s int, b string) string {
			if s/100 != 2 || b != want {
				return fmt.Sprintf("%d %q, want a 2xx %q", s, b, want)
			}
			return ""
		}
	}
	maintenance := strings.TrimSpace(proxy.BodyMaintenance)
	var firstNew atomic.Int64 // unix nanoseconds of the first 2xx from the NEW container (success path)
	deploying := func(s int, h http.Header, b string) string {
		switch {
		case s/100 == 2 && strings.HasPrefix(strings.TrimSpace(b), "app-") && strings.TrimSpace(b) != "app-B":
			if !failing && strings.TrimSpace(b) != "app-OLD" {
				firstNew.CompareAndSwap(0, time.Now().UnixNano())
			}
			return ""
		case s == 503 && !failing && firstNew.Load() != 0 && time.Now().UnixNano()-firstNew.Load() > int64(100*time.Millisecond):
			return fmt.Sprintf("a maintenance 503 more than 100 ms after the new container first answered")
		case s == 503 && b == proxy.BodyMaintenance && h.Get("Retry-After") == "10" && h.Get("Cache-Control") == "no-store" && h.Get("Content-Type") == "text/plain; charset=utf-8":
			return ""
		}
		return fmt.Sprintf("%d %q retry-after %q", s, b, h.Get("Retry-After"))
	}
	load := startLoad([]itTarget{
		{name: "deploying app", url: "http://127.0.0.1:" + strconv.Itoa(portA) + "/cgi-bin/id", checkResp: deploying},
		{name: "second app", url: "http://127.0.0.1:" + strconv.Itoa(portB) + "/cgi-bin/id", check: only("app-B")},
		{name: "panel", url: "https://127.0.0.1:" + strconv.Itoa(g.panelPort) + "/", check: only("panel")},
	})
	time.Sleep(300 * time.Millisecond)

	ctxBefore := caddy.ActiveContext()
	applyBefore := x.cc.n("proxy_apply")
	snap := oldSnap
	if failing {
		snap.Health = SnapHealth{Kind: "http", Path: "/no-such-page", TimeoutS: 5, Passes: 1} // the new version is unhealthy
	}
	t0 := time.Now()
	d, runErr := x.deploy(t, snap)
	took := time.Since(t0)
	time.Sleep(300 * time.Millisecond)
	loaded := caddyLoaded(ctxBefore)
	applyDuring := x.cc.n("proxy_apply") - applyBefore
	load.finish()
	total, bad, drops, retries, fails := load.report(t)
	for _, a := range load.anoms {
		t.Logf("anomaly at %s", a)
	}

	var gap503 [2]int64
	seen := map[string]int64{}
	for i, class := range []string{"keep-alive", "new-connection"} {
		for _, c := range load.counters {
			if c.target == "deploying app" && c.class == class {
				for b, n := range c.seen() {
					seen[b] += n
					if b == maintenance {
						gap503[i] += n
					}
				}
			}
		}
	}
	var kinds []string
	for b, n := range seen {
		kinds = append(kinds, fmt.Sprintf("%s x%d", b, n))
	}
	sort.Strings(kinds)
	t.Logf("RESULT failing=%v: 503s keep-alive %d, new-connection %d; dropped %d; retries %d; Caddy loads %v; proxy_apply %d; deploy took %v; responses: %s",
		failing, gap503[0], gap503[1], drops, retries, loaded, applyDuring, took.Round(time.Millisecond), strings.Join(kinds, ", "))

	status, code, reason, appSafe := x.row(t, d)
	if failing {
		if runErr == nil || status != "failed" || code != CodeHealthTimeout || appSafe != AppSafeYes {
			t.Fatalf("the failing deploy: %v / %s %s %q app_safe=%s", runErr, status, code, reason, appSafe)
		}
		if !strings.Contains(reason, "previous version is serving again") {
			t.Errorf("reason %q", reason)
		}
	} else if runErr != nil || status != "succeeded" || code != "" {
		t.Fatalf("the deploy: %v / %s %s %q", runErr, status, code, reason)
	}
	if bad != 0 || drops != 0 || retries != 0 || fails != 0 {
		t.Errorf("non-conforming %d, dropped %d, retries %d, failed %d (first: %v)", bad, drops, retries, fails, load.firstBad.Load())
	}
	if loaded {
		t.Error("Caddy loaded a config during the stop-first deploy")
	}
	if applyDuring != 2 {
		t.Errorf("%d proxy_apply calls, want 2 (the page, then the upstream)", applyDuring)
	}
	if gap503[0] == 0 || gap503[1] == 0 {
		t.Errorf("the gap was not seen by both connection classes: %v", gap503)
	}
	if total < 400 {
		t.Errorf("only %d requests in all", total)
	}
	if seen["app-OLD"] == 0 {
		t.Errorf("the old version was never seen: %v", seen)
	}
	if failing {
		if x.activeOf(t, x.svc) != oldDeploy {
			t.Error("the active deploy changed")
		}
		if n := x.containersOfDeploy(t, d); n != 0 {
			t.Errorf("%d containers of the failed deploy are left", n)
		}
		if out := itDocker(t, "ps", "-q", "--filter", "id="+oldID); out == "" {
			t.Error("the old container is not running again")
		}
		if resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(portA) + "/cgi-bin/id"); err != nil {
			t.Errorf("the app does not answer after the failed deploy: %v", err)
		} else {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || strings.TrimSpace(string(b)) != "app-OLD" {
				t.Errorf("after the failed deploy the app answers %d %q, want 200 app-OLD", resp.StatusCode, b)
			}
		}
		var attention *string
		x.st.R.QueryRow(`SELECT needs_attention FROM services WHERE id = ?`, x.svc).Scan(&attention)
		if attention != nil {
			t.Errorf("needs_attention %q", *attention)
		}
	} else {
		if firstNew.Load() == 0 {
			t.Error("the new container never answered through the route")
		}
		if x.activeOf(t, x.svc) != d {
			t.Error("the new deploy is not active")
		}
		if out := itDocker(t, "ps", "-a", "-q", "--filter", "id="+oldID); out != "" {
			t.Errorf("the old container still exists: %s", out)
		}
		for _, s := range stopFirstStages {
			var state string
			x.st.R.QueryRow(`SELECT state FROM deploy_stages WHERE deploy_id = ? AND stage = ?`, d, s).Scan(&state)
			if state != StageOK {
				t.Errorf("stage %s is %q", s, state)
			}
		}
	}
	if out := itDocker(t, "ps", "-q", "--filter", "id="+bID); out == "" {
		t.Error("the second app's container is not running")
	}
}

func TestIntegrationStopFirstGapUnderLoad(t *testing.T)               { stopFirstUnderLoad(t, false) }
func TestIntegrationFailedStopFirstRestartsOldUnderLoad(t *testing.T) { stopFirstUnderLoad(t, true) }

// nopQueue adapts the jobs store to DeployQueue for the action layer in tests that
// run the jobs themselves.
type storeQueue struct{ js *jobs.Store }

func (q storeQueue) EnqueueDeploy(ctx context.Context, n jobs.NewDeploy) (jobs.Enqueued, error) {
	return q.js.EnqueueDeploy(ctx, n)
}
func (q storeQueue) Enqueue(ctx context.Context, n jobs.NewJob) (int64, error) {
	return q.js.Enqueue(ctx, n)
}
func (q storeQueue) EnqueueDelete(ctx context.Context, n jobs.NewDelete) (jobs.Deleted, error) {
	return q.js.EnqueueDelete(ctx, n)
}
func (q storeQueue) CancelDeploy(context.Context, int64, string, []string) (string, error) {
	return "", errors.New("not used")
}

// TestRollbackRedeployAndRestartMakeZeroCaddyLoads: the real chain, service actions
// included. Redeploy (new container, new address), rollback (an earlier image pinned
// by id) and restart (same container, same address) never load Caddy (exact
// caddy.ActiveContext check); only table swaps are sent, and the app answers from
// the right container after each.
func TestRollbackRedeployAndRestartMakeZeroCaddyLoads(t *testing.T) {
	g, ref := newSwitchRig(t)
	ctx := g.ctx
	x := newITRunner(t, g, nil)
	port := itFreePort(t)
	mustExec(t, x.st, `UPDATE services SET public_port = ?, container_port = 8080, stop_grace_s = 1, strategy = 'stop_first' WHERE id = ?`, port, x.svc)
	if err := x.rt.ApplyRoutes(ctx, func() error { x.rt.state.replace(nil); return nil }); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	al, err := audit.New(x.st, log)
	if err != nil {
		t.Fatal(err)
	}
	reg := actions.New(al, log)
	RegisterServiceActions(reg, ServiceDeps{DB: x.st, Routes: x.rt, Jobs: storeQueue{x.js}, Log: log})
	// An admin token: the fixture service's image_ref is not the test image's repository,
	// and a write token may not roll back across repositories (that is tested apart).
	writer := actions.TokenActor("1", actions.ScopeAdmin)
	invoke := func(name string, in any) any {
		t.Helper()
		raw, _ := json.Marshal(in)
		res, err := reg.Invoke(ctx, writer, name, raw, actions.Options{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res.Out
	}
	var finished func()
	runQueued := func() int64 { // picks and runs the one queued job
		t.Helper()
		j, ok, err := x.js.Pick(ctx, "it-boot")
		if err != nil || !ok {
			t.Fatalf("pick: %v %v", ok, err)
		}
		switch j.Kind {
		case jobs.KindRestart:
			if _, err := x.r.runRestart(ctx, actions.Call{}, j.ID); err != nil {
				t.Fatalf("restart: %v", err)
			}
		default:
			if _, err := x.r.runDeploy(ctx, actions.Call{}, j.ID); err != nil {
				t.Fatalf("deploy: %v", err)
			}
		}
		finished()
		return j.ID
	}
	snap := Snapshot{V: 1, ImageRef: ref, ContainerPort: 8080, Health: SnapHealth{Kind: "http", Path: "/cgi-bin/id", TimeoutS: 30, Passes: 2},
		Strategy: "stop_first", DrainS: 10, StopGraceS: 1}
	finished = func() {
		mustExec(t, x.st, `UPDATE jobs SET state = 'succeeded', finished_at = 'x' WHERE state = 'running'`)
	} // x.deploy runs the job itself
	d1, err := x.deploy(t, snap) // the first deploy: it may touch the routes (the service had none)
	if err != nil {
		t.Fatal(err)
	}
	finished()
	if _, err := x.deploy(t, snap); err != nil { // deploy 2: a second version to roll back from
		t.Fatal(err)
	}
	finished()
	answer := func(want []string) string {
		t.Helper()
		var got string
		for try := 0; try < 50; try++ {
			if resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/cgi-bin/id"); err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				got = strings.TrimSpace(string(b))
				if resp.StatusCode == 200 {
					for _, w := range want {
						if got == "app-"+w[:12] {
							return got
						}
					}
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("the app answered %q, want one of the containers %v", got, want)
		return ""
	}
	activeIDs := func() []string {
		var a int64
		x.st.R.QueryRow(`SELECT active_deploy_id FROM services WHERE id = ?`, x.svc).Scan(&a)
		return x.idsOfDeploy(t, a)
	}

	ctxBefore := caddy.ActiveContext()
	applyBefore := x.cc.n("proxy_apply")

	// Rollback to deploy #1, pinned to its image id.
	out := invoke("services.rollback", m{"service": "p/web", "to": 1}).(DeployOut)
	runQueued()
	ids := activeIDs()
	if len(ids) != 1 {
		t.Fatalf("after the rollback: %v", ids)
	}
	answer(ids)
	var kind string
	var imageID *string
	x.st.R.QueryRow(`SELECT kind, image_id FROM deploys WHERE service_id = ? AND number = ?`, x.svc, out.Deploy).Scan(&kind, &imageID)
	var d1Image *string
	x.st.R.QueryRow(`SELECT image_id FROM deploys WHERE id = ?`, d1).Scan(&d1Image)
	if kind != "rollback" || imageID == nil || d1Image == nil || *imageID != *d1Image {
		t.Errorf("rollback row: %s %v (deploy 1: %v)", kind, imageID, d1Image)
	}

	// Redeploy: a new container at a new address.
	before := ids[0]
	invoke("services.redeploy", m{"service": "p/web"})
	runQueued()
	ids = activeIDs()
	if len(ids) != 1 || ids[0] == before {
		t.Fatalf("after the redeploy: %v (was %s)", ids, before)
	}
	answer(ids)

	// Restart: the same container answers again, from the same address.
	invoke("services.restart", m{"service": "p/web"})
	same := activeIDs()
	runQueued()
	if got := activeIDs(); len(got) != 1 || got[0] != same[0] {
		t.Fatalf("a restart changed the container: %v -> %v", same, got)
	}
	answer(same)

	if caddyLoaded(ctxBefore) {
		t.Error("Caddy loaded a config during the rollback, the redeploy or the restart")
	}
	// Per stop-first deploy: the page and the upstream; the restart sends nothing.
	if n := x.cc.n("proxy_apply") - applyBefore; n != 4 {
		t.Errorf("%d proxy_apply calls for a rollback and a redeploy (page + upstream each) and a restart (none), want 4", n)
	}
	t.Logf("RESULT rollback+redeploy+restart: Caddy loads %v; proxy_apply %d", caddyLoaded(ctxBefore), x.cc.n("proxy_apply")-applyBefore)
}
