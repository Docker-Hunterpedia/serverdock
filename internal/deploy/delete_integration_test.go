//go:build integration

package deploy

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

// TestDeleteRemovesOnlyLabelledContainers: the real chain (Docker, agent ops, Caddy).
// A service with a public port has a container of this install; next to it run a
// look-alike (the same service and deploy labels, ANOTHER instance label), a plain
// container with no labels, and another service's container. The delete removes the
// first only; the others keep running. The service's rows, its deploy-log directory
// and its route go (deleting an exposed service is a port-set change, so Caddy
// reloads: one of the documented reload cases, logged here, not asserted away).
func TestDeleteRemovesOnlyLabelledContainers(t *testing.T) {
	g, ref := newSwitchRig(t)
	ctx := g.ctx
	logs := t.TempDir()
	x := newITRunner(t, g, func(c *RunnerConfig) { c.LogRoot = logs })
	base := g.subnet[:strings.LastIndex(g.subnet, ".")]
	ipOurs, ipLook, ipOther := base+".2", base+".3", base+".4"

	svcOther := addService(t, x.st, "other")
	dOurs := addLiveDeploy(t, x.st, x.svc, 1, ipOurs, Snapshot{V: 1, ImageRef: ref, ContainerPort: 8080, Health: SnapHealth{Kind: "tcp", TimeoutS: 5, Passes: 1},
		Strategy: "blue_green", DrainS: 10, StopGraceS: 1})
	dOther := addDeploy(t, x.st, svcOther, 1, "succeeded", ipOther)
	g.startApp(t, ref, "OURS", ipOurs, itOwnedLabels(1, x.svc, dOurs)...)
	// The look-alike: every label of ours, but another install's instance id.
	g.startApp(t, ref, "LOOK", ipLook, append(itOwnedLabels(1, x.svc, dOurs), "--label", "serverdock.instance=ffffffffffffffffffffffffffffffff")...)
	g.startApp(t, ref, "OTHER", ipOther, itOwnedLabels(1, svcOther, dOther)...)
	plain := itDocker(t, "run", "-d", "--name", "sd-it-plain-"+g.inst[:8], "--label", "serverdock.service="+strconv.FormatInt(x.svc, 10), "--entrypoint", "sleep", ref, "300")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "sd-it-plain-"+g.inst[:8]).Run() })
	ours, look, other := g.containerID(t, "OURS"), g.containerID(t, "LOOK"), g.containerID(t, "OTHER")

	port := itFreePort(t)
	mustExec(t, x.st, `UPDATE services SET public_port = ?, container_port = 8080, active_deploy_id = ? WHERE id = ?`, port, dOurs, x.svc)
	if err := x.rt.ApplyRoutes(ctx, func() error {
		x.rt.state.replace([]Container{{ID: ours, Service: x.svc, Deploy: dOurs, State: StateRunning, IP: ipOurs}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(logs, strconv.FormatInt(x.svc, 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1.log.gz"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/cgi-bin/id"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("the app does not answer before the delete: %v", err)
	} else {
		resp.Body.Close()
	}

	out, err := x.js.EnqueueDelete(ctx, jobs.NewDelete{ServiceID: x.svc, Target: "p/web", Actor: jobs.ActorOf(actions.TokenActor("1", actions.ScopeWrite)), StopGraceS: 1})
	if err != nil {
		t.Fatal(err)
	}
	j, ok, err := x.js.Pick(ctx, "it-boot")
	if err != nil || !ok || j.ID != out.JobID {
		t.Fatalf("pick: %v %v", ok, err)
	}
	ctxBefore := caddy.ActiveContext()
	res, err := x.r.runDelete(ctx, actions.Call{}, j.ID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	t.Logf("RESULT delete of an exposed service: containers removed %d; Caddy loaded (a port-set change, allowed) %v", res.Containers, caddyLoaded(ctxBefore))
	if res.Containers != 1 || res.Status != "deleted" {
		t.Errorf("%+v", res)
	}
	if itDocker(t, "ps", "-a", "-q", "--filter", "id="+ours) != "" {
		t.Error("the service's container is still there")
	}
	for name, id := range map[string]string{"the look-alike (another instance)": look, "another service's container": other, "the plain container": plain} {
		if itDocker(t, "ps", "-q", "--filter", "id="+id) == "" {
			t.Errorf("%s was removed", name)
		}
	}
	var n int
	x.st.R.QueryRow(`SELECT (SELECT COUNT(*) FROM services WHERE id = ?) + (SELECT COUNT(*) FROM deploys WHERE service_id = ?)`, x.svc, x.svc).Scan(&n)
	if n != 0 {
		t.Error("the service's rows are still there")
	}
	var others int
	x.st.R.QueryRow(`SELECT COUNT(*) FROM services WHERE id = ?`, svcOther).Scan(&others)
	if others != 1 {
		t.Error("the other service's row went")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the deploy-log directory is still there: %v", err)
	}
	// The route is gone: the port no longer answers (Caddy reloaded without it).
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/cgi-bin/id")
		if err != nil {
			break
		}
		resp.Body.Close()
		if time.Now().After(deadline) {
			t.Fatal("the deleted service's port still answers")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
