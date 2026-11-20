package audit

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/redact"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

var ctx = context.Background()
var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newLogger(t *testing.T, st *store.Store, opts ...Option) *Logger {
	t.Helper()
	l, err := New(st, quiet, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func entry(action string) Entry {
	return Entry{ActorKind: "user", ActorID: "u1", Action: action, Target: "app:web", Risk: "mutate",
		Phase: PhaseIntent, Result: ResultPending, ArgsJSON: `{"name":"web"}`, RequestID: "req-1"}
}

func seed(t *testing.T, l *Logger, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		e := entry(fmt.Sprintf("app.act%d", i))
		if i%3 == 1 {
			e.Executor, e.Approver, e.DryRun = "system", "admin", true
		}
		if _, err := l.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
}

func verify(t *testing.T, st *store.Store) VerifyResult {
	t.Helper()
	r, err := Verify(ctx, st.R)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestChainVerifies(t *testing.T) {
	st := newStore(t)
	l := newLogger(t, st)
	if r := verify(t, st); !r.OK || r.Rows != 0 {
		t.Fatalf("empty log: %+v", r)
	}
	seed(t, l, 6)
	r := verify(t, st)
	h, err := l.Head(ctx)
	if err != nil || !r.OK || r.Rows != 6 || h.Count != 6 || !bytes.Equal(h.Hash, r.Head) || h.HashHex() == "" {
		t.Fatalf("verify %+v head %+v %v", r, h, err)
	}
	var prev0 []byte
	st.R.QueryRow(`SELECT prev_hash FROM audit_log WHERE id = 1`).Scan(&prev0)
	if !bytes.Equal(prev0, Genesis) || len(Genesis) != 32 {
		t.Error("the first row does not chain from the 32-byte genesis")
	}
	if err := l.LogHead(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestTamperingIsDetectedAtTheRightID(t *testing.T) {
	// The append-only triggers guard against our own bugs; dropping them models
	// someone editing the file.
	prep := func(t *testing.T) (*store.Store, *Logger) {
		st := newStore(t)
		l := newLogger(t, st)
		seed(t, l, 6)
		for _, q := range []string{`DROP TRIGGER audit_log_no_update`, `DROP TRIGGER audit_log_no_delete`} {
			if _, err := st.W.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		return st, l
	}
	cases := []struct {
		name string
		do   func(t *testing.T, st *store.Store)
		want int64
	}{
		{"a field", func(t *testing.T, st *store.Store) {
			exec(t, st, `UPDATE audit_log SET args_json='{"name":"evil"}' WHERE id=3`)
		}, 3},
		{"the boot id", func(t *testing.T, st *store.Store) { exec(t, st, `UPDATE audit_log SET boot_id='other' WHERE id=2`) }, 2},
		{"the actor", func(t *testing.T, st *store.Store) { exec(t, st, `UPDATE audit_log SET actor_id='admin' WHERE id=5`) }, 5},
		{"an executor added", func(t *testing.T, st *store.Store) { exec(t, st, `UPDATE audit_log SET executor='system' WHERE id=1`) }, 1},
		{"a deleted row", func(t *testing.T, st *store.Store) { exec(t, st, `DELETE FROM audit_log WHERE id=3`) }, 4},
		{"the first row deleted", func(t *testing.T, st *store.Store) { exec(t, st, `DELETE FROM audit_log WHERE id=1`) }, 2},
		{"swapped rows", func(t *testing.T, st *store.Store) {
			exec(t, st, `UPDATE audit_log SET id=100 WHERE id=3`)
			exec(t, st, `UPDATE audit_log SET id=3 WHERE id=4`)
			exec(t, st, `UPDATE audit_log SET id=4 WHERE id=100`)
		}, 3},
		{"a forged hash", func(t *testing.T, st *store.Store) { exec(t, st, `UPDATE audit_log SET hash=zeroblob(32) WHERE id=4`) }, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, _ := prep(t)
			c.do(t, st)
			r := verify(t, st)
			if r.OK || r.BrokenID != c.want {
				t.Fatalf("verify = %+v, want broken at %d", r, c.want)
			}
		})
	}
}

func exec(t *testing.T, st *store.Store, q string) {
	t.Helper()
	if _, err := st.W.Exec(q); err != nil {
		t.Fatal(err)
	}
}

func TestTableIsAppendOnly(t *testing.T) {
	st := newStore(t)
	seed(t, newLogger(t, st), 2)
	if _, err := st.W.Exec(`UPDATE audit_log SET result='ok' WHERE id=1`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("update: %v", err)
	}
	if _, err := st.W.Exec(`DELETE FROM audit_log WHERE id=1`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("delete: %v", err)
	}
	var n int
	st.R.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='audit_log_intent'`).Scan(&n)
	if n != 1 {
		t.Error("the intent_id index is missing")
	}
}

func TestFiftyConcurrentAppendersKeepTheChainLinear(t *testing.T) {
	st := newStore(t)
	l := newLogger(t, st)
	var wg sync.WaitGroup
	ids := make(chan int64, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := l.Append(ctx, entry(fmt.Sprintf("app.c%d", i)))
			if err != nil {
				t.Error(err)
				return
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[int64]bool{}
	for id := range ids {
		if seen[id] {
			t.Errorf("duplicate id %d", id)
		}
		seen[id] = true
	}
	if r := verify(t, st); !r.OK || r.Rows != 50 || len(seen) != 50 {
		t.Fatalf("verify %+v, %d distinct ids", r, len(seen))
	}
}

func TestPendingUnknownAndALaterOutcomeResolves(t *testing.T) {
	st := newStore(t)
	a := newLogger(t, st, WithBootID("boot-A"))
	intent, err := a.Append(ctx, entry("app.deploy"))
	if err != nil {
		t.Fatal(err)
	}
	if s, err := a.IntentStatus(ctx, intent); err != nil || s != StatusPending {
		t.Fatalf("same boot, no outcome: %q %v", s, err)
	}
	b := newLogger(t, st, WithBootID("boot-B")) // a later boot
	if s, err := b.IntentStatus(ctx, intent); err != nil || s != StatusUnknown {
		t.Fatalf("older boot, no outcome: %q %v", s, err)
	}
	// The reconciler of boot B establishes what happened and writes the outcome.
	out := entry("app.deploy")
	out.Phase, out.IntentID, out.Result = PhaseOutcome, intent, ResultOK
	if _, err := b.Append(ctx, out); err != nil {
		t.Fatal(err)
	}
	for _, l := range []*Logger{a, b} {
		if s, err := l.IntentStatus(ctx, intent); err != nil || s != StatusOK {
			t.Fatalf("after the outcome: %q %v", s, err)
		}
	}
	if _, err := b.IntentStatus(ctx, 9999); err == nil {
		t.Error("an unknown intent id was accepted")
	}
	if r := verify(t, st); !r.OK {
		t.Errorf("chain broken: %+v", r)
	}
}

func TestEntryValidation(t *testing.T) {
	l := newLogger(t, newStore(t))
	bad := []func(*Entry){
		func(e *Entry) { e.Phase = "weird" },
		func(e *Entry) { e.Result = "weird" },
		func(e *Entry) { e.Risk = "weird" },
		func(e *Entry) { e.ActorID = "" },
		func(e *Entry) { e.Action = "" },
		func(e *Entry) { e.Phase, e.IntentID = PhaseOutcome, 0 }, // outcome without an intent
		func(e *Entry) { e.IntentID = 5 },                        // intent row with an intent_id
	}
	for i, f := range bad {
		e := entry("app.x")
		f(&e)
		if _, err := l.Append(ctx, e); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

// failing wraps an Appender and fails the Nth call (1-based) and every call from then on if sticky.
type failing struct {
	inner  Appender
	failAt int
	calls  int
}

func (f *failing) Append(ctx context.Context, e Entry) (int64, error) {
	f.calls++
	if f.calls == f.failAt {
		return 0, errors.New("disk full")
	}
	return f.inner.Append(ctx, e)
}

func TestFailedIntentWriteMeansNoExecution(t *testing.T) {
	st := newStore(t)
	f := &failing{inner: newLogger(t, st), failAt: 1}
	ran := false
	out, err := Execute(ctx, f, quiet, redact.New(), entry("secret.reveal"), func(context.Context) (string, error) {
		ran = true
		return "THE-SECRET", nil
	})
	if ran {
		t.Fatal("the action ran although its intent could not be written")
	}
	if !errors.Is(err, ErrAuditUnavailable) || out.Value != "" {
		t.Fatalf("err = %v, value = %q", err, out.Value)
	}
	if !strings.Contains(err.Error(), "audit unavailable; action not run") {
		t.Errorf("message: %v", err)
	}
}

func TestFailedOutcomeWriteReportsItButKeepsTheResult(t *testing.T) {
	st := newStore(t)
	f := &failing{inner: newLogger(t, st), failAt: 2}
	out, err := Execute(ctx, f, quiet, redact.New(), entry("app.restart"), func(context.Context) (int, error) { return 42, nil })
	if !errors.Is(err, ErrAuditOutcome) || out.Value != 42 || out.AuditOutcomeRecorded {
		t.Fatalf("out %+v err %v", out, err)
	}
	// Both fail: the execution error comes back wrapped with ErrAuditOutcome.
	f = &failing{inner: newLogger(t, st), failAt: 2}
	boom := errors.New("container did not start")
	_, err = Execute(ctx, f, quiet, redact.New(), entry("app.restart"), func(context.Context) (int, error) { return 0, boom })
	if !errors.Is(err, boom) || !errors.Is(err, ErrAuditOutcome) {
		t.Fatalf("both failed: %v", err)
	}
	// Success path records the outcome, linked to the intent.
	l := newLogger(t, st)
	out2, err := Execute(ctx, l, quiet, redact.New(), entry("app.ok"), func(context.Context) (string, error) { return "fine", nil })
	if err != nil || !out2.AuditOutcomeRecorded || out2.Value != "fine" || out2.IntentID == 0 {
		t.Fatalf("%+v %v", out2, err)
	}
	if s, _ := l.IntentStatus(ctx, out2.IntentID); s != StatusOK {
		t.Errorf("status %q", s)
	}
}

func TestPanicBecomesAnErrorOutcome(t *testing.T) {
	st := newStore(t)
	l := newLogger(t, st)
	out, err := Execute(ctx, l, quiet, redact.New(), entry("app.boom"), func(context.Context) (int, error) { panic("kaboom") })
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("err = %v", err)
	}
	if s, _ := l.IntentStatus(ctx, out.IntentID); s != StatusError {
		t.Errorf("status %q", s)
	}
}

type secretInput struct {
	Name     string `json:"name"`
	Password string `json:"password" sd:"secret"`
}

func TestSentinelSecretNeverReachesTheErrorColumnOrTheLogs(t *testing.T) {
	st := newStore(t)
	var logs bytes.Buffer
	l, _ := New(st, slog.New(slog.NewTextHandler(&logs, nil)))
	in := secretInput{Name: "db", Password: "SENTINEL-p4ssw0rd"}
	args, _ := redact.ArgsJSON(in)
	e := entry("db.create")
	e.ArgsJSON = args
	_, err := Execute(ctx, &failing{inner: l, failAt: 2}, slog.New(slog.NewTextHandler(&logs, nil)), redact.ForInput(in), e,
		func(context.Context) (int, error) {
			return 0, fmt.Errorf("connect failed with password SENTINEL-p4ssw0rd")
		})
	if err == nil {
		t.Fatal("expected an error")
	}
	// A second run whose outcome write succeeds, to inspect the stored error column.
	_, _ = Execute(ctx, l, quiet, redact.ForInput(in), e, func(context.Context) (int, error) {
		return 0, fmt.Errorf("connect failed with password SENTINEL-p4ssw0rd")
	})
	var all string
	rows, _ := st.R.Query(`SELECT args_json || '|' || error FROM audit_log`)
	for rows.Next() {
		var s string
		rows.Scan(&s)
		all += s + "\n"
	}
	rows.Close()
	if strings.Contains(all, "SENTINEL") || strings.Contains(logs.String(), "SENTINEL") {
		t.Fatalf("secret leaked:\nrows:\n%s\nlogs:\n%s", all, logs.String())
	}
	if !strings.Contains(all, redact.Placeholder) {
		t.Errorf("no placeholder in the rows: %s", all)
	}
	// And the sentinel never appears anywhere in the database file bytes.
	var dump string
	for _, q := range []string{`SELECT group_concat(error, ' ') FROM audit_log`, `SELECT group_concat(args_json, ' ') FROM audit_log`} {
		var s sql.NullString
		st.R.QueryRow(q).Scan(&s)
		dump += s.String
	}
	if strings.Contains(dump, "SENTINEL") {
		t.Error("sentinel in the stored text")
	}
}

func TestDeniedRowsAreRateLimitedWithASuppressedSummary(t *testing.T) {
	st := newStore(t)
	clk := &clock{t: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
	l := newLogger(t, st, WithClock(clk.Now))
	deny := func(action string) bool {
		e := entry(action)
		e.Phase = PhaseDenied
		w, err := l.Denied(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	written := 0
	for i := 0; i < 15; i++ {
		if deny("app.delete") {
			written++
		}
	}
	if written != 10 {
		t.Fatalf("written %d, want 10", written)
	}
	if !deny("app.other") { // another action has its own budget
		t.Error("a different action was limited by the first")
	}
	var n int
	st.R.QueryRow(`SELECT count(*) FROM audit_log WHERE phase='denied'`).Scan(&n)
	if n != 11 {
		t.Fatalf("denied rows %d, want 11", n)
	}
	clk.Add(61 * time.Second)
	if !deny("app.delete") { // new window: the summary of the old one, then this attempt
		t.Fatal("first attempt of a new window was dropped")
	}
	var msg string
	if err := st.R.QueryRow(`SELECT error FROM audit_log WHERE error LIKE '%suppressed%'`).Scan(&msg); err != nil || msg != "5 denied attempts suppressed" {
		t.Fatalf("summary row: %q %v", msg, err)
	}
	// Flush writes a pending summary at shutdown.
	for i := 0; i < 13; i++ {
		deny("app.delete")
	}
	if err := l.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	st.R.QueryRow(`SELECT count(*) FROM audit_log WHERE error LIKE '%suppressed%'`).Scan(&count)
	if count != 2 {
		t.Errorf("summary rows %d, want 2", count)
	}
	if r := verify(t, st); !r.OK {
		t.Errorf("chain: %+v", r)
	}
}
