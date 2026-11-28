//go:build integration

package deploy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

// itReconciler is a reconciler over the real chain of an itRunner (Docker, the agent's
// ops and its events op over a hub session, the real embedded Caddy through Routes).
func (x *itRunner) reconciler(t *testing.T) *Reconciler {
	t.Helper()
	rec := NewReconciler(ReconcilerConfig{DB: x.st, Ops: x.cl, Routes: x.rt, Locks: x.r.cfg.Locks, Act: x.r.cfg.Inv, Proxy: true})
	RegisterReconcilerActions(x.reg, rec)
	return rec
}

func (x *itRunner) runReconciler(t *testing.T, rec *Reconciler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rec.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

// TestReconcilerEventsDuringDeploysReloadNothing: with the watcher running, two real
// blue-green deploys of an exposed service (every container event of the candidate, the
// old version and their removal goes through the reconciler's handler) cause no Caddy
// load: the events only ever swap the route table, and the app answers 2xx afterwards.
func TestReconcilerEventsDuringDeploysReloadNothing(t *testing.T) {
	g, ref := newSwitchRig(t)
	x := newITRunner(t, g, nil)
	port := itFreePort(t)
	mustExec(t, x.st, `UPDATE services SET public_port = ?, container_port = 8080 WHERE id = ?`, port, x.svc)
	rec := x.reconciler(t)
	x.runReconciler(t, rec)
	deadline := time.Now().Add(20 * time.Second)
	for !rec.Up() {
		if time.Now().After(deadline) {
			t.Fatal("the reconciler did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	snap := Snapshot{V: 1, ImageRef: ref, ContainerPort: 8080, Health: SnapHealth{Kind: "tcp", TimeoutS: 30, Passes: 1}, Strategy: "blue_green", DrainS: 10, StopGraceS: 2}
	ctxBefore := caddy.ActiveContext()
	var last int64
	for i := 0; i < 2; i++ {
		d, err := x.deploy(t, snap)
		if err != nil {
			t.Fatalf("deploy %d: %v", i+1, err)
		}
		if status, code, reason, _ := x.row(t, d); status != "succeeded" {
			t.Fatalf("deploy %d: %s %s %q", i+1, status, code, reason)
		}
		last = d
		// The helper runs the job without the scheduler, which would record its end.
		mustExec(t, x.st, `UPDATE jobs SET state = 'succeeded' WHERE state = 'running'`)
	}
	// Let the watcher see the last events (the old container's removal).
	time.Sleep(500 * time.Millisecond)
	if caddyLoaded(ctxBefore) {
		t.Error("Caddy loaded a config while the watcher handled the deploys' container events")
	}
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/cgi-bin/id")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 || !strings.HasPrefix(string(b), "app-") {
		t.Errorf("the app answers %d %q", resp.StatusCode, b)
	}
	// The watcher's state map holds the live container, and only that one.
	n := 0
	for _, c := range x.rt.State().snapshot() {
		if c.Service == x.svc {
			n++
			if c.Deploy != last || c.State != StateRunning {
				t.Errorf("state map entry %+v, want the last deploy (%d) running", c, last)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d containers of the service in the state map, want 1", n)
	}
}

// TestOrphanRemovalRemovesOnlyAnonymousVolumes: the reconciler removes an orphan
// container of this install with the volumes Docker made for it (anonymous), and a
// named volume somebody attached by hand survives.
func TestOrphanRemovalRemovesOnlyAnonymousVolumes(t *testing.T) {
	g, ref := newSwitchRig(t)
	x := newITRunner(t, g, nil)
	base := netip.MustParsePrefix(g.subnet).Addr().As4()
	ip := func(n byte) string { return netip.AddrFrom4([4]byte{base[0], base[1], base[2], n}).String() }
	live := addDeploy(t, x.st, x.svc, 1, "succeeded", ip(2))
	orphan := addDeploy(t, x.st, x.svc, 2, "failed", ip(3))
	mustExec(t, x.st, `UPDATE services SET container_port = 8080, active_deploy_id = ? WHERE id = ?`, live, x.svc)
	named := "sd-it-named-" + g.inst[:8]
	g.startApp(t, ref, "LIVE", ip(2), itOwnedLabels(1, x.svc, live)...)
	g.startApp(t, ref, "ORPH", ip(3), append(itOwnedLabels(1, x.svc, orphan), "-v", "/anonymous-data", "-v", named+":/named-data")...)
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", named).Run() })
	orphID := g.containerID(t, "ORPH")
	mounts := itDocker(t, "inspect", "-f", `{{range .Mounts}}{{.Name}}{{"\n"}}{{end}}`, orphID)
	var anon string
	for _, m := range strings.Fields(mounts) {
		if m != named {
			anon = m
		}
	}
	if anon == "" {
		t.Fatalf("no anonymous volume among %q", mounts)
	}

	rec := x.reconciler(t)
	if _, err := rec.fullResync(g.ctx, true, nil); err != nil {
		t.Fatal(err)
	}
	if itDocker(t, "ps", "-aq", "--filter", "id="+orphID) != "" {
		t.Error("the orphan container is still there")
	}
	if itDocker(t, "ps", "-q", "--filter", "id="+g.containerID(t, "LIVE")) == "" {
		t.Error("the live version's container was removed")
	}
	vols := itDocker(t, "volume", "ls", "-q")
	if strings.Contains(vols, anon) {
		t.Errorf("the anonymous volume %s survived the removal", anon)
	}
	if !strings.Contains(vols, named) {
		t.Errorf("the named volume %s was removed", named)
	}
}

// TestDaemonRestartKeepsRoutesCorrect: two exposed services, then the Docker daemon is
// restarted (`sudo systemctl restart docker`, or SD_TEST_DOCKER_RESTART_CMD). The
// event stream drops and the containers are stopped by the daemon; the reconciler
// reconnects, its resync starts them again, and within 30 s each public port answers
// 2xx from its own service again, never from the other.
//
// It needs a Docker daemon that the test may restart, so it runs only where
// SD_TEST_DOCKER_RESTART=1 is set: the CI integration job sets it, a developer's
// machine does not.
func TestDaemonRestartKeepsRoutesCorrect(t *testing.T) {
	if os.Getenv("SD_TEST_DOCKER_RESTART") != "1" {
		t.Skip("SD_TEST_DOCKER_RESTART=1 is not set: this test restarts the Docker daemon (CI only)")
	}
	restart := os.Getenv("SD_TEST_DOCKER_RESTART_CMD")
	if restart == "" {
		restart = "sudo systemctl restart docker"
	}
	g, ref := newSwitchRig(t)
	x := newITRunner(t, g, nil)
	base := netip.MustParsePrefix(g.subnet).Addr().As4()
	ip := func(n byte) string { return netip.AddrFrom4([4]byte{base[0], base[1], base[2], n}).String() }
	svcB := addService(t, x.st, "second")
	dA := addDeploy(t, x.st, x.svc, 1, "succeeded", ip(2))
	dB := addDeploy(t, x.st, svcB, 1, "succeeded", ip(3))
	portA, portB := itFreePort(t), itFreePort(t)
	mustExec(t, x.st, `UPDATE services SET public_port = ?, container_port = 8080, active_deploy_id = ? WHERE id = ?`, portA, dA, x.svc)
	mustExec(t, x.st, `UPDATE services SET public_port = ?, container_port = 8080, active_deploy_id = ? WHERE id = ?`, portB, dB, svcB)
	g.startApp(t, ref, "A", ip(2), itOwnedLabels(1, x.svc, dA)...)
	g.startApp(t, ref, "B", ip(3), itOwnedLabels(1, svcB, dB)...)

	rec := x.reconciler(t)
	x.runReconciler(t, rec)
	get := func(port int) (string, error) {
		cl := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
		resp, err := cl.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/cgi-bin/id")
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode/100 != 2 {
			return "", fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return strings.TrimSpace(string(b)), nil
	}
	waitBoth := func(what string, d time.Duration) {
		t.Helper()
		deadline := time.Now().Add(d)
		for {
			a, errA := get(portA)
			b, errB := get(portB)
			if errA == nil && errB == nil && a == "app-A" && b == "app-B" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: A answers %q (%v), B answers %q (%v)", what, a, errA, b, errB)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	waitBoth("before the restart", 30*time.Second)

	// Watch both ports for a wrong answer during the whole restart.
	var wrong []string
	var mu sync.Mutex
	stop := make(chan struct{})
	var watch sync.WaitGroup
	for port, want := range map[int]string{portA: "app-A", portB: "app-B"} {
		watch.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if got, err := get(port); err == nil && got != want {
					mu.Lock()
					wrong = append(wrong, fmt.Sprintf("port %d answered %q, want %q", port, got, want))
					mu.Unlock()
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
	t0 := time.Now()
	if out, err := exec.Command("sh", "-c", restart).CombinedOutput(); err != nil {
		close(stop)
		watch.Wait()
		t.Fatalf("restarting Docker (%s): %v\n%s", restart, err, out)
	}
	waitBoth("after the Docker restart", 30*time.Second)
	took := time.Since(t0)
	close(stop)
	watch.Wait()
	t.Logf("RESULT Docker restart: both public ports answer from their own service again %v after the restart began", took.Round(100*time.Millisecond))
	mu.Lock()
	defer mu.Unlock()
	if len(wrong) > 0 {
		t.Errorf("an app answered from the wrong service: %v", wrong)
	}
}
