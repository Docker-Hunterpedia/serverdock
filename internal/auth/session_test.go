package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func (e *env) user(id int64) {
	e.t.Helper()
	if _, err := e.st.W.Exec(`INSERT OR IGNORE INTO users (id, username, created_at) VALUES (?, ?, 1)`, id, fmt.Sprint("u", id)); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) newSession(userID int64) (string, Session) {
	e.t.Helper()
	var secret string
	var sess Session
	err := e.s.write(ctx, func(tx *sql.Tx) error {
		var err error
		secret, sess, err = e.s.createSession(ctx, tx, userID, "1.2.3.4", "ua", "{}", nil)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return secret, sess
}

func TestSessionLifetimes(t *testing.T) {
	e := newEnv(t)
	e.user(1)
	_, sess := e.newSession(1)
	if d := sess.IdleExpiresAt.Sub(sess.CreatedAt); d != 12*time.Hour {
		t.Errorf("idle %v", d)
	}
	if d := sess.AbsExpiresAt.Sub(sess.CreatedAt); d != 7*24*time.Hour {
		t.Errorf("absolute %v", d)
	}
	if d := sess.MFAFreshUntil.Sub(sess.CreatedAt); d != 5*time.Minute {
		t.Errorf("fresh 2FA window %v", d)
	}
}

func TestExpiredSessionRefusedClearedAndDeleted(t *testing.T) {
	e := newEnv(t)
	e.user(1)
	check := func(secret, reason string) {
		t.Helper()
		_, err := e.s.LookupSession(ctx, secret)
		var se *SessionError
		if !errors.As(err, &se) || se.Reason != reason {
			t.Fatalf("got %v, want reason %s", err, reason)
		}
	}
	// Idle expiry: 12 hours without a request.
	s1, _ := e.newSession(1)
	e.clk.Add(12*time.Hour + time.Second)
	check(s1, "idle_expired")
	if e.count(`SELECT count(*) FROM sessions`) != 0 {
		t.Error("an idle-expired session was not deleted on lookup")
	}
	check(s1, "revoked") // gone: the same cookie is now simply unknown
	// Activity keeps it alive, but never past 7 days.
	s2, _ := e.newSession(1)
	for i := 0; i < 15; i++ {
		e.clk.Add(11 * time.Hour)
		if _, err := e.s.LookupSession(ctx, s2); err != nil {
			if i < 14 {
				t.Fatalf("step %d: %v", i, err)
			}
			break
		}
	}
	e.clk.Add(11 * time.Hour)
	check(s2, "absolute_expired")
	if e.count(`SELECT count(*) FROM sessions`) != 0 {
		t.Error("an absolute-expired session was not deleted")
	}
	// Unknown, empty and oversized cookies are all "no session".
	for _, junk := range []string{"", "nope", string(make([]byte, 1000))} {
		check(junk, "revoked")
	}
}

func TestExpiredSessionsPrunedOnInsert(t *testing.T) {
	e := newEnv(t)
	e.user(1)
	now := e.clk.Now().Unix()
	for i := 0; i < 150; i++ { // spread over users so the per-user cap does not trim them
		uid := int64(2 + i/15)
		e.user(uid)
		if _, err := e.st.W.Exec(`INSERT INTO sessions (id_hash, public_id, user_id, created_at, last_seen_at, idle_expires_at, abs_expires_at, mfa_fresh_until, ip_key, user_agent)
			VALUES (?, ?, ?, 1, 1, ?, ?, 1, 'x', 'x')`, []byte(fmt.Sprint("hash", i)), fmt.Sprint("pub", i), uid, now-10, now+1000); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 130; i++ { // expired pending rows too
		e.st.W.Exec(`INSERT INTO auth_pending (id_hash, user_id, expires_at, tries_left, ip_key, created_at) VALUES (?, ?, ?, 5, 'x', 1)`, []byte(fmt.Sprint("p", i)), 1, now-10)
	}
	e.newSession(1)
	// One insert prunes at most 100 of each; the rest wait for later inserts.
	if n := e.count(`SELECT count(*) FROM sessions WHERE idle_expires_at <= ?`, now); n != 50 {
		t.Errorf("%d expired sessions left after one insert, want 50", n)
	}
	if n := e.count(`SELECT count(*) FROM auth_pending`); n != 30 {
		t.Errorf("%d expired pending rows left, want 30", n)
	}
	e.newSession(1)
	if n := e.count(`SELECT count(*) FROM sessions WHERE idle_expires_at <= ?`, now); n != 0 {
		t.Errorf("%d expired sessions left after the second insert", n)
	}
	if e.count(`SELECT count(*) FROM sessions`) != 2 {
		t.Error("live sessions were pruned")
	}
}

func TestSessionCapTwenty(t *testing.T) {
	e := newEnv(t)
	e.user(1)
	e.user(2)
	var secrets []string
	for i := 0; i < 25; i++ {
		e.clk.Add(time.Second)
		s, _ := e.newSession(1)
		secrets = append(secrets, s)
	}
	other, _ := e.newSession(2)
	if n := e.count(`SELECT count(*) FROM sessions WHERE user_id = 1`); n != 20 {
		t.Fatalf("%d sessions for user 1", n)
	}
	// The five with the oldest last_seen_at are gone; the newest 20 work.
	for i, s := range secrets {
		_, err := e.s.LookupSession(ctx, s)
		if (i < 5) == (err == nil) {
			t.Errorf("session %d: err=%v", i, err)
		}
	}
	if _, err := e.s.LookupSession(ctx, other); err != nil {
		t.Errorf("another user's session was removed: %v", err)
	}
}

func TestLastSeenWrittenOncePerMinute(t *testing.T) {
	e := newEnv(t)
	e.user(1)
	secret, _ := e.newSession(1)
	var writes atomic.Int32
	e.s.touchTx = func(c context.Context, fn func(*sql.Tx) error) error { writes.Add(1); return e.s.write(c, fn) }
	look := func() *Session {
		t.Helper()
		s, err := e.s.LookupSession(ctx, secret)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	for i := 0; i < 5; i++ { // within the first minute: no write at all
		e.clk.Add(10 * time.Second)
		look()
	}
	if writes.Load() != 0 {
		t.Fatalf("%d writes within a minute of creation", writes.Load())
	}
	e.clk.Add(30 * time.Second) // 80 s since the session was last seen
	s := look()
	if writes.Load() != 1 || !s.LastSeenAt.Equal(e.clk.Now()) {
		t.Fatalf("writes %d, last seen %v", writes.Load(), s.LastSeenAt)
	}
	for i := 0; i < 5; i++ { // a burst of requests: still one write
		e.clk.Add(5 * time.Second)
		look()
	}
	if writes.Load() != 1 {
		t.Fatalf("%d writes for a burst", writes.Load())
	}
	e.clk.Add(time.Minute)
	look()
	if writes.Load() != 2 {
		t.Fatalf("%d writes after another minute", writes.Load())
	}
	// Idle expiry moved with the activity.
	if got := look().IdleExpiresAt; got.Sub(e.clk.Now()) != 12*time.Hour {
		t.Errorf("idle expiry %v ahead", got.Sub(e.clk.Now()))
	}
}

// A failing activity write is logged (once a minute) and the request goes on.
func TestLastSeenFailureIsBestEffort(t *testing.T) {
	e := newEnv(t)
	e.user(1)
	secret, _ := e.newSession(1)
	e.s.touchTx = func(context.Context, func(*sql.Tx) error) error { return errors.New("disk full") }
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		e.clk.Add(2 * time.Minute)
		sess, err := e.s.LookupSession(ctx, secret)
		if err != nil || sess == nil {
			t.Fatalf("the lookup failed with its best-effort write: %v", err)
		}
	}
	wg.Wait()
	// Three failures two minutes apart: each is its own minute, so three warnings.
	if n := countOf(e.logs.String(), "could not record session activity"); n != 3 {
		t.Errorf("%d warnings, want 3", n)
	}
	// Within one minute many failures log once.
	e2 := newEnv(t)
	e2.user(1)
	secret2, _ := e2.newSession(1)
	e2.s.touchTx = func(context.Context, func(*sql.Tx) error) error { return errors.New("disk full") }
	e2.clk.Add(2 * time.Minute)
	for i := 0; i < 20; i++ {
		e2.clk.Add(time.Second)
		e2.s.LookupSession(ctx, secret2)
	}
	if n := countOf(e2.logs.String(), "could not record session activity"); n != 1 {
		t.Errorf("%d warnings for failures within one minute, want 1", n)
	}
}

func countOf(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}

func TestRevokeSessions(t *testing.T) {
	e := newEnv(t)
	e.user(1)
	e.user(2)
	a, _ := e.newSession(1)
	b, _ := e.newSession(1)
	c, _ := e.newSession(2)
	if err := e.s.RevokeSession(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.LookupSession(ctx, a); err == nil {
		t.Error("a revoked session still works")
	}
	if n, err := e.s.RevokeUserSessions(ctx, 1); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := e.s.LookupSession(ctx, b); err == nil {
		t.Error("a session survived revoke-all")
	}
	if _, err := e.s.LookupSession(ctx, c); err != nil {
		t.Error("another user's session was revoked")
	}
	if err := e.s.RevokeSession(ctx, "unknown"); err != nil {
		t.Errorf("revoking an unknown session: %v", err)
	}
}

// ---- pending second factor and the sign-in transition ------------------------

func TestPendingTokenFiveTries(t *testing.T) {
	e := newEnv(t)
	e.user(1)
	secret, err := e.s.CreatePending(ctx, 1, "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	for want := 4; want >= 0; want-- {
		info, err := e.s.PendingAttempt(ctx, secret)
		if err != nil || info.UserID != 1 || info.TriesLeft != want {
			t.Fatalf("attempt: %+v %v, want %d left", info, err, want)
		}
	}
	if _, err := e.s.PendingAttempt(ctx, secret); !errors.Is(err, ErrPendingInvalid) {
		t.Fatalf("a 6th attempt: %v", err)
	}
	if err := e.s.PendingFailed(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM auth_pending`) != 0 {
		t.Error("the used-up pending row was kept")
	}
	// PendingFailed with tries left keeps the row.
	secret, _ = e.s.CreatePending(ctx, 1, "x")
	e.s.PendingAttempt(ctx, secret)
	e.s.PendingFailed(ctx, secret)
	if e.count(`SELECT count(*) FROM auth_pending`) != 1 {
		t.Error("a pending row with tries left was deleted")
	}
	// Concurrent guesses cannot exceed five.
	secret, _ = e.s.CreatePending(ctx, 1, "x")
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.s.PendingAttempt(ctx, secret); err == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 5 {
		t.Fatalf("%d concurrent attempts counted, want 5", ok.Load())
	}
	// Expired pending rows are refused.
	secret, _ = e.s.CreatePending(ctx, 1, "x")
	e.clk.Add(5*time.Minute + time.Second)
	if _, err := e.s.PendingAttempt(ctx, secret); !errors.Is(err, ErrPendingInvalid) {
		t.Fatalf("an expired pending row: %v", err)
	}
	for _, junk := range []string{"", "nope", string(make([]byte, 1000))} {
		if _, err := e.s.PendingAttempt(ctx, junk); !errors.Is(err, ErrPendingInvalid) {
			t.Errorf("%q: %v", junk, err)
		}
	}
}

func TestPendingPrunedAndCapped(t *testing.T) {
	e := newEnv(t)
	e.user(1)
	e.user(2)
	now := e.clk.Now().Unix()
	for i := 0; i < 130; i++ {
		e.st.W.Exec(`INSERT INTO auth_pending (id_hash, user_id, expires_at, tries_left, ip_key, created_at) VALUES (?, 2, ?, 5, 'x', 1)`, []byte(fmt.Sprint("old", i)), now-5)
	}
	var first, last string
	for i := 0; i < 8; i++ {
		e.clk.Add(time.Second)
		s, err := e.s.CreatePending(ctx, 1, "x")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = s
		}
		last = s
	}
	if n := e.count(`SELECT count(*) FROM auth_pending WHERE user_id = 1`); n != 5 {
		t.Fatalf("%d pending rows for the user, want the cap of 5", n)
	}
	if _, err := e.s.PendingAttempt(ctx, first); !errors.Is(err, ErrPendingInvalid) {
		t.Error("the oldest pending row survived the cap")
	}
	if _, err := e.s.PendingAttempt(ctx, last); err != nil {
		t.Errorf("the newest was removed: %v", err)
	}
	// Eight inserts pruned 100 expired rows (the first one's batch); 30 remain
	// at most, and the next inserts finish the job.
	if n := e.count(`SELECT count(*) FROM auth_pending WHERE user_id = 2`); n != 0 {
		t.Errorf("%d expired pending rows remain after eight inserts", n)
	}
}

func completeInput(secret string, counter int64) LoginInput {
	return LoginInput{PendingSecret: secret, TOTPCounter: counter, IPKey: "1.2.3.4", UserAgent: "ua"}
}

// A pending cookie authenticates nothing: it is not in the sessions table, so it
// is not a session.
func TestPendingIsNotASession(t *testing.T) {
	e := newEnv(t)
	e.user(1)
	pending, _ := e.s.CreatePending(ctx, 1, "x")
	_, err := e.s.LookupSession(ctx, pending)
	var se *SessionError
	if !errors.As(err, &se) {
		t.Fatalf("a pending secret was accepted as a session: %v", err)
	}
	if e.count(`SELECT count(*) FROM sessions`) != 0 {
		t.Fatal("a pending sign-in created a session")
	}
	// And the other way round: a session secret is not a pending sign-in.
	sess, _ := e.newSession(1)
	if _, err := e.s.PendingAttempt(ctx, sess); !errors.Is(err, ErrPendingInvalid) {
		t.Fatalf("a session secret was accepted as pending: %v", err)
	}
}

func TestLoginMintsNewSessionIDAndRevokesTheOldOne(t *testing.T) {
	e := newEnv(t)
	e.user(1)
	old, _ := e.newSession(1)
	pending, _ := e.s.CreatePending(ctx, 1, "1.2.3.4")
	in := completeInput(pending, 100)
	in.OldSessionSecret = old
	res, err := e.s.CompleteLogin(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionSecret == old || res.SessionSecret == pending || res.SessionSecret == "" {
		t.Fatal("the session id was not minted fresh")
	}
	if _, err := e.s.LookupSession(ctx, old); err == nil {
		t.Error("the old session survived the sign-in")
	}
	if _, err := e.s.LookupSession(ctx, res.SessionSecret); err != nil {
		t.Errorf("the new session does not work: %v", err)
	}
	// The pending row is single use.
	if _, err := e.s.CompleteLogin(ctx, completeInput(pending, 101)); !errors.Is(err, ErrPendingInvalid) {
		t.Errorf("a pending row was used twice: %v", err)
	}
	// A client cannot choose its session id: there is no input for it, and a
	// pending secret that is not in the table mints nothing.
	if _, err := e.s.CompleteLogin(ctx, completeInput("client-chosen", 102)); !errors.Is(err, ErrPendingInvalid) {
		t.Errorf("%v", err)
	}
	if n := e.count(`SELECT count(*) FROM sessions`); n != 1 {
		t.Errorf("%d sessions, want only the new one", n)
	}
}

func TestLoginRefusesAReusedCodeAndKeepsThePending(t *testing.T) {
	e := newEnv(t)
	e.user(1)
	e.s.AcceptTOTP(ctx, 1, 100)
	pending, _ := e.s.CreatePending(ctx, 1, "x")
	if _, err := e.s.CompleteLogin(ctx, completeInput(pending, 100)); !errors.Is(err, ErrCodeReused) {
		t.Fatalf("%v", err)
	}
	if e.count(`SELECT count(*) FROM sessions`) != 0 || e.count(`SELECT count(*) FROM auth_pending`) != 1 {
		t.Fatal("a refused sign-in changed state")
	}
	if _, err := e.s.CompleteLogin(ctx, completeInput(pending, 101)); err != nil {
		t.Fatal(err)
	}
}

func TestLoginWithARecoveryCode(t *testing.T) {
	e := newEnv(t)
	token, er, seed := e.enrolled()
	setup, err := e.s.SetupComplete(ctx, e.completion(token, er, seed))
	if err != nil {
		t.Fatal(err)
	}
	pending, _ := e.s.CreatePending(ctx, setup.UserID, "1.2.3.4")
	in := LoginInput{PendingSecret: pending, RecoveryCode: setup.RecoveryCodes[0], IPKey: "1.2.3.4"}
	if _, err := e.s.CompleteLogin(ctx, in); err != nil {
		t.Fatal(err)
	}
	pending2, _ := e.s.CreatePending(ctx, setup.UserID, "1.2.3.4")
	in.PendingSecret = pending2
	if _, err := e.s.CompleteLogin(ctx, in); !errors.Is(err, ErrBadCode) {
		t.Fatalf("a recovery code worked twice: %v", err)
	}
	if e.count(`SELECT count(*) FROM auth_pending`) != 1 {
		t.Error("the pending row was consumed by a bad recovery code")
	}
}

func TestSecondFactorNoticeKnownIPsAndCap(t *testing.T) {
	e := newEnv(t)
	e.user(1)
	for i := 0; i < 3; i++ {
		e.s.RecordSecondFactorFailure(ctx, 1, "198.51.100.9")
	}
	pending, _ := e.s.CreatePending(ctx, 1, "x")
	res, err := e.s.CompleteLogin(ctx, completeInput(pending, 100))
	if err != nil {
		t.Fatal(err)
	}
	if n := res.Session.Notice; n.TOTPFailures != 3 || n.LastFailureIP != "198.51.100.9" || n.LastFailureAt == 0 {
		t.Fatalf("notice %+v", n)
	}
	sess, _ := e.s.LookupSession(ctx, res.SessionSecret)
	if sess.Notice.TOTPFailures != 3 {
		t.Errorf("the notice did not persist with the session: %+v", sess.Notice)
	}
	// The counter reset: the next sign-in has no notice.
	pending, _ = e.s.CreatePending(ctx, 1, "x")
	res, _ = e.s.CompleteLogin(ctx, completeInput(pending, 101))
	if res.Session.Notice.TOTPFailures != 0 {
		t.Errorf("the notice was not reset: %+v", res.Session.Notice)
	}
	// Known IPs: remembered for 30 days, at most ten per user.
	if ok, _ := e.s.KnownIP(ctx, 1, "1.2.3.4"); !ok {
		t.Error("the sign-in IP is not known")
	}
	if ok, _ := e.s.KnownIP(ctx, 1, "9.9.9.9"); ok {
		t.Error("a stranger is known")
	}
	e.clk.Add(31 * 24 * time.Hour)
	if ok, _ := e.s.KnownIP(ctx, 1, "1.2.3.4"); ok {
		t.Error("an IP is still known after 30 days")
	}
	for i := 0; i < 14; i++ {
		e.clk.Add(time.Second)
		p, _ := e.s.CreatePending(ctx, 1, "x")
		in := completeInput(p, int64(200+i))
		in.IPKey = fmt.Sprintf("10.0.0.%d", i)
		if _, err := e.s.CompleteLogin(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.count(`SELECT count(*) FROM auth_known_ips WHERE user_id = 1`); n != 10 {
		t.Errorf("%d known IPs, want the cap of 10", n)
	}
	if ok, _ := e.s.KnownIP(ctx, 1, "10.0.0.13"); !ok {
		t.Error("the newest IP was evicted")
	}
	if ok, _ := e.s.KnownIP(ctx, 1, "10.0.0.0"); ok {
		t.Error("the oldest IP was kept past the cap")
	}
}
