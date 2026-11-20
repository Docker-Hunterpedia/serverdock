package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/secretbox"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type env struct {
	t       *testing.T
	dir     string
	st      *store.Store
	box     *secretbox.Box
	s       *Store
	clk     *fakeClock
	logs    *lockedBuf
	derives *atomic.Int32
}

const goodPassword = "correct horse battery staple"

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0o700)
	return openEnv(t, dir, bytes.Repeat([]byte{7}, 32))
}

func openEnv(t *testing.T, dir string, key []byte) *env {
	t.Helper()
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dir: dir, st: st, box: box, clk: &fakeClock{t: time.Unix(1_800_000_000, 0)}, logs: &lockedBuf{}, derives: &atomic.Int32{}}
	h := NewHasher()
	h.maxWaiters, h.wait = 64, 30*time.Second
	h.freeMem = func() {}
	h.derive = func(pw, salt []byte, m, tt uint32, th uint8, kl uint32) []byte {
		e.derives.Add(1)
		sum := sha256.Sum256(append(append([]byte{}, pw...), salt...))
		return sum[:kl]
	}
	e.s = NewStore(st, box, h, Options{Now: e.clk.Now, Log: slog.New(slog.NewTextHandler(e.logs, nil))})
	return e
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.st.R.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) token() string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, SetupTokenFile))
	if err != nil {
		e.t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// enrolled runs PrepareSetup and SetupEnroll and returns what a client would hold.
func (e *env) enrolled() (token string, er EnrollResult, seed []byte) {
	e.t.Helper()
	if _, err := e.s.PrepareSetup(ctx); err != nil {
		e.t.Fatal(err)
	}
	token = e.token()
	er, err := e.s.SetupEnroll(ctx, token, "admin")
	if err != nil {
		e.t.Fatal(err)
	}
	seed, err = b32().DecodeString(er.Secret)
	if err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) completion(token string, er EnrollResult, seed []byte) SetupComplete {
	return SetupComplete{Token: token, EnrollmentID: er.EnrollmentID, Username: "admin", Password: goodPassword,
		Code: TOTPCode(seed, TOTPCounter(e.clk.Now())), IPKey: "203.0.113.5", UserAgent: "test/1.0", HostKind: "ip"}
}

var ctx = context.Background()

// ---- TOTP and recovery codes ----------------------------------------------

func TestRFC6238AppendixBVectors(t *testing.T) {
	seed := []byte("12345678901234567890") // the RFC's SHA-1 seed
	for unix, want := range map[uint64]string{
		59: "94287082", 1111111109: "07081804", 1111111111: "14050471",
		1234567890: "89005924", 2000000000: "69279037", 20000000000: "65353130",
	} {
		if got := hotp(seed, unix/30, 8); got != want {
			t.Errorf("T=%d: %s, want %s", unix, got, want)
		}
	}
	// Our 6-digit code is the low six digits of the same value.
	if got := TOTPCode(seed, 59/30); got != "287082" {
		t.Errorf("6-digit code %s", got)
	}
}

func TestTOTPAcceptsOneStepEitherWayAndNoMore(t *testing.T) {
	seed, _ := NewTOTPSeed()
	now := time.Unix(1_800_000_015, 0)
	c := TOTPCounter(now)
	for d := int64(-1); d <= 1; d++ {
		got, ok := VerifyTOTP(seed, TOTPCode(seed, c+d), now)
		if !ok || got != c+d {
			t.Errorf("step %+d: %d %v", d, got, ok)
		}
	}
	for _, d := range []int64{-3, -2, 2, 3} {
		if _, ok := VerifyTOTP(seed, TOTPCode(seed, c+d), now); ok {
			t.Errorf("step %+d accepted", d)
		}
	}
	for _, bad := range []string{"", "12345", "1234567", "12345a", " 12345", "١٢٣٤٥٦"} {
		if _, ok := VerifyTOTP(seed, bad, now); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if !strings.HasPrefix(TOTPURI(seed, "admin"), "otpauth://totp/ServerDock:admin?") || !strings.Contains(TOTPURI(seed, "admin"), "secret="+TOTPSecret(seed)) {
		t.Errorf("URI %s", TOTPURI(seed, "admin"))
	}
}

func TestTOTPCounterNeverReused(t *testing.T) {
	e := newEnv(t)
	e.st.W.Exec(`INSERT INTO users (id, username, created_at) VALUES (1, 'a', 1)`)
	if err := e.s.AcceptTOTP(ctx, 1, 100); err != nil {
		t.Fatal(err)
	}
	for _, c := range []int64{100, 99, 1} {
		if err := e.s.AcceptTOTP(ctx, 1, c); !errors.Is(err, ErrCodeReused) {
			t.Errorf("counter %d: %v", c, err)
		}
	}
	if err := e.s.AcceptTOTP(ctx, 1, 101); err != nil {
		t.Fatal(err)
	}
	// Concurrent: one code, many attempts, one winner.
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e.s.AcceptTOTP(ctx, 1, 500) == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d concurrent attempts with one code succeeded", wins.Load())
	}
}

func TestRecoveryCodes(t *testing.T) {
	display, hashes, err := NewRecoveryCodes()
	if err != nil || len(display) != 10 || len(hashes) != 10 {
		t.Fatal(err, len(display))
	}
	seen := map[string]bool{}
	for _, c := range display {
		if len(c) != 19 || c[4] != '-' || c[9] != '-' || c[14] != '-' || seen[c] {
			t.Fatalf("bad or repeated code %q", c)
		}
		seen[c] = true
		norm, ok := normaliseRecovery(c)
		if !ok || len(norm) != 16 {
			t.Fatalf("%q does not normalise", c)
		}
		// Upper case, spaces and missing dashes all normalise to the same code.
		if n2, _ := normaliseRecovery(strings.ToUpper(strings.ReplaceAll(c, "-", " "))); n2 != norm {
			t.Errorf("%q normalises differently", c)
		}
	}
	for _, bad := range []string{"", "abcd", strings.Repeat("a", 16), "0000-0000-0000-0000", strings.Repeat("a", 17)} {
		if _, ok := normaliseRecovery(bad); ok && bad != strings.Repeat("a", 16) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRecoveryCodeSingleUse(t *testing.T) {
	e := newEnv(t)
	token, er, seed := e.enrolled()
	res, err := e.s.SetupComplete(ctx, e.completion(token, er, seed))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := e.s.RecoveryCodesLeft(ctx, res.UserID); n != 10 {
		t.Fatalf("%d codes left", n)
	}
	if ok, err := e.s.UseRecoveryCode(ctx, res.UserID, res.RecoveryCodes[3]); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if ok, _ := e.s.UseRecoveryCode(ctx, res.UserID, res.RecoveryCodes[3]); ok {
		t.Error("a recovery code worked twice")
	}
	if ok, _ := e.s.UseRecoveryCode(ctx, res.UserID, "aaaa-bbbb-cccc-dddd"); ok {
		t.Error("an unknown code worked")
	}
	if n, _ := e.s.RecoveryCodesLeft(ctx, res.UserID); n != 9 {
		t.Fatalf("%d codes left", n)
	}
	// The codes are stored hashed, never in the clear.
	raw, _ := os.ReadFile(filepath.Join(e.dir, "serverdock.db"))
	for _, c := range res.RecoveryCodes {
		n, _ := normaliseRecovery(c)
		if bytes.Contains(raw, []byte(n)) || bytes.Contains(raw, []byte(c)) {
			t.Fatal("a recovery code is in the database file in the clear")
		}
	}
}

func TestPasswordAndUsernameRules(t *testing.T) {
	for _, pw := range []string{"short", "elevenchars", strings.Repeat("x", 257), "admin-admin-admin"[:0] + "\xff\xfe invalid utf8 pw"} {
		if err := CheckPassword("someone", pw); !errors.Is(err, ErrWeakPassword) {
			t.Errorf("%q accepted", pw)
		}
	}
	if err := CheckPassword("Administrator1", "administrator1"); !errors.Is(err, ErrWeakPassword) {
		t.Error("the user name as the password was accepted")
	}
	for _, pw := range []string{goodPassword, "twelve chars", strings.Repeat("é", 12), strings.Repeat("x", 256)} {
		if err := CheckPassword("someone", pw); err != nil {
			t.Errorf("%q: %v", pw, err)
		}
	}
	for _, n := range []string{"admin", "Admin.User-1", "a@b.c", strings.Repeat("a", 64)} {
		if err := ValidateUsername(n); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	for _, n := range []string{"", " ", "a b", "ad\nmin", strings.Repeat("a", 65), "adm/in", "ädmin", "a'b"} {
		if err := ValidateUsername(n); !errors.Is(err, ErrBadUsername) {
			t.Errorf("%q accepted", n)
		}
	}
}

// ---- the admin, the setup token, setup --------------------------------------

func TestAdminExistsDefinition(t *testing.T) {
	e := newEnv(t)
	check := func(want bool, label string) {
		t.Helper()
		if got, err := e.s.AdminExists(ctx); err != nil || got != want {
			t.Fatalf("%s: %v %v, want %v", label, got, err, want)
		}
	}
	check(false, "no users")
	e.st.W.Exec(`INSERT INTO users (id, username, created_at) VALUES (1, 'a', 1)`)
	check(false, "a user row alone")
	e.st.W.Exec(`UPDATE users SET password_hash = 'x'`)
	check(false, "password but no confirmed TOTP")
	e.st.W.Exec(`UPDATE users SET password_hash = NULL, totp_confirmed_at = 5`)
	check(false, "confirmed TOTP but no password")
	e.st.W.Exec(`UPDATE users SET password_hash = 'x'`)
	check(true, "password and confirmed TOTP")
	// What `admin reset` leaves: the row, nothing else.
	e.st.W.Exec(`UPDATE users SET password_hash = NULL, totp_seed = NULL, totp_confirmed_at = NULL, totp_pending_seed = NULL, totp_last_counter = 0`)
	check(false, "after a reset")
}

func TestSetupTokenFileModeAndRemoval(t *testing.T) {
	e := newEnv(t)
	info, err := e.s.PrepareSetup(ctx)
	if err != nil || info == nil {
		t.Fatalf("%v %v", info, err)
	}
	path := filepath.Join(e.dir, SetupTokenFile)
	if info.TokenFile != path || !info.ExpiresAt.Equal(e.clk.Now().Add(time.Hour)) {
		t.Fatalf("%+v", info)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", st, err)
	}
	tok1 := e.token()
	if !strings.HasPrefix(tok1, "sd_setup_") || len(tok1) != len("sd_setup_")+43 {
		t.Fatalf("token format %q", tok1)
	}
	// Only the hash is stored.
	raw, _ := os.ReadFile(filepath.Join(e.dir, "serverdock.db"))
	if bytes.Contains(raw, []byte(tok1)) || bytes.Contains(raw, []byte(strings.TrimPrefix(tok1, "sd_setup_"))) {
		t.Error("the setup token is in the database in the clear")
	}
	// A new token at every start; the old one stops working.
	if _, err := e.s.PrepareSetup(ctx); err != nil {
		t.Fatal(err)
	}
	tok2 := e.token()
	if tok1 == tok2 {
		t.Fatal("the token did not change on restart")
	}
	if _, err := e.s.SetupEnroll(ctx, tok1, ""); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("an old token: %v", err)
	}
	// Nothing about the token is logged.
	if strings.Contains(e.logs.String(), tok2) || strings.Contains(e.logs.String(), strings.TrimPrefix(tok2, "sd_setup_")) {
		t.Error("the setup token was logged")
	}
	// Completing setup removes the file; a later start with an admin removes a stale file and the row.
	_, er, seed := e.enrolled()
	if _, err := e.s.SetupComplete(ctx, e.completion(e.token(), er, seed)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the token file is still there after setup")
	}
	os.WriteFile(path, []byte("stale\n"), 0o600)
	e.st.W.Exec(`INSERT INTO setup_state (id, token_hash, expires_at, created_at) VALUES (1, x'00', 1, 1)`)
	if info, err := e.s.PrepareSetup(ctx); err != nil || info != nil {
		t.Fatalf("with an admin: %v %v", info, err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a stale token file survived a start with an admin")
	}
	if e.count(`SELECT count(*) FROM setup_state`) != 0 {
		t.Error("the setup row survived a start with an admin")
	}
}

func TestSetupTokenFileWriteFailureFailsStart(t *testing.T) {
	e := newEnv(t)
	os.Mkdir(filepath.Join(e.dir, SetupTokenFile), 0o700) // a directory where the file must go
	if _, err := e.s.PrepareSetup(ctx); err == nil {
		t.Fatal("start went on without a token file")
	}
}

func TestStartupLogHasPathFingerprintAndTunnelHint(t *testing.T) {
	info := SetupInfo{TokenFile: "/var/lib/serverdock/setup-token", ExpiresAt: time.Date(2026, 11, 20, 14, 5, 0, 0, time.UTC)}
	fp := "AB:CD:EF"
	msg := StartupNotice(info, fp, "443")
	for _, want := range []string{
		"/var/lib/serverdock/setup-token", "expires 14:05 UTC", "panel certificate SHA-256 AB:CD:EF",
		"ssh -L 8443:127.0.0.1:443 you@this-server", "https://localhost:8443/setup",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in %q", want, msg)
		}
	}
	if strings.Contains(msg, "sd_setup_") {
		t.Error("the notice mentions a token")
	}
	if got := StartupNotice(info, "", ""); strings.Contains(got, "SHA-256") || !strings.Contains(got, ":443 you@") {
		t.Errorf("without a panel: %q", got)
	}
}

func TestSetupStatus(t *testing.T) {
	e := newEnv(t)
	e.s.PrepareSetup(ctx)
	st, err := e.s.SetupStatus(ctx)
	if err != nil || !st.Required || !st.ExpiresAt.Equal(e.clk.Now().Add(time.Hour).UTC()) || st.ServerTime.IsZero() {
		t.Fatalf("%+v %v", st, err)
	}
	token, er, seed := e.enrolled()
	e.s.SetupComplete(ctx, e.completion(token, er, seed))
	st, _ = e.s.SetupStatus(ctx)
	if st.Required || !st.ExpiresAt.IsZero() {
		t.Fatalf("after setup: %+v", st)
	}
}

func TestSetupHappyPathStoresWhatItShould(t *testing.T) {
	e := newEnv(t)
	token, er, seed := e.enrolled()
	res, err := e.s.SetupComplete(ctx, e.completion(token, er, seed))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.s.AdminExists(ctx); !ok {
		t.Fatal("no admin after setup")
	}
	if len(res.RecoveryCodes) != 10 || res.SessionSecret == "" || res.Session.PublicID == "" {
		t.Fatalf("%+v", res)
	}
	var host, ip, scope string
	var counter, confirmed int64
	e.st.R.QueryRow(`SELECT totp_enrolled_host, setup_ip, scope, totp_last_counter, totp_confirmed_at FROM users WHERE id = ?`, res.UserID).Scan(&host, &ip, &scope, &counter, &confirmed)
	if host != "ip" || ip != "203.0.113.5" || scope != "admin" || counter != TOTPCounter(e.clk.Now()) || confirmed == 0 {
		t.Fatalf("%s %s %s %d %d", host, ip, scope, counter, confirmed)
	}
	// The pending enrolment is gone; the seed is readable only through the box.
	if e.count(`SELECT count(*) FROM setup_state`) != 0 {
		t.Error("setup row kept")
	}
	got, err := e.s.UserSeed(ctx, res.UserID)
	if err != nil || !bytes.Equal(got, seed) {
		t.Fatalf("seed round trip: %v", err)
	}
	// The same code cannot be used again at sign-in.
	if err := e.s.AcceptTOTP(ctx, res.UserID, TOTPCounter(e.clk.Now())); !errors.Is(err, ErrCodeReused) {
		t.Errorf("the setup code could be reused: %v", err)
	}
	// The first session works.
	sess, err := e.s.LookupSession(ctx, res.SessionSecret)
	if err != nil || sess.UserID != res.UserID || sess.UserAgent != "test/1.0" {
		t.Fatalf("%+v %v", sess, err)
	}
	// A second setup is refused: an admin exists.
	if _, err := e.s.SetupEnroll(ctx, token, ""); !errors.Is(err, ErrAdminExists) {
		t.Errorf("enrol after setup: %v", err)
	}
	if _, err := e.s.SetupComplete(ctx, e.completion(token, er, seed)); !errors.Is(err, ErrAdminExists) {
		t.Errorf("complete after setup: %v", err)
	}
}

func TestTOTPSeedNotInDBPlaintext(t *testing.T) {
	e := newEnv(t)
	token, er, seed := e.enrolled()
	// While pending: sealed in setup_state.
	var pend []byte
	e.st.R.QueryRow(`SELECT pending_seed FROM setup_state`).Scan(&pend)
	if len(pend) != 1+4+12+len(seed)+16 || pend[0] != 1 || bytes.Contains(pend, seed) {
		t.Fatalf("pending seed column: %d bytes, version %d", len(pend), pend[0])
	}
	res, err := e.s.SetupComplete(ctx, e.completion(token, er, seed))
	if err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	e.st.R.QueryRow(`SELECT totp_seed FROM users WHERE id = ?`, res.UserID).Scan(&sealed)
	if len(sealed) != 1+4+12+len(seed)+16 || bytes.Contains(sealed, seed) {
		t.Fatalf("totp_seed column: %d bytes", len(sealed))
	}
	// Neither the raw seed nor its base32 text is anywhere in the database or its WAL.
	e.st.W.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	for _, name := range []string{"serverdock.db", "serverdock.db-wal"} {
		raw, _ := os.ReadFile(filepath.Join(e.dir, name))
		if bytes.Contains(raw, seed) || bytes.Contains(raw, []byte(er.Secret)) {
			t.Errorf("the TOTP seed is in %s in the clear", name)
		}
	}
	// A seed copied to another row does not open (AAD binding).
	if _, err := e.box.Open("users/999/totp_seed", sealed); err == nil {
		t.Error("a seed opened for another user id")
	}
}

func TestSetupChecksBeforeHash(t *testing.T) {
	e := newEnv(t)
	token, er, seed := e.enrolled()
	good := e.completion(token, er, seed)
	bad := map[string]func(SetupComplete) SetupComplete{
		"wrong token":     func(in SetupComplete) SetupComplete { in.Token = "sd_setup_" + strings.Repeat("x", 43); return in },
		"wrong enrolment": func(in SetupComplete) SetupComplete { in.EnrollmentID = "nope"; return in },
		"wrong code":      func(in SetupComplete) SetupComplete { in.Code = "000000"; return in },
		"weak password":   func(in SetupComplete) SetupComplete { in.Password = "short"; return in },
		"bad username":    func(in SetupComplete) SetupComplete { in.Username = "a b"; return in },
		"password == name": func(in SetupComplete) SetupComplete {
			in.Username = "administrator"
			in.Password = "Administrator"
			return in
		},
	}
	for name, mutate := range bad {
		before := e.derives.Load()
		if _, err := e.s.SetupComplete(ctx, mutate(good)); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if e.derives.Load() != before {
			t.Errorf("%s: argon2 ran before the checks", name)
		}
	}
	// And nothing was written by the failures.
	if e.count(`SELECT count(*) FROM users`) != 0 || e.count(`SELECT count(*) FROM setup_state`) != 1 {
		t.Fatal("a failed setup wrote something")
	}
	// The good one still works (the failed ones did not consume anything).
	if _, err := e.s.SetupComplete(ctx, good); err != nil {
		t.Fatal(err)
	}
	if e.derives.Load() != 1 {
		t.Errorf("%d hashes for one setup", e.derives.Load())
	}
}

func TestSetupExpiryAndReplacedEnrolment(t *testing.T) {
	e := newEnv(t)
	token, er1, seed1 := e.enrolled()
	// A second enrolment replaces the first.
	er2, err := e.s.SetupEnroll(ctx, token, "admin")
	if err != nil {
		t.Fatal(err)
	}
	seed2, _ := b32().DecodeString(er2.Secret)
	if _, err := e.s.SetupComplete(ctx, e.completion(token, er1, seed1)); !errors.Is(err, ErrEnrollmentReplaced) {
		t.Fatalf("the replaced enrolment: %v", err)
	}
	// The first client's seed does not work with the second enrolment either.
	in := e.completion(token, er2, seed1)
	if _, err := e.s.SetupComplete(ctx, in); !errors.Is(err, ErrBadCode) {
		t.Fatalf("the old seed's code: %v", err)
	}
	// Completing without enrolling at all.
	e2 := newEnv(t)
	e2.s.PrepareSetup(ctx)
	if _, err := e2.s.SetupComplete(ctx, SetupComplete{Token: e2.token(), EnrollmentID: "x", Username: "admin", Password: goodPassword, Code: "123456"}); !errors.Is(err, ErrEnrollmentReplaced) {
		t.Fatalf("no enrolment: %v", err)
	}
	// Expiry: after an hour every setup call says so (a wrong token still learns nothing).
	e.clk.Add(time.Hour + time.Second)
	if _, err := e.s.SetupEnroll(ctx, token, ""); !errors.Is(err, ErrSetupExpired) {
		t.Errorf("enrol after expiry: %v", err)
	}
	if _, err := e.s.SetupComplete(ctx, e.completion(token, er2, seed2)); !errors.Is(err, ErrSetupExpired) {
		t.Errorf("complete after expiry: %v", err)
	}
	if _, err := e.s.SetupEnroll(ctx, "wrong", ""); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("wrong token after expiry: %v", err)
	}
	if e.count(`SELECT count(*) FROM users`) != 0 {
		t.Fatal("a user was created after expiry")
	}
	// A restart issues a new token and setup works again.
	e.s.PrepareSetup(ctx)
	if _, err := e.s.SetupEnroll(ctx, e.token(), ""); err != nil {
		t.Fatalf("after a restart: %v", err)
	}
}

// A fault after any statement of the completion leaves nothing behind, the token
// and enrolment still work, and the token file is untouched.
func TestSetupCompleteIsAtomic(t *testing.T) {
	probe := newEnv(t)
	token, er, seed := probe.enrolled()
	var total int
	probe.s.afterStmt = func(n int) error { total = n; return nil }
	if _, err := probe.s.SetupComplete(ctx, probe.completion(token, er, seed)); err != nil {
		t.Fatal(err)
	}
	if total < 15 { // delete, insert, update, delete codes, 10 codes, 4 session statements...
		t.Fatalf("only %d statements counted: the hook misses some", total)
	}
	for k := 1; k <= total; k++ {
		e := newEnv(t)
		token, er, seed := e.enrolled()
		e.s.afterStmt = func(n int) error {
			if n == k {
				return fmt.Errorf("injected fault after statement %d", n)
			}
			return nil
		}
		if _, err := e.s.SetupComplete(ctx, e.completion(token, er, seed)); err == nil {
			t.Fatalf("fault %d was swallowed", k)
		}
		for _, table := range []string{"users", "recovery_codes", "sessions", "auth_known_ips"} {
			if n := e.count(`SELECT count(*) FROM ` + table); n != 0 {
				t.Fatalf("fault %d left %d rows in %s", k, n, table)
			}
		}
		if ok, _ := e.s.AdminExists(ctx); ok {
			t.Fatalf("fault %d: an admin exists", k)
		}
		if e.count(`SELECT count(*) FROM setup_state WHERE enrollment_hash IS NOT NULL AND pending_seed IS NOT NULL`) != 1 {
			t.Fatalf("fault %d consumed the setup row", k)
		}
		if _, err := os.Stat(filepath.Join(e.dir, SetupTokenFile)); err != nil {
			t.Fatalf("fault %d removed the token file", k)
		}
		e.s.afterStmt = nil
		if _, err := e.s.SetupComplete(ctx, e.completion(token, er, seed)); err != nil {
			t.Fatalf("fault %d: the retry failed: %v", k, err)
		}
	}
}

func TestConcurrentSetupOneWinner(t *testing.T) {
	e := newEnv(t)
	token, er, seed := e.enrolled()
	in := e.completion(token, er, seed)
	var wg sync.WaitGroup
	results := make(chan error, 20)
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := e.s.SetupComplete(ctx, in)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	ok, notFound, conflict := 0, 0, 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrAdminExists):
			notFound++ // 404
		case errors.Is(err, ErrTokenConsumed), errors.Is(err, ErrTokenInvalid), errors.Is(err, ErrEnrollmentReplaced):
			conflict++ // 409 (or the row was already gone)
		default:
			t.Errorf("unexpected: %v", err)
		}
	}
	if ok != 1 || ok+notFound+conflict != 20 {
		t.Fatalf("ok=%d admin-exists=%d consumed=%d", ok, notFound, conflict)
	}
	if e.count(`SELECT count(*) FROM users`) != 1 || e.count(`SELECT count(*) FROM sessions`) != 1 || e.count(`SELECT count(*) FROM recovery_codes`) != 10 {
		t.Fatal("more than one admin's worth of rows")
	}
}

// After `admin reset` the user row is kept and setup reuses it (and its id).
func TestSetupReusesAResetUserRow(t *testing.T) {
	e := newEnv(t)
	token, er, seed := e.enrolled()
	res, err := e.s.SetupComplete(ctx, e.completion(token, er, seed))
	if err != nil {
		t.Fatal(err)
	}
	e.st.W.Exec(`UPDATE users SET password_hash = NULL, totp_seed = NULL, totp_confirmed_at = NULL, totp_last_counter = 0`)
	e.st.W.Exec(`DELETE FROM sessions`)
	e.clk.Add(time.Minute)
	token, er, seed = e.enrolled()
	in := e.completion(token, er, seed)
	in.Username = "newname"
	res2, err := e.s.SetupComplete(ctx, in)
	if err != nil || res2.UserID != res.UserID {
		t.Fatalf("%v: id %d, want %d", err, res2.UserID, res.UserID)
	}
	if e.count(`SELECT count(*) FROM users`) != 1 {
		t.Fatal("a second user row")
	}
}

func TestWrongKeyIDClearError(t *testing.T) {
	e := newEnv(t)
	token, er, seed := e.enrolled()
	if _, err := e.s.SetupComplete(ctx, e.completion(token, er, seed)); err != nil {
		t.Fatal(err)
	}
	// The same data, a different key.
	other, _ := secretbox.New(bytes.Repeat([]byte{9}, 32))
	var logs lockedBuf
	s2 := NewStore(e.st, other, e.s.hasher, Options{Now: e.clk.Now, Log: slog.New(slog.NewTextHandler(&logs, nil))})
	_, err := s2.UserSeed(ctx, 1)
	if !errors.Is(err, ErrKeyMismatch) || !strings.Contains(err.Error(), "admin reset") {
		t.Fatalf("%v", err)
	}
	if out := logs.String(); !strings.Contains(out, "table=users") || !strings.Contains(out, "column=totp_seed") {
		t.Errorf("the log does not name the table and column: %s", out)
	}
	for _, b := range [][]byte{seed, bytes.Repeat([]byte{9}, 32), bytes.Repeat([]byte{7}, 32)} {
		if bytes.Contains([]byte(logs.String()), b) {
			t.Error("key or seed bytes in the log")
		}
	}
	// And with a pending enrolment (setup_state.pending_seed).
	e2 := newEnv(t)
	token2, er2, seed2 := e2.enrolled()
	s3 := NewStore(e2.st, other, e2.s.hasher, Options{Now: e2.clk.Now, Log: slog.New(slog.NewTextHandler(&logs, nil))})
	if _, err := s3.SetupComplete(ctx, e2.completion(token2, er2, seed2)); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("setup with the wrong key: %v", err)
	}
	if !strings.Contains(logs.String(), "table=setup_state") {
		t.Errorf("log: %s", logs.String())
	}
}
