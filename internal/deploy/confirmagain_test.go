package deploy

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

// delete_confirm_again is only a hint for the page: it may be written over NULL only
// and never blocks versions_kept, previous_version_down or the reconciler.

func (g *rrig) attentionOf() sql.NullString {
	g.t.Helper()
	var a sql.NullString
	if err := g.st.R.QueryRow(`SELECT needs_attention FROM services WHERE id = ?`, g.svc).Scan(&a); err != nil {
		g.t.Fatal(err)
	}
	return a
}

// The boot pass writes versions_kept over a leftover hint.
func TestBootPassWritesVersionsKeptOverTheConfirmAgainHint(t *testing.T) {
	g := newRecRig(t, nil)
	g.expose(g.svc, 9000)
	g.activeDeploy(g.svc, 1, "10.250.0.2", false)
	d, j := g.deploy(g.svc, func(s *Snapshot) { s.Strategy = "stop_first" })
	mustExec(t, g.st, `UPDATE jobs SET state = 'running', boot_id = 'old', request_id = 'job-1-a1' WHERE id = ?`, j)
	mustExec(t, g.st, `UPDATE deploys SET status = 'running' WHERE id = ?`, d)
	mustExec(t, g.st, `INSERT INTO deploy_stages(deploy_id, stage, state) VALUES (?, 'start', 'running')`, d)
	g.put(g.svc, d, 1, "10.250.0.3", true)
	mustExec(t, g.st, `UPDATE services SET needs_attention = ? WHERE id = ?`, AttentionDeleteConfirmAgain, g.svc)
	if n, err := ResolveInterrupted(context.Background(), g.st, "new", time.Now(), nil); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if a := g.attention(g.svc); a != AttentionVersionsKept {
		t.Errorf("needs_attention %q, want versions_kept over the hint", a)
	}
}

// Queuing any work clears the hint in the same transaction: deploy, redeploy,
// rollback, restart and a new delete.
func TestQueuedWorkClearsTheConfirmAgainHint(t *testing.T) {
	g := newRRig(t)
	set := func() {
		mustExec(t, g.st, `UPDATE services SET needs_attention = ? WHERE id = ?`, AttentionDeleteConfirmAgain, g.svc)
	}
	g.liveViaStore()
	for _, c := range []struct {
		name string
		in   m
	}{
		{"services.deploy", m{"service": "p/web"}},
		{"services.redeploy", m{"service": "p/web"}},
		{"services.rollback", m{"service": "p/web", "to": 1}},
		{"services.restart", m{"service": "p/web"}},
	} {
		set()
		// The request may be refused for other reasons; a refusal must not clear it, a queued job must.
		before := g.rows(`SELECT COUNT(*) FROM jobs`)
		_, err := g.invoke(rigWrite, c.name, c.in)
		queued := g.rows(`SELECT COUNT(*) FROM jobs`) > before
		if got := g.attentionOf(); queued && got.Valid {
			t.Errorf("%s queued a job and left needs_attention %q", c.name, got.String)
		} else if !queued && (!got.Valid || got.String != AttentionDeleteConfirmAgain) {
			t.Errorf("%s (%v) queued nothing but changed needs_attention to %v", c.name, err, got)
		}
	}
	// A new delete.
	set()
	sess := g.adminSession("sess-ca", time.Now().Add(time.Hour))
	if _, err := g.js.EnqueueDelete(context.Background(), jobs.NewDelete{ServiceID: g.svc, Target: "p/web", Actor: jobs.ActorOf(sess), StopGraceS: 10}); err != nil {
		t.Fatal(err)
	}
	if got := g.attentionOf(); got.Valid {
		t.Errorf("a queued delete left needs_attention %q", got.String)
	}
}

// The hint is written over NULL only: never over another reason.
func TestConfirmAgainHintIsNotWrittenOverAnotherReason(t *testing.T) {
	g := newRRig(t)
	for _, other := range []string{AttentionDeleteInterrupted, AttentionVersionsKept, AttentionPreviousDown} {
		mustExec(t, g.st, `UPDATE services SET needs_attention = ? WHERE id = ?`, other, g.svc)
		g.runner.flagConfirmAgain(g.svc)
		if got := g.attentionOf(); got.String != other {
			t.Errorf("the hint overwrote %q: %q", other, got.String)
		}
	}
	mustExec(t, g.st, `UPDATE services SET needs_attention = NULL WHERE id = ?`, g.svc)
	g.runner.flagConfirmAgain(g.svc)
	if got := g.attentionOf(); got.String != AttentionDeleteConfirmAgain {
		t.Errorf("the hint was not written over NULL: %q", got.String)
	}
}

// Reconciler rule 3 still starts a stopped service that carries the hint.
func TestRule3StartsAStoppedServiceCarryingTheConfirmAgainHint(t *testing.T) {
	g := newRecRig(t, nil)
	_, id := g.activeDeploy(g.svc, 1, "10.250.0.2", false)
	mustExec(t, g.st, `UPDATE services SET needs_attention = ? WHERE id = ?`, AttentionDeleteConfirmAgain, g.svc)
	g.resync(true)
	if c := g.ops.find(id); c == nil || !c.running {
		t.Error("the stopped active container was not started")
	}
}
