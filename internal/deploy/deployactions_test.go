package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

// withJobs gives the rig a real scheduler over its database (never started: the
// jobs stay queued, which is what the queuing tests look at).
func withJobs(d *ServiceDeps) {
	sched, err := jobs.NewScheduler(jobs.Config{Store: jobs.NewStore(d.DB, nil), Run: func(context.Context, jobs.Job) error { return nil }})
	if err != nil {
		panic(err)
	}
	d.Jobs = sched
}

func newDeployRig(t *testing.T) *svcRig { t.Helper(); return newSvcRig(t, withJobs) }

// seedSession writes the user and a session row, as login would (enqueue refuses
// an actor whose session does not exist).
func seedSession(t *testing.T, g *svcRig, public string) actions.Actor {
	t.Helper()
	mustExec(t, g.st, `INSERT OR IGNORE INTO users(id, username, scope, created_at) VALUES (1, 'admin', 'admin', 1)`)
	mustExec(t, g.st, `INSERT INTO sessions(id_hash, public_id, user_id, created_at, last_seen_at, idle_expires_at, abs_expires_at, mfa_fresh_until, ip_key, user_agent)
		VALUES (?, ?, 1, 1, 1, 9, 9, 9, 'ip', 'ua')`, []byte(strings.Repeat(public[:1], 32)), public)
	return actions.UserSessionActor("1", public, actions.ScopeAdmin, time.Now().Add(time.Hour))
}

func TestServicesDeployQueuesAJobAsTheCaller(t *testing.T) {
	g := newDeployRig(t)
	g.create("p", "web", m{"container_port": 80, "health": m{"kind": "http", "path": "/up"}})
	sess := seedSession(t, g, "sess-a")
	out, err := callAs[DeployOut](g, sess, "services.deploy", m{"service": "p/web"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Deploy != 1 || out.Job < 1 || out.Status != "queued" || out.Service != "p/web" {
		t.Fatalf("%+v", out)
	}
	var kind, status, actorKind, actorID, snap string
	var actorSession string
	if err := g.st.R.QueryRow(`SELECT j.kind, d.status, j.actor_kind, j.actor_id, COALESCE(j.actor_session, ''), d.snapshot_json
		FROM jobs j JOIN deploys d ON d.job_id = j.id WHERE j.id = ?`, out.Job).Scan(&kind, &status, &actorKind, &actorID, &actorSession, &snap); err != nil {
		t.Fatal(err)
	}
	if kind != "deploy" || status != "queued" || actorKind != "user" || actorID != "1" || actorSession != "sess-a" {
		t.Errorf("rows: %s %s %s %s %s", kind, status, actorKind, actorID, actorSession)
	}
	s, err := ParseSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	if s.ImageRef != "docker.io/library/nginx:1.27" && s.ImageRef != "nginx:1.27" {
		t.Errorf("image %q", s.ImageRef)
	}
	if s.ContainerPort != 80 || s.Health.Kind != "http" || s.Health.Path != "/up" || s.Strategy != "blue_green" || s.DrainS != 10 {
		t.Errorf("snapshot %+v", s)
	}
	// The next deploy supersedes the queued one.
	out2, err := callAs[DeployOut](g, sess, "services.deploy", m{"service": "p/web"})
	if err != nil || out2.Deploy != 2 || out2.Superseded != 1 {
		t.Fatalf("%+v %v", out2, err)
	}
	var st1 string
	if err := g.st.R.QueryRow(`SELECT status FROM deploys WHERE number = 1`).Scan(&st1); err != nil || st1 != "superseded" {
		t.Errorf("deploy 1 is %q %v", st1, err)
	}
}

func TestServicesDeploySameRepositoryRule(t *testing.T) {
	g := newDeployRig(t)
	g.create("p", "web", nil)
	sess := seedSession(t, g, "sess-a")
	// Another repository (or registry): refused for everyone, with the pointer.
	for _, ref := range []string{"redis:7", "ghcr.io/x/nginx:1", "example/nginx:1"} {
		_, err := callAs[DeployOut](g, sess, "services.deploy", m{"service": "p/web", "image_ref": ref})
		wantKind(t, err, actions.ErrInvalidInput)
		if !strings.Contains(err.Error(), "services.set_source") {
			t.Errorf("%s: %v", ref, err)
		}
	}
	if countRows(t, g.st, "deploys") != 0 || countRows(t, g.st, "jobs") != 0 {
		t.Error("a refused deploy left rows")
	}
	// Another tag of the same repository is fine for a write token, and becomes the
	// service's desired image in the same transaction as the deploy.
	if _, err := callAs[DeployOut](g, writeTok, "services.deploy", m{"service": "p/web", "image_ref": "nginx:1.28"}); err != nil {
		t.Fatal(err)
	}
	if got := g.get("p/web").ImageRef; !strings.HasSuffix(got, "nginx:1.28") {
		t.Errorf("service image %q", got)
	}
	// A digest of it too; a plain deploy then keeps the new tag.
	if _, err := callAs[DeployOut](g, writeTok, "services.deploy", m{"service": "p/web"}); err != nil {
		t.Fatal(err)
	}
	var snap string
	if err := g.st.R.QueryRow(`SELECT snapshot_json FROM deploys ORDER BY id DESC LIMIT 1`).Scan(&snap); err != nil || !strings.Contains(snap, "nginx:1.28") {
		t.Errorf("the plain deploy went back to the old tag: %s %v", snap, err)
	}
	// Scopes: read is denied, write allowed.
	_, err := callAs[DeployOut](g, readTok, "services.deploy", m{"service": "p/web"})
	wantKind(t, err, actions.ErrDenied)
	// A bad reference and a missing service.
	_, err = callAs[DeployOut](g, writeTok, "services.deploy", m{"service": "p/web", "image_ref": "NOT a ref"})
	wantKind(t, err, actions.ErrInvalidInput)
	_, err = callAs[DeployOut](g, writeTok, "services.deploy", m{"service": "p/nope"})
	wantKind(t, err, actions.ErrNotFound)
}

func TestServicesDeployWithoutAQueueIsUnavailable(t *testing.T) {
	g := newSvcRig(t)
	g.create("p", "web", nil)
	_, err := callAs[DeployOut](g, writeTok, "services.deploy", m{"service": "p/web"})
	wantKind(t, err, actions.ErrUnavailable)
	_, err = callAs[ServiceChangeOut](g, adminActor(), "services.create", m{"project": "p", "name": "x", "image_ref": "nginx", "deploy": true})
	wantKind(t, err, actions.ErrUnavailable)
	if countRows(t, g.st, "services") != 1 {
		t.Error("a create that cannot deploy still created the service")
	}
}

// jobs.ErrActorSessionGone (the session was revoked or signed out between the
// authorisation and the enqueue) reaches the caller as plain words, for both
// entry points (plan 0004 Revisions 5 and 8).
func TestEnqueueMapsASignedOutSessionToSignInAgain(t *testing.T) {
	g := newDeployRig(t)
	g.create("p", "web", nil)
	gone := actions.UserSessionActor("1", "sess-gone", actions.ScopeAdmin, time.Now().Add(time.Hour)) // no sessions row
	_, err := callAs[DeployOut](g, gone, "services.deploy", m{"service": "p/web"})
	wantKind(t, err, actions.ErrDenied)
	if !strings.Contains(err.Error(), "your session was signed out; sign in again") {
		t.Errorf("services.deploy: %v", err)
	}
	if errors.Is(err, jobs.ErrInvalid) || strings.Contains(err.Error(), "jobs:") {
		t.Errorf("the internal text leaked: %v", err)
	}
	_, err = callAs[ServiceChangeOut](g, gone, "services.create", m{"project": "p", "name": "second", "image_ref": "nginx", "deploy": true})
	wantKind(t, err, actions.ErrDenied)
	if !strings.Contains(err.Error(), "your session was signed out; sign in again") {
		t.Errorf("services.create deploy:true: %v", err)
	}
	if countRows(t, g.st, "jobs") != 0 || countRows(t, g.st, "deploys") != 0 {
		t.Error("a refused enqueue left rows")
	}
}

func TestServiceCreateWithDeployTrueQueuesTheFirstDeploy(t *testing.T) {
	g := newDeployRig(t)
	sess := seedSession(t, g, "sess-a")
	out, err := callAs[ServiceChangeOut](g, sess, "services.create", m{"project": "p", "name": "web", "image_ref": "nginx:1", "container_port": 80, "deploy": true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Deploy == nil || out.Deploy.Deploy != 1 || out.Deploy.Job < 1 || out.Deploy.Status != "queued" || out.Service.Service != "p/web" {
		t.Fatalf("%+v", out)
	}
	// deploy:false (or absent) queues nothing.
	out, err = callAs[ServiceChangeOut](g, sess, "services.create", m{"project": "p", "name": "web2", "image_ref": "nginx:1", "deploy": false})
	if err != nil || out.Deploy != nil {
		t.Fatalf("%+v %v", out, err)
	}
	if countRows(t, g.st, "jobs") != 1 {
		t.Errorf("jobs: %d", countRows(t, g.st, "jobs"))
	}
}

func TestDeploysCancel(t *testing.T) {
	g := newDeployRig(t)
	g.create("p", "web", nil)
	sess := seedSession(t, g, "sess-a")
	q, err := callAs[DeployOut](g, sess, "services.deploy", m{"service": "p/web"})
	if err != nil {
		t.Fatal(err)
	}
	// A queued deploy is cancelled at once.
	out, err := callAs[CancelOut](g, writeTok, "deploys.cancel", m{"service": "p/web", "number": q.Deploy})
	if err != nil || out.Status != "cancelled" {
		t.Fatalf("%+v %v", out, err)
	}
	var st string
	if err := g.st.R.QueryRow(`SELECT status FROM deploys WHERE number = ?`, q.Deploy).Scan(&st); err != nil || st != "cancelled" {
		t.Errorf("deploy %q %v", st, err)
	}
	// A finished one is a conflict; an unknown one is not found; read is denied.
	_, err = callAs[CancelOut](g, writeTok, "deploys.cancel", m{"service": "p/web", "number": q.Deploy})
	wantKind(t, err, actions.ErrConflict)
	_, err = callAs[CancelOut](g, writeTok, "deploys.cancel", m{"service": "p/web", "number": 99})
	wantKind(t, err, actions.ErrNotFound)
	_, err = callAs[CancelOut](g, readTok, "deploys.cancel", m{"service": "p/web", "number": 1})
	wantKind(t, err, actions.ErrDenied)
	// A running deploy is flagged; one in switch or commit refuses.
	q2, _ := callAs[DeployOut](g, sess, "services.deploy", m{"service": "p/web"})
	mustExec(t, g.st, `UPDATE jobs SET state = 'running', boot_id = 'b', started_at = 'x' WHERE id = ?`, q2.Job)
	mustExec(t, g.st, `UPDATE deploys SET status = 'running', stage = 'health' WHERE number = ?`, q2.Deploy)
	out, err = callAs[CancelOut](g, writeTok, "deploys.cancel", m{"service": "p/web", "number": q2.Deploy})
	if err != nil || out.Status != "cancelling" {
		t.Fatalf("%+v %v", out, err)
	}
	var flagged int
	if err := g.st.R.QueryRow(`SELECT cancel_requested FROM jobs WHERE id = ?`, q2.Job).Scan(&flagged); err != nil || flagged != 1 {
		t.Errorf("the running job was not flagged: %d %v", flagged, err)
	}
	mustExec(t, g.st, `UPDATE deploys SET stage = 'switch' WHERE number = ?`, q2.Deploy)
	_, err = callAs[CancelOut](g, writeTok, "deploys.cancel", m{"service": "p/web", "number": q2.Deploy})
	wantKind(t, err, actions.ErrConflict)
	if !strings.Contains(err.Error(), "not_cancellable") {
		t.Errorf("%v", err)
	}
}

func TestCancellableStages(t *testing.T) {
	for _, s := range []string{StagePull, StageCheck, StageStart, StageHealth} {
		if !CancellableStage(s) {
			t.Errorf("%s must be cancellable", s)
		}
	}
	for _, s := range []string{StageSwitch, StageCommit, StageDrain, StageStopOld} {
		if CancellableStage(s) {
			t.Errorf("%s must not be cancellable", s)
		}
	}
}

func TestSnapshotRoundTripAndValidation(t *testing.T) {
	s := Snapshot{ImageRef: "nginx:1", ContainerPort: 80, Health: SnapHealth{Kind: "tcp", TimeoutS: 60, Passes: 3}, Strategy: "blue_green", DrainS: 10, StopGraceS: 10}
	raw, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseSnapshot(string(raw))
	if err != nil || got.ImageRef != "nginx:1" || got.V != SnapshotVersion {
		t.Fatalf("%+v %v", got, err)
	}
	for name, mod := range map[string]func(map[string]any){
		"unknown key":    func(m map[string]any) { m["extra"] = 1 },
		"version":        func(m map[string]any) { m["v"] = 3 },
		"bad ref":        func(m map[string]any) { m["image_ref"] = "A B" },
		"bad strategy":   func(m map[string]any) { m["strategy"] = "yolo" },
		"drain too low":  func(m map[string]any) { m["drain_s"] = 1 },
		"image id shape": func(m map[string]any) { m["image_id"] = "sha256:xyz"; m["repo_digest"] = "x" },
	} {
		var mm map[string]any
		_ = json.Unmarshal(raw, &mm)
		mod(mm)
		b, _ := json.Marshal(mm)
		if _, err := ParseSnapshot(string(b)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseSnapshot(`{"v":1,"v":1}`); err == nil {
		t.Error("a duplicate key was accepted")
	}
}

// A write token's deploy cannot undo an admin's set_source: the enqueue transaction
// finds the service's image changed since this request read it and refuses.
func TestDeployCannotUndoAConcurrentSetSource(t *testing.T) {
	g := newDeployRig(t)
	g.create("p", "web", nil)
	d := &svcDeps{ServiceDeps: ServiceDeps{DB: g.st, Now: time.Now}}
	if sched, err := jobs.NewScheduler(jobs.Config{Store: jobs.NewStore(g.st, nil), Run: func(context.Context, jobs.Job) error { return nil }}); err == nil {
		d.Jobs = sched
	}
	row, err := d.load(context.Background(), g.st.R, "p/web") // what the deploy request read
	if err != nil {
		t.Fatal(err)
	}
	// The admin changes the repository in between.
	must[ServiceChangeOut](g, "services.set_source", m{"service": "p/web", "image_ref": "ghcr.io/x/app:2"})
	_, err = d.queueDeploy(context.Background(), actions.Call{Actor: writeTok}, row, row.imageRef)
	wantKind(t, err, actions.ErrConflict)
	if !strings.HasSuffix(g.get("p/web").ImageRef, "ghcr.io/x/app:2") {
		t.Errorf("the admin's image was undone: %s", g.get("p/web").ImageRef)
	}
	if countRows(t, g.st, "deploys") != 0 || countRows(t, g.st, "jobs") != 0 {
		t.Error("a refused deploy left rows")
	}
}

// Any failure to queue after the service was created says so.
func TestServiceCreateDeployFailureSaysTheServiceWasCreated(t *testing.T) {
	g := newSvcRig(t, func(d *ServiceDeps) { d.Jobs = failingQueue{} })
	_, err := callAs[ServiceChangeOut](g, adminActor(), "services.create", m{"project": "p", "name": "web", "image_ref": "nginx", "deploy": true})
	if err == nil || !strings.Contains(err.Error(), "the service was created but not deployed") {
		t.Fatalf("%v", err)
	}
	if countRows(t, g.st, "services") != 1 {
		t.Error("the service is not there")
	}
}

type failingQueue struct{}

func (failingQueue) EnqueueDeploy(context.Context, jobs.NewDeploy) (jobs.Enqueued, error) {
	return jobs.Enqueued{}, errors.New("disk full")
}
func (failingQueue) Enqueue(context.Context, jobs.NewJob) (int64, error) {
	return 0, errors.New("disk full")
}
func (failingQueue) EnqueueDelete(context.Context, jobs.NewDelete) (jobs.Deleted, error) {
	return jobs.Deleted{}, errors.New("disk full")
}
func (failingQueue) CancelDeploy(context.Context, int64, string, []string) (string, error) {
	return "", errors.New("x")
}

func TestSnapshotRepoDigestMustBeADigestOfTheSameRepository(t *testing.T) {
	base := Snapshot{V: 1, ImageRef: "nginx:1", Health: SnapHealth{Kind: "running", TimeoutS: 60, Passes: 1}, Strategy: "blue_green", DrainS: 10,
		ImageID: "sha256:" + strings.Repeat("a", 64)}
	digest := "@sha256:" + strings.Repeat("b", 64)
	for name, tc := range map[string]struct {
		ref  string
		good bool
	}{
		"same repository":     {"nginx" + digest, true},
		"the docker.io form":  {"docker.io/library/nginx" + digest, true},
		"another repository":  {"redis" + digest, false},
		"another registry":    {"ghcr.io/x/nginx" + digest, false},
		"a tag, not a digest": {"nginx:1", false},
		"not a reference":     {"A B", false},
	} {
		s := base
		s.RepoDigest = tc.ref
		if err := s.Validate(); (err == nil) != tc.good {
			t.Errorf("%s: %v", name, err)
		}
	}
	s := base
	s.ImageID, s.RepoDigest = "", "nginx"+digest
	if s.Validate() == nil {
		t.Error("a digest without its image id was accepted")
	}
}
