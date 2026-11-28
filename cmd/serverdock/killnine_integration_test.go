//go:build integration

package main

import (
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/deploy"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
	"github.com/Docker-Hunterpedia/serverdock/internal/testbin"
)

// The kill -9 suite (plan 0004 step 10): the real binary (built with -tags testhooks
// by internal/testbin), the real Docker and the local registry. For every blue-green
// and stop-first stage a deploy is paused at that stage boundary (a testhooks pause
// point writes a marker file and blocks), the server is killed with SIGKILL, and a
// fresh server is started on the same data directory and Docker:
//
//   - the app answers 2xx again, from the right version;
//   - an uncommitted deploy is failed "interrupted by restart" and its containers are
//     removed by the resync; a committed one is succeeded and its old container removed;
//   - a stop-first previous container that was stopped runs again;
//   - the jobs.run_deploy intent has exactly one outcome row, with the user's actor
//     and executor = system;
//   - the outage (SIGKILL to the first 2xx after the restart) is logged on a parseable
//     line: OUTAGE strategy=<s> stage=<name> ms=<n> (QM3a-9, docs/research/05).
//
// Needs Docker and SD_TEST_APP_IMAGE (the CI integration job has both).

// killSubnets hands every scenario its own app network.
var killSubnet atomic.Int32

// umaskMu serialises the umask change around starting a server (the umask is process-wide).
var umaskMu sync.Mutex

type killStage struct {
	pause     string // the testhooks pause point
	committed bool   // the deploy had committed when the server was killed
}

var blueGreenKillStages = []killStage{
	{"pull:before", false}, {"check:before", false}, {"start:before", false}, {"health:before", false}, {"switch:before", false},
	{"commit:before", false}, {"commit:after", true}, {"drain:before", true}, {"stop_old:before", true},
}

var stopFirstKillStages = []killStage{
	{"pull:before", false}, {"check:before", false}, {"maintenance:before", false}, {"stop_old_first:before", false}, {"start:before", false},
	{"start:after", false}, {"health:before", false}, {"switch:before", false}, {"commit:before", false}, {"commit:after", true}, {"stop_old:before", true},
}

func TestKillNineBlueGreen(t *testing.T) { runKillSuite(t, "blue_green", blueGreenKillStages) }
func TestKillNineStopFirst(t *testing.T) { runKillSuite(t, "stop_first", stopFirstKillStages) }

func runKillSuite(t *testing.T, strategy string, stages []killStage) {
	image := os.Getenv("SD_TEST_APP_IMAGE")
	if image == "" {
		t.Skip("SD_TEST_APP_IMAGE is not set")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker CLI")
	}
	sem := make(chan struct{}, 3) // three scenarios at a time
	for _, st := range stages {
		t.Run(strings.ReplaceAll(st.pause, ":", "-"), func(t *testing.T) {
			t.Parallel()
			sem <- struct{}{}
			defer func() { <-sem }()
			runKillScenario(t, image, strategy, st)
		})
	}
}

// ---- one scenario ----

type killRig struct {
	t                         *testing.T
	data                      string
	sock                      string
	listen                    string
	https                     string
	subnet                    string
	port                      int
	instance                  string
	svc                       int64
	image                     string
	project                   string
	strategy                  string
	args                      []string
	releaseServer, releaseApp func() // let go of the reserved ports
}

func (k *killRig) server(env ...string) *testbin.Proc {
	k.t.Helper()
	k.releaseServer()
	k.releaseApp()
	// Started with the most permissive umask: the files under deploy-logs/ must be
	// private whatever umask the service is started with.
	umaskMu.Lock()
	old := syscall.Umask(0)
	p := testbin.Start(k.t, testbin.Options{Args: k.args, Env: env})
	syscall.Umask(old)
	umaskMu.Unlock()
	k.t.Cleanup(func() {
		if k.t.Failed() {
			_ = p.Cmd.Process.Signal(syscall.SIGQUIT) // a goroutine dump, if it is still running
			time.Sleep(500 * time.Millisecond)
			k.t.Logf("server log:\n%s", p.Logs())
		}
	})
	return p
}

func runKillScenario(t *testing.T, image, strategy string, st killStage) {
	k := &killRig{t: t, image: image, strategy: strategy}
	base := testbin.ShortDir(t)
	k.data = filepath.Join(base, "data")
	k.sock = filepath.Join(base, "cp.sock")
	// The server's two listeners and the app's public port are three distinct ports,
	// held open until use. The server binds a seeded service's public port at start
	// (an exposure is opened at boot, not at the deploy's switch), so all three are
	// released together, just before the first server starts. Both reservations are
	// open at the same time, so they cannot overlap.
	srv, relSrv := testbin.ReserveAddrs(t, 2)
	app, relApp := testbin.ReserveAddrs(t, 1)
	k.listen, k.https = srv[0], srv[1]
	_, p, _ := net.SplitHostPort(app[0])
	k.port, _ = strconv.Atoi(p)
	k.releaseServer, k.releaseApp = relSrv, relApp
	n := int(killSubnet.Add(1))
	k.subnet = fmt.Sprintf("10.%d.%d.0/24", 231, n)
	k.project = fmt.Sprintf("kn%d", n) // container names are Docker-wide: a project name of its own per scenario
	k.args = []string{"server", "--data", k.data, "--control-socket", k.sock, "--listen", k.listen, "--https-listen", k.https,
		"--docker-socket", "/var/run/docker.sock", "--allow-loopback-registry", "--app-subnet", k.subnet}
	t.Cleanup(k.cleanup)

	// Phase 0: seed the data directory (the server is not running).
	k.seed()
	// Phase 1: a server deploys version 1 (deploy A) and is stopped cleanly.
	p1 := k.server()
	k.waitFor("deploy A to finish", 120*time.Second, func() bool {
		st := k.deployStatus(1)
		if st == "failed" {
			code, reason := k.deployReason(1)
			k.t.Fatalf("deploy A failed: %s: %s", code, reason)
		}
		return st == "succeeded"
	})
	aHost := k.answer(20 * time.Second)
	if !strings.HasPrefix(aHost, "app-") {
		t.Fatalf("version 1 answers %q", aHost)
	}
	if code := p1.Stop(); code != 0 {
		t.Fatalf("the first server stopped with %d:\n%s", code, p1.Logs())
	}
	aID := k.containerOf(1)
	if aID == "" {
		t.Fatal("no container for deploy A")
	}
	// Phase 2: deploy B is queued while the server is down; the next server pauses at the stage.
	k.enqueue(2)
	pauseDir := filepath.Join(base, "pause")
	if err := os.MkdirAll(pauseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p2 := k.server("SERVERDOCK_TESTHOOK_PAUSE="+st.pause, "SERVERDOCK_TESTHOOK_DIR="+pauseDir)
	marker := filepath.Join(pauseDir, "paused-"+strings.ReplaceAll(st.pause, ":", "-"))
	k.waitFor("the pause at "+st.pause, 120*time.Second, func() bool { _, err := os.Stat(marker); return err == nil })
	// The candidate's container, if the deploy got far enough to have one: its output
	// must be in deploy B's log after the restart (the resync keeps it before removal).
	bBefore := k.containerOf(2)
	// Phase 3: SIGKILL, then a fresh server; the clock starts at the kill.
	killedAt := time.Now()
	if err := p2.Cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := p2.Wait(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	var twoVersions func() []string
	if strategy == "stop_first" {
		twoVersions = k.watchTwoVersions()
	}
	p3 := k.server()
	got := k.answer(120 * time.Second)
	outage := time.Since(killedAt)
	t.Logf("OUTAGE strategy=%s stage=%s ms=%d", strategy, st.pause, outage.Milliseconds())
	if !strings.HasPrefix(got, "app-") {
		t.Fatalf("the app did not answer 2xx again after the restart (last: %q)\n%s", got, p3.Logs())
	}

	// Phase 4: what the resync and the boot pass settled.
	if st.committed {
		k.waitFor("deploy B succeeded", 60*time.Second, func() bool { return k.deployStatus(2) == "succeeded" })
		k.waitFor("the old container removed", 60*time.Second, func() bool { return k.containerOf(1) == "" })
		bID := k.containerOf(2)
		if bID == "" || !strings.Contains(got, bID[:12]) && !k.waitAnswer("app-"+bID[:12], 30*time.Second) {
			t.Fatalf("the committed version's container %q does not answer (got %q)", bID, got)
		}
		if k.activeDeploy() != k.deployID(2) {
			t.Errorf("the active deploy is %d, want deploy B (%d)", k.activeDeploy(), k.deployID(2))
		}
		// The old container's tail was kept before the resync removed it (step 11).
		if txt := k.deployLog(1); !strings.Contains(txt, "sd-testapp ready "+aID[:12]) || !strings.Contains(txt, "deploy: #1") {
			t.Errorf("deploy A's log does not hold the old container's output:\n%s", txt)
		}
	} else {
		k.waitFor("deploy B failed (interrupted)", 60*time.Second, func() bool { return k.deployStatus(2) == "failed" })
		if code, reason := k.deployReason(2); code != "interrupted" || !strings.Contains(reason, "interrupted by restart") {
			t.Errorf("deploy B ended %q %q", code, reason)
		}
		k.waitFor("deploy B's containers removed by the resync", 60*time.Second, func() bool { return k.containerOf(2) == "" })
		if k.containerOf(1) == "" {
			t.Fatal("the old version's container is gone")
		}
		k.waitFor("the old version answers", 60*time.Second, func() bool { return k.waitAnswer("app-"+aID[:12], 3*time.Second) })
		k.waitFor("the old container is running", 30*time.Second, func() bool { return k.state(aID) == "running" })
		if k.activeDeploy() != k.deployID(1) {
			t.Errorf("the active deploy is %d, want deploy A", k.activeDeploy())
		}
		k.waitFor("app_safe raised to yes", 30*time.Second, func() bool { return k.appSafe(2) == "yes" })
		// The uncommitted candidate's tail was kept before the resync removed it (step 11).
		if bBefore != "" {
			if txt := k.deployLog(2); !strings.Contains(txt, "sd-testapp ready "+bBefore[:12]) || !strings.Contains(txt, "deploy: #2") {
				t.Errorf("deploy B's log does not hold the candidate's output:\n%s", txt)
			}
		} else if k.hasLog(2) {
			t.Error("a log exists for a deploy that never had a container")
		}
	}
	if twoVersions != nil {
		if seen := twoVersions(); len(seen) > 0 {
			t.Errorf("two versions of a stop-first service ran at the same time: %v", seen)
		}
	}
	if n := k.jobState(2); n != map[bool]string{true: "succeeded", false: "failed"}[st.committed] {
		t.Errorf("deploy B's job is %s", n)
	}
	k.checkAudit()
}

// deployLog is the decompressed deploy log of deploy number n, read from the data
// directory of the built binary: the root, the service's directory and the file are
// private (0700, 0700, 0600), whatever umask the test runner has.
func (k *killRig) deployLog(number int) string {
	t := k.t
	t.Helper()
	id := k.deployID(number)
	root := filepath.Join(k.data, "deploy-logs")
	dir := filepath.Join(root, strconv.FormatInt(k.svc, 10))
	file := filepath.Join(dir, strconv.FormatInt(id, 10)+".log.gz")
	k.waitFor("the log of deploy "+strconv.Itoa(number), 30*time.Second, func() bool { _, err := os.Stat(file); return err == nil })
	for p, want := range map[string]os.FileMode{root: 0o700, dir: 0o700, file: 0o600} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s has mode %v, want %v", p, fi.Mode().Perm(), want)
		}
	}
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(io.LimitReader(zr, 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (k *killRig) hasLog(number int) bool {
	_, err := os.Stat(filepath.Join(k.data, "deploy-logs", strconv.FormatInt(k.svc, 10), strconv.FormatInt(k.deployID(number), 10)+".log.gz"))
	return err == nil
}

// watchTwoVersions samples docker ps from now until the returned function is called
// (it then returns what it saw): a stop-first service must never have containers of two
// deploys running at once, not even for a moment while the restarted server settles.
func (k *killRig) watchTwoVersions() func() []string {
	stop := make(chan struct{})
	done := make(chan []string, 1)
	go func() {
		var seen []string
		for {
			select {
			case <-stop:
				done <- seen
				return
			default:
			}
			out := k.docker("ps", "--filter", "label=serverdock.instance="+k.instance, "--filter", "status=running", "--format", `{{.ID}} {{.Label "serverdock.deploy"}}`)
			deploys := map[string]bool{}
			for _, l := range strings.Split(out, "\n") {
				if f := strings.Fields(l); len(f) == 2 {
					deploys[f[1]] = true
				}
			}
			if len(deploys) > 1 {
				seen = append(seen, strings.ReplaceAll(out, "\n", "; "))
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	var once sync.Once
	var res []string
	end := func() []string {
		once.Do(func() { close(stop); res = <-done })
		return res
	}
	k.t.Cleanup(func() { end() })
	return end
}

// ---- seeding and reading ----

func (k *killRig) seed() {
	t := k.t
	ctx := context.Background()
	if err := os.MkdirAll(k.data, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, k.data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().Unix()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		// An administrator as the runner's refresh wants one: a password and a confirmed authenticator, both older than the queued jobs.
		{`INSERT INTO users(id, username, scope, created_at, password_hash, password_changed_at, totp_confirmed_at) VALUES (1, 'admin', 'admin', 1, 'x', 1, 1)`, nil},
		{`INSERT INTO sessions(id_hash, public_id, user_id, created_at, last_seen_at, idle_expires_at, abs_expires_at, mfa_fresh_until, ip_key, user_agent)
			VALUES (?, 'killsess', 1, ?, ?, ?, ?, ?, 'ip', 'ua')`, []any{[]byte(strings.Repeat("k", 32)), now, now, now + 86400, now + 86400, now + 86400}},
		{`INSERT INTO projects(id, name, created_at) VALUES (1, ?, 'x')`, []any{k.project}},
		{`INSERT INTO services(id, project_id, name, image_ref, container_port, public_port, health_kind, health_timeout_s, health_passes, strategy, drain_s, stop_grace_s, created_at, updated_at)
			VALUES (1, 1, 'web', ?, 8080, ?, 'tcp', 30, 1, ?, 10, 2, 'x', 'x')`, []any{k.image, k.port, k.strategy}},
	} {
		if _, err := s.W.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	k.svc = 1
	if err := s.R.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'install_id'`).Scan(&k.instance); err != nil {
		t.Fatal(err)
	}
	k.enqueueWith(ctx, s, 1)
}

func (k *killRig) enqueueWith(ctx context.Context, s *store.Store, n int) {
	snap := deploy.Snapshot{V: 1, ImageRef: k.image, ContainerPort: 8080, Health: deploy.SnapHealth{Kind: "tcp", TimeoutS: 30, Passes: 1},
		Strategy: k.strategy, DrainS: 10, StopGraceS: 2}
	raw, err := snap.Marshal()
	if err != nil {
		k.t.Fatal(err)
	}
	actor := actions.UserSessionActor("1", "killsess", actions.ScopeAdmin, time.Now().Add(time.Hour))
	if _, err := jobs.NewStore(s, nil).EnqueueDeploy(ctx, jobs.NewDeploy{ServiceID: k.svc, Kind: "deploy", Snapshot: raw, Settings: snap.Settings(), Actor: jobs.ActorOf(actor)}); err != nil {
		k.t.Fatalf("queuing deploy %d: %v", n, err)
	}
}

func (k *killRig) enqueue(n int) {
	ctx := context.Background()
	s, err := store.Open(ctx, k.data)
	if err != nil {
		k.t.Fatal(err)
	}
	defer s.Close()
	k.enqueueWith(ctx, s, n)
}

func (k *killRig) scan(q string, args []any, dst ...any) bool {
	ro, err := store.OpenReadOnly(k.data, store.ReadOnlyOptions{})
	if err != nil {
		return false
	}
	defer ro.Close()
	return ro.DB.QueryRow(q, args...).Scan(dst...) == nil
}

func (k *killRig) deployStatus(number int) string {
	var s string
	k.scan(`SELECT status FROM deploys WHERE service_id = ? AND number = ?`, []any{k.svc, number}, &s)
	return s
}

func (k *killRig) deployID(number int) int64 {
	var id int64
	k.scan(`SELECT id FROM deploys WHERE service_id = ? AND number = ?`, []any{k.svc, number}, &id)
	return id
}

func (k *killRig) deployReason(number int) (code, reason string) {
	var c, r sql.NullString
	k.scan(`SELECT reason_code, reason FROM deploys WHERE service_id = ? AND number = ?`, []any{k.svc, number}, &c, &r)
	return c.String, r.String
}

func (k *killRig) appSafe(number int) string {
	var s sql.NullString
	k.scan(`SELECT app_safe FROM deploys WHERE service_id = ? AND number = ?`, []any{k.svc, number}, &s)
	return s.String
}

func (k *killRig) jobState(number int) string {
	var s string
	k.scan(`SELECT j.state FROM jobs j JOIN deploys d ON d.job_id = j.id WHERE d.service_id = ? AND d.number = ?`, []any{k.svc, number}, &s)
	return s
}

func (k *killRig) activeDeploy() int64 {
	var id sql.NullInt64
	k.scan(`SELECT active_deploy_id FROM services WHERE id = ?`, []any{k.svc}, &id)
	return id.Int64
}

// checkAudit: the jobs.run_deploy intent of deploy B has exactly one outcome row,
// with the user's actor and executor = system.
func (k *killRig) checkAudit() {
	t := k.t
	var intent int64
	var kind, id, exec string
	if !k.scan(`SELECT a.id, a.actor_kind, a.actor_id, COALESCE(a.executor, '') FROM audit_log a JOIN jobs j ON a.request_id = j.request_id
		JOIN deploys d ON d.job_id = j.id WHERE a.phase = 'intent' AND a.action = 'jobs.run_deploy' AND d.service_id = ? AND d.number = 2`, []any{k.svc}, &intent, &kind, &id, &exec) {
		t.Fatal("the intent of deploy B's run is not in the audit log")
	}
	ro, err := store.OpenReadOnly(k.data, store.ReadOnlyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	rows, err := ro.DB.Query(`SELECT actor_kind, actor_id, COALESCE(executor, ''), result FROM audit_log WHERE phase = 'outcome' AND intent_id = ?`, intent)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var ak, ai, ex, res string
		if err := rows.Scan(&ak, &ai, &ex, &res); err != nil {
			t.Fatal(err)
		}
		n++
		want := "error"
		if k.deployStatus(2) == "succeeded" {
			want = "ok"
		}
		if ak != "user" || ai != "1" || ex != "system" || res != want {
			t.Errorf("outcome row: actor %s/%s executor %q result %s (want user/1, system, %s)", ak, ai, ex, res, want)
		}
	}
	if n != 1 {
		t.Errorf("the intent has %d outcome rows, want exactly 1", n)
	}
}

// ---- docker and the app ----

func (k *killRig) docker(args ...string) string {
	out, _ := exec.Command("docker", args...).Output()
	return strings.TrimSpace(string(out))
}

// containerOf is the id of this install's container of deploy number n ("" if none).
func (k *killRig) containerOf(number int) string {
	id := k.deployID(number)
	if id == 0 {
		return ""
	}
	out := k.docker("ps", "-aq", "--no-trunc", "--filter", "label=serverdock.instance="+k.instance, "--filter", "label=serverdock.deploy="+strconv.FormatInt(id, 10))
	if f := strings.Fields(out); len(f) > 0 {
		return f[0]
	}
	return ""
}

func (k *killRig) state(id string) string {
	return k.docker("inspect", "-f", "{{.State.Status}}", id)
}

// answer polls the app until it answers 2xx and returns "app-<hostname>" ("" on timeout).
func (k *killRig) answer(d time.Duration) string {
	deadline := time.Now().Add(d)
	cl := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	url := "http://127.0.0.1:" + strconv.Itoa(k.port) + "/cgi-bin/id"
	last := ""
	for time.Now().Before(deadline) {
		resp, err := cl.Get(url)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return strings.TrimSpace(string(b))
			}
			last = fmt.Sprintf("HTTP %d", resp.StatusCode)
		} else {
			last = err.Error()
		}
		time.Sleep(25 * time.Millisecond)
	}
	return last
}

// waitAnswer is true when the app answers exactly want within d.
func (k *killRig) waitAnswer(want string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if k.answer(time.Second) == want {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func (k *killRig) waitFor(what string, d time.Duration, cond func() bool) {
	k.t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			k.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// cleanup removes this install's containers and its network.
func (k *killRig) cleanup() {
	if k.instance == "" {
		return
	}
	for _, id := range strings.Fields(k.docker("ps", "-aq", "--filter", "label=serverdock.instance="+k.instance)) {
		_ = exec.Command("docker", "rm", "-f", "-v", id).Run()
	}
	_ = exec.Command("docker", "network", "rm", agentops.NetworkName(k.instance)).Run()
	for _, n := range strings.Fields(k.docker("network", "ls", "-q", "--filter", "label=serverdock.instance="+k.instance)) {
		_ = exec.Command("docker", "network", "rm", n).Run()
	}
}
