package store_test

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"testing"

	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

const ts = "2026-11-22T10:00:00.000Z"

func openFresh(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustExec(t *testing.T, db interface {
	Exec(string, ...any) (sql.Result, error)
}, q string, args ...any) sql.Result {
	t.Helper()
	r, err := db.Exec(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return r
}

func insProject(t *testing.T, s *store.Store, name string) int64 {
	t.Helper()
	id, _ := mustExec(t, s.W, `INSERT INTO projects(name, created_at) VALUES (?, ?)`, name, ts).LastInsertId()
	return id
}

func insService(t *testing.T, s *store.Store, project int64, name string) int64 {
	t.Helper()
	id, _ := mustExec(t, s.W, `INSERT INTO services(project_id, name, image_ref, health_kind, health_timeout_s, health_passes, strategy,
		drain_s, stop_grace_s, created_at, updated_at) VALUES (?, ?, 'nginx:1', 'tcp', 60, 3, 'blue_green', 10, 10, ?, ?)`, project, name, ts, ts).LastInsertId()
	return id
}

func insJob(t *testing.T, s *store.Store, lock, state string) int64 {
	t.Helper()
	id, _ := mustExec(t, s.W, `INSERT INTO jobs(kind, lock_key, state, actor_kind, actor_id, actor_blob, enqueued_at, payload_json, run_after, timeout_s, created_at)
		VALUES ('deploy', ?, ?, 'user', '1', '{}', ?, '{}', ?, 100, ?)`, lock, state, ts, ts, ts).LastInsertId()
	return id
}

func insDeploy(t *testing.T, s *store.Store, service int64, number int, job any, status string, ip any) int64 {
	t.Helper()
	id, _ := mustExec(t, s.W, `INSERT INTO deploys(service_id, number, job_id, kind, snapshot_json, ip, status, actor_kind, actor_id, created_at)
		VALUES (?, ?, ?, 'deploy', '{"v":1}', ?, ?, 'user', '1', ?)`, service, number, job, ip, status, ts).LastInsertId()
	return id
}

func TestInstallIDCreatedByMigration(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := s.R.QueryRow(`SELECT value FROM meta WHERE key = 'install_id'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("install_id %q is not 16 random bytes in lowercase hex", id)
	}
	s.Close()
	// Stable across restarts, and two installs differ.
	s, err = store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var again string
	s.R.QueryRow(`SELECT value FROM meta WHERE key = 'install_id'`).Scan(&again)
	s.Close()
	if again != id {
		t.Errorf("install_id changed on reopen: %q then %q", id, again)
	}
	other := openFresh(t)
	var o string
	other.R.QueryRow(`SELECT value FROM meta WHERE key = 'install_id'`).Scan(&o)
	if o == id {
		t.Error("two installs share one install_id")
	}
}

func TestSnapshotIsImmutable(t *testing.T) {
	s := openFresh(t)
	svc := insService(t, s, insProject(t, s, "p"), "web")
	d := insDeploy(t, s, svc, 1, nil, "queued", nil)
	_, err := s.W.Exec(`UPDATE deploys SET snapshot_json = '{"v":2}' WHERE id = ?`, d)
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("snapshot update: %v", err)
	}
	// Other columns stay writable.
	mustExec(t, s.W, `UPDATE deploys SET status = 'running', stage = 'pull' WHERE id = ?`, d)
}

func TestDeploysIPLiveIndexRefusesASecondLiveDeployWithTheSameAddress(t *testing.T) {
	s := openFresh(t)
	p := insProject(t, s, "p")
	a, b := insService(t, s, p, "a"), insService(t, s, p, "b")
	d1 := insDeploy(t, s, a, 1, nil, "running", "10.250.0.2")
	if _, err := s.W.Exec(`INSERT INTO deploys(service_id, number, kind, snapshot_json, ip, status, actor_kind, actor_id, created_at)
		VALUES (?, 1, 'deploy', '{}', '10.250.0.2', 'queued', 'user', '1', ?)`, b, ts); err == nil {
		t.Fatal("a second live deploy took the same address")
	}
	// A finished deploy no longer holds the index (the allocator's held rule covers
	// its container), and NULL addresses never collide.
	mustExec(t, s.W, `UPDATE deploys SET status = 'succeeded' WHERE id = ?`, d1)
	insDeploy(t, s, b, 1, nil, "running", "10.250.0.2")
	insDeploy(t, s, a, 2, nil, "queued", nil)
	insDeploy(t, s, a, 3, nil, "queued", nil)
}

func TestIDsNeverReusedAfterDeletingTheNewestService(t *testing.T) {
	s := openFresh(t)
	p1, p2 := insProject(t, s, "one"), insProject(t, s, "two")
	s1, s2 := insService(t, s, p1, "web"), insService(t, s, p1, "db")
	j1, j2 := insJob(t, s, "service:1", "succeeded"), insJob(t, s, "service:2", "succeeded")
	d1, d2 := insDeploy(t, s, s1, 1, j1, "succeeded", nil), insDeploy(t, s, s2, 1, j2, "succeeded", nil)
	// Delete the newest row of every table (the deploy goes with its service).
	mustExec(t, s.W, `DELETE FROM jobs WHERE id = ?`, j2)
	mustExec(t, s.W, `DELETE FROM services WHERE id = ?`, s2)
	mustExec(t, s.W, `DELETE FROM projects WHERE id = ?`, p2)
	var n int
	s.R.QueryRow(`SELECT count(*) FROM deploys WHERE id = ?`, d2).Scan(&n)
	if n != 0 {
		t.Fatal("deleting a service must delete its deploys")
	}
	np := insProject(t, s, "three")
	ns := insService(t, s, p1, "cache")
	nj := insJob(t, s, "service:3", "queued")
	nd := insDeploy(t, s, ns, 1, nj, "queued", nil)
	for _, c := range []struct {
		table    string
		old, new int64
	}{{"projects", p2, np}, {"services", s2, ns}, {"jobs", j2, nj}, {"deploys", d2, nd}} {
		if c.new <= c.old {
			t.Errorf("%s: id %d was reused after deleting id %d", c.table, c.new, c.old)
		}
	}
	_ = d1
}

// Every foreign key states its delete rule, and each one behaves as the plan says.
func TestForeignKeyDeleteRules(t *testing.T) {
	s := openFresh(t)
	p := insProject(t, s, "p")
	svc := insService(t, s, p, "web")
	j := insJob(t, s, "service:1", "succeeded")
	d := insDeploy(t, s, svc, 1, j, "succeeded", nil)
	mustExec(t, s.W, `INSERT INTO deploy_stages(deploy_id, stage, state) VALUES (?, 'pull', 'ok')`, d)
	mustExec(t, s.W, `UPDATE services SET active_deploy_id = ? WHERE id = ?`, d, svc)

	// projects RESTRICT: a project with a service cannot be deleted.
	if _, err := s.W.Exec(`DELETE FROM projects WHERE id = ?`, p); err == nil {
		t.Error("deleting a project with services must be refused")
	}
	// jobs: the deploy keeps its row with job_id NULL.
	mustExec(t, s.W, `DELETE FROM jobs WHERE id = ?`, j)
	var job sql.NullInt64
	if err := s.R.QueryRow(`SELECT job_id FROM deploys WHERE id = ?`, d).Scan(&job); err != nil || job.Valid {
		t.Errorf("deploys.job_id after the job was deleted: %v %v", job, err)
	}
	// deploys: active_deploy_id SET NULL, stages cascade.
	mustExec(t, s.W, `DELETE FROM deploys WHERE id = ?`, d)
	var active sql.NullInt64
	if err := s.R.QueryRow(`SELECT active_deploy_id FROM services WHERE id = ?`, svc).Scan(&active); err != nil || active.Valid {
		t.Errorf("services.active_deploy_id after its deploy was deleted: %v %v", active, err)
	}
	var n int
	s.R.QueryRow(`SELECT count(*) FROM deploy_stages`).Scan(&n)
	if n != 0 {
		t.Error("deploy_stages did not cascade")
	}
	// services CASCADE to deploys.
	d2 := insDeploy(t, s, svc, 2, nil, "queued", nil)
	mustExec(t, s.W, `DELETE FROM services WHERE id = ?`, svc)
	s.R.QueryRow(`SELECT count(*) FROM deploys WHERE id = ?`, d2).Scan(&n)
	if n != 0 {
		t.Error("deploys did not cascade from services")
	}
}

func TestOneRunningJobPerLockKeyIndex(t *testing.T) {
	s := openFresh(t)
	insJob(t, s, "service:1", "running")
	if _, err := s.W.Exec(`INSERT INTO jobs(kind, lock_key, state, actor_kind, actor_id, actor_blob, enqueued_at, payload_json, run_after, timeout_s, created_at)
		VALUES ('deploy', 'service:1', 'running', 'user', '1', '{}', ?, '{}', ?, 1, ?)`, ts, ts, ts); err == nil {
		t.Fatal("two running jobs share a lock key")
	}
	insJob(t, s, "service:1", "queued") // queued rows may pile up
	insJob(t, s, "service:2", "running")
	if _, err := s.W.Exec(`UPDATE jobs SET state = 'unknown' WHERE lock_key = 'service:2'`); err == nil {
		t.Error("jobs.state accepted 'unknown'")
	}
}
