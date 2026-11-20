package audit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

const fixturePath = "../store/testdata/v2.db"

// TestGenerateFixtureV2 regenerates internal/store/testdata/v2.db: a database at
// schema v2 (migrations 0001 and 0002) holding a verifiable audit chain.
//
//	UPDATE_FIXTURES=1 go test ./internal/audit -run TestGenerateFixtureV2
//
// Fixtures are committed once per milestone that changes the schema (not per
// release), so the N-5 upgrade tests in M7b have fine-grained steps.
func TestGenerateFixtureV2(t *testing.T) {
	if os.Getenv("UPDATE_FIXTURES") == "" {
		t.Skip("set UPDATE_FIXTURES=1 to regenerate the v2 fixture")
	}
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	clk := &clock{t: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
	l, err := New(st, quiet, WithClock(clk.Now), WithBootID("fixture-boot-0001"))
	if err != nil {
		t.Fatal(err)
	}
	e := func(action, phase, result string, intent int64) Entry {
		return Entry{ActorKind: "user", ActorID: "admin", Action: action, Target: "app:web", Risk: "mutate",
			Phase: phase, Result: result, IntentID: intent, ArgsJSON: `{"name":"web"}`, RequestID: "fixture-req"}
	}
	step := func(en Entry) int64 {
		clk.Add(time.Second)
		id, err := l.Append(ctx, en)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	i1 := step(e("app.restart", PhaseIntent, ResultPending, 0))
	step(e("app.restart", PhaseOutcome, ResultOK, i1))
	i2 := step(e("app.deploy", PhaseIntent, ResultPending, 0))
	step(e("app.deploy", PhaseOutcome, ResultError, i2))
	den := e("app.delete", PhaseDenied, ResultDenied, 0)
	den.ActorKind, den.ActorID, den.Risk, den.Error = "token", "tok-fixture", "destructive", "API tokens cannot run destructive actions"
	step(den)
	if _, err := st.W.Exec(`INSERT INTO meta (key, value) VALUES ('fixture', 'v2')`); err != nil {
		t.Fatal(err)
	}
	out, _ := filepath.Abs(fixturePath)
	os.Remove(out)
	if _, err := st.W.Exec("VACUUM INTO '" + strings.ReplaceAll(out, "'", "''") + "'"); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", out)
}

// The committed fixture is a valid v2 database whose audit chain verifies; any
// change that alters how rows are hashed breaks this, which is the point.
func TestFixtureV2AuditChainVerifies(t *testing.T) {
	dir := t.TempDir()
	b, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "serverdock.db"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	ro, err := store.OpenReadOnly(dir, store.ReadOnlyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if ro.Schema != "v2" {
		t.Fatalf("schema %q", ro.Schema)
	}
	res, err := Verify(ctx, ro.DB)
	if err != nil || !res.OK || res.Rows != 5 || bytes.Equal(res.Head, Genesis) {
		t.Fatalf("verify %+v %v", res, err)
	}
	var boot string
	ro.DB.QueryRow(`SELECT DISTINCT boot_id FROM audit_log`).Scan(&boot)
	if boot != "fixture-boot-0001" {
		t.Errorf("boot id %q", boot)
	}
}
