package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/deploy"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

// noDocker is a Docker socket that does not exist: the server runs, the boot
// cannot list containers, and so the scheduler's start gate stays closed.
func noDocker(t *testing.T) string { return filepath.Join(t.TempDir(), "no-docker.sock") }

// seedSession writes the admin user (once) and a session for it, as login would,
// and returns the session's public id. Sessions are what a job's actor_session
// names; enqueue refuses an actor whose session row does not exist.
func seedSession(t *testing.T, addrs serverAddrs, public string) string {
	t.Helper()
	if _, err := addrs.db.W.Exec(`INSERT OR IGNORE INTO users(id, username, scope, created_at) VALUES (1, 'admin', 'admin', 1)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := addrs.db.W.Exec(`INSERT INTO sessions(id_hash, public_id, user_id, created_at, last_seen_at, idle_expires_at, abs_expires_at, mfa_fresh_until, ip_key, user_agent)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?, 'ip', 'ua')`, []byte(public + strings.Repeat("x", 32))[:32], public, now, now, now+3600, now+3600, now+3600); err != nil {
		t.Fatal(err)
	}
	return public
}

func sessionActor(public string) actions.Actor {
	return actions.UserSessionActor("1", public, actions.ScopeAdmin, time.Now().Add(time.Hour))
}

// deployAs creates a service with deploy:true as the session's actor (through the
// real registry of the real server) and returns the queued job's id.
func deployAs(t *testing.T, addrs serverAddrs, public, name string) int64 {
	t.Helper()
	in := json.RawMessage(`{"project":"p","name":"` + name + `","image_ref":"nginx:1","deploy":true}`)
	res, err := addrs.reg.Invoke(context.Background(), sessionActor(public), "services.create", in, actions.Options{})
	if err != nil {
		t.Fatalf("services.create deploy:true: %v", err)
	}
	out := res.Out.(deploy.ServiceChangeOut)
	if out.Deploy == nil || out.Deploy.Job == 0 || out.Deploy.Status != jobs.StateQueued {
		t.Fatalf("no queued deploy: %+v", out)
	}
	return out.Deploy.Job
}

func jobState(t *testing.T, addrs serverAddrs, id int64) string {
	t.Helper()
	j, err := addrs.jobStore.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j.State
}

func revoke(t *testing.T, addrs serverAddrs, caller, body string) {
	t.Helper()
	if _, err := addrs.reg.Invoke(context.Background(), sessionActor(caller), "auth.sessions_revoke", json.RawMessage(body), actions.Options{}); err != nil {
		t.Fatalf("auth.sessions_revoke %s: %v", body, err)
	}
}

// The real server's wiring (plan 0004 Revisions 6 and 7): ActionDeps.Jobs is the
// real scheduler, so revoking a session cancels the jobs it queued. The start gate
// is closed (no Docker), so the jobs are queued, not running; the scheduler is
// running, though, and a cancel through it ends the row.
func TestRealServerSessionsRevokeCancelsThatSessionsQueuedJobs(t *testing.T) {
	addrs, stop := startServer(t, serverConfig{Data: newData(t), ControlSocket: shortSock(t), DockerSocket: noDocker(t)})
	defer func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	}()
	seedSession(t, addrs, "sess-caller")
	seedSession(t, addrs, "sess-b")
	seedSession(t, addrs, "sess-c")
	mine := deployAs(t, addrs, "sess-caller", "mine")
	jb := deployAs(t, addrs, "sess-b", "b")
	jc := deployAs(t, addrs, "sess-c", "c")
	for _, id := range []int64{mine, jb, jc} {
		if s := jobState(t, addrs, id); s != jobs.StateQueued {
			t.Fatalf("job %d is %s, want queued (the gate is closed)", id, s)
		}
	}

	// One session.
	revoke(t, addrs, "sess-caller", `{"public_id":"sess-b"}`)
	if s := jobState(t, addrs, jb); s != jobs.StateCancelled {
		t.Errorf("the revoked session's job is %s, want cancelled", s)
	}
	j, _ := addrs.jobStore.Get(context.Background(), jb)
	if !strings.Contains(j.CancelReason, "session that queued this job was revoked") {
		t.Errorf("cancel reason %q", j.CancelReason)
	}
	if s := jobState(t, addrs, mine); s != jobs.StateQueued {
		t.Errorf("the caller's own job is %s", s)
	}
	if s := jobState(t, addrs, jc); s != jobs.StateQueued {
		t.Errorf("another session's job is %s", s)
	}
	// "All others".
	revoke(t, addrs, "sess-caller", `{"all_others":true}`)
	if s := jobState(t, addrs, jc); s != jobs.StateCancelled {
		t.Errorf("job of a session revoked by all_others is %s", s)
	}
	if s := jobState(t, addrs, mine); s != jobs.StateQueued {
		t.Errorf("the caller's own job is %s after all_others", s)
	}
	// The deploy rows of the cancelled jobs ended with them.
	for _, id := range []int64{jb, jc} {
		var status string
		if err := addrs.db.R.QueryRow(`SELECT status FROM deploys WHERE job_id = ?`, id).Scan(&status); err != nil || status != "cancelled" {
			t.Errorf("deploy of job %d: %q %v", id, status, err)
		}
	}
}

// A signed-out session cannot queue: the enqueue is refused with the plain message
// (plan 0004 Revision 8), through the real server, for both deploy entry points.
func TestRealServerQueuingFromASignedOutSessionSaysSignInAgain(t *testing.T) {
	addrs, stop := startServer(t, serverConfig{Data: newData(t), ControlSocket: shortSock(t), DockerSocket: noDocker(t)})
	defer stop()
	seedSession(t, addrs, "sess-gone")
	if _, err := addrs.db.W.Exec(`DELETE FROM sessions WHERE public_id = 'sess-gone'`); err != nil {
		t.Fatal(err)
	}
	in := json.RawMessage(`{"project":"p","name":"web","image_ref":"nginx:1","deploy":true}`)
	_, err := addrs.reg.Invoke(context.Background(), sessionActor("sess-gone"), "services.create", in, actions.Options{})
	if err == nil || !strings.Contains(err.Error(), "your session was signed out; sign in again") {
		t.Fatalf("create deploy:true: %v", err)
	}
}

// The scheduler dispatches nothing until the reconciler's first resync has finished
// (the start gate), then picks the job up.
func TestRealServerSchedulerStartsOnlyAfterTheBootSucceeds(t *testing.T) {
	sock, fe := startFakeEngine(t, `[]`)
	fe.listFails.Store(true)
	addrs, stop := startServer(t, serverConfig{Data: newData(t), ControlSocket: shortSock(t), DockerSocket: sock, Proxy: true,
		Listen: testbinFree(t), HTTPSListen: testbinFree(t), AppSubnet: "10.78.0.0/24"})
	defer stop()
	waitFor(t, "the boot attempt", func() bool { return indexOf(fe.sawPaths(), "GET /containers/json") >= 0 })
	seedSession(t, addrs, "sess-a")
	id := deployAs(t, addrs, "sess-a", "web")
	time.Sleep(200 * time.Millisecond)
	if s := jobState(t, addrs, id); s != jobs.StateQueued {
		t.Fatalf("the job is %s while the gate is closed", s)
	}
	fe.listFails.Store(false)
	if err := addrs.booter.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to leave the queue", func() bool { return jobState(t, addrs, id) != jobs.StateQueued })
}
