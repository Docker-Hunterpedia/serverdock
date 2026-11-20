package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/auth"
	"github.com/Docker-Hunterpedia/serverdock/internal/limits"
	"github.com/Docker-Hunterpedia/serverdock/internal/redact"
)

// AuditSink is what the pre-session endpoints need from the audit log: the
// intent/outcome appender, and the rate-limited denied rows.
type AuditSink interface {
	audit.Appender
	Denied(ctx context.Context, e audit.Entry) (bool, error)
}

// Limits are the four login back-offs (docs/plans/0003, "Login protection"), each
// with its own cap so one cannot starve another.
type Limits struct {
	IP64            *limits.Backoff // IPv4 address or IPv6 /64: setup token, password, 2FA and recovery codes
	IP48            *limits.Backoff // IPv6 /48, checked alongside IP64
	AccountPassword *limits.Backoff // per user name (one shared key for every name that does not exist)
	Account2FA      *limits.Backoff // per user id: 2FA, step-up
}

// NewLimits builds the four instances: 5 free failures then 1 s doubling to 15 min
// (IP64); 20 then the same (IP48); 10 then the same (AccountPassword); 5 then 2 s
// doubling to 1 h (Account2FA). now may be nil.
func NewLimits(now func() time.Time) *Limits {
	return &Limits{
		IP64:            limits.NewBackoff(5, time.Second, 15*time.Minute, now),
		IP48:            limits.NewBackoff(20, time.Second, 15*time.Minute, now),
		AccountPassword: limits.NewBackoff(10, time.Second, 15*time.Minute, now),
		Account2FA:      limits.NewBackoff(5, 2*time.Second, time.Hour, now),
	}
}

// PasswordHasher is what the sign-in handler needs from auth.Hasher (an interface
// so tests can count calls).
type PasswordHasher interface {
	Verify(ctx context.Context, phc, password string) (bool, error)
	VerifyDummy(ctx context.Context, password string) error
}

// AuthDeps are the dependencies of the pre-session endpoints.
type AuthDeps struct {
	Auth *auth.Store
	// Hasher is the process-wide password hasher (an *auth.Hasher): one semaphore
	// for every caller.
	Hasher PasswordHasher
	Audit  AuditSink
	Limits *Limits
	Log    *slog.Logger
}

type authAPI struct {
	AuthDeps
}

// NewAuthHandlers returns the setup, sign-in, step-up and sign-out handlers.
func NewAuthHandlers(d AuthDeps) *AuthHandlers {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Limits == nil {
		d.Limits = NewLimits(nil)
	}
	a := &authAPI{d}
	return &AuthHandlers{
		SetupStatus: http.HandlerFunc(a.setupStatus), SetupEnroll: http.HandlerFunc(a.setupEnroll), SetupComplete: http.HandlerFunc(a.setupComplete),
		Login: http.HandlerFunc(a.login), TwoFA: http.HandlerFunc(a.twoFA), SessionInfo: http.HandlerFunc(a.sessionInfo),
		Reauth: http.HandlerFunc(a.reauth), Logout: http.HandlerFunc(a.logout),
	}
}

// ---- helpers ------------------------------------------------------------------

const maxBody = 64 << 10

// decode reads one JSON object, refusing unknown fields and trailing data. Error
// messages never echo the body (it may hold a password).
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_input", "the request body is not valid for this endpoint")
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		WriteError(w, http.StatusBadRequest, "invalid_input", "the request body is not valid for this endpoint")
		return false
	}
	return true
}

func retryAfter(d time.Duration) string {
	return strconv.Itoa(int(max(1, (d+time.Second-1)/time.Second)))
}

func tooMany(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", retryAfter(wait))
	WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many attempts; try again in "+retryAfter(wait)+" seconds")
}

func busy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "2")
	WriteError(w, http.StatusServiceUnavailable, "busy", "the server is busy checking passwords; try again in a moment")
}

func (a *authAPI) auditDown(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "2")
	WriteError(w, http.StatusServiceUnavailable, "audit_unavailable", "the server is busy or the audit log is not writable; nothing was done")
}

// checkIP applies the per-IP back-offs; it answers 429 and returns false when
// blocked. known IPs (a full sign-in in the last 30 days) are admitted even when
// the limiter maps are full.
func (a *authAPI) checkIP(w http.ResponseWriter, r *http.Request, known bool) bool {
	wait := max(a.Limits.IP64.Check(limits.ClientIP(r), known), a.Limits.IP48.Check(limits.ClientIP48(r), known))
	if wait > 0 {
		tooMany(w, wait)
		return false
	}
	return true
}

func (a *authAPI) failIP(r *http.Request, known bool) {
	a.Limits.IP64.Fail(limits.ClientIP(r), known)
	a.Limits.IP48.Fail(limits.ClientIP48(r), known)
}

func (a *authAPI) resetIP(r *http.Request) {
	a.Limits.IP64.Reset(limits.ClientIP(r))
	a.Limits.IP48.Reset(limits.ClientIP48(r))
}

// hygiene clears a presented session cookie that is stale (it counts as no
// credential); a valid one is left for the handler to revoke when it mints a new
// session.
func (a *authAPI) hygiene(w http.ResponseWriter, r *http.Request, p *Principal) {
	if p.OldSessionSecret == "" {
		return
	}
	var se *auth.SessionError
	if _, err := a.Auth.LookupSession(r.Context(), p.OldSessionSecret); errors.As(err, &se) {
		ClearCookie(w, SessionCookie)
	}
}

func hostKind(host string) string {
	h := host
	if strings.HasPrefix(h, "[") {
		return "ip"
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return "ip"
	}
	if strings.EqualFold(h, "localhost") {
		return "localhost"
	}
	return "name"
}

func argsJSON(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func userActor(id int64) (kind, actorID string) { return "user", "user:" + strconv.FormatInt(id, 10) }

// denied records a failed attempt. The actor is anonymous, one id per action, so
// the denied-row limiter bounds rows per action rather than per IP; the IP is in
// the arguments. username is recorded only when that user exists.
func (a *authAPI) denied(ctx context.Context, action, username string, r *http.Request, method string) {
	if username == "" {
		username = "<unknown>"
	}
	_, err := a.Audit.Denied(ctx, audit.Entry{
		ActorKind: "anonymous", ActorID: "anonymous:" + action, Action: action, Target: username, Risk: "mutate",
		ArgsJSON: argsJSON(map[string]any{"username": username, "ip_key": limits.ClientIP(r), "method": method}), RequestID: newRequestID(),
		Error: "denied",
	})
	if err != nil {
		a.Log.Warn("could not write a denied audit row", "action", action)
	}
}

// execute runs fn between an intent row and an outcome row (intent first: if the
// intent cannot be written, fn is not run).
func execute[T any](ctx context.Context, a *authAPI, actorKind, actorID, action, target string, args map[string]any, fn func(context.Context) (T, error)) (audit.Outcome[T], error) {
	return audit.Execute(ctx, a.Audit, a.Log, redact.New(), audit.Entry{
		ActorKind: actorKind, ActorID: actorID, Action: action, Target: target, Risk: "mutate",
		ArgsJSON: argsJSON(args), RequestID: newRequestID(),
	}, fn)
}

const sessionMaxAge = int(auth.SessionAbsolute / time.Second)

func (a *authAPI) startSession(w http.ResponseWriter, secret string) string {
	SetCookie(w, SessionCookie, secret, sessionMaxAge)
	return CSRFToken(secret)
}

// ---- setup ----------------------------------------------------------------------

func (a *authAPI) setupStatus(w http.ResponseWriter, r *http.Request) {
	st, err := a.Auth.SetupStatus(r.Context())
	if err != nil {
		a.Log.Error("setup status failed", "err", err.Error())
		WriteError(w, http.StatusInternalServerError, "internal", "could not read the setup state")
		return
	}
	out := map[string]any{"setup_required": st.Required, "server_time": st.ServerTime.Format(time.RFC3339)}
	if st.Required {
		// Never the certificate fingerprint here: an on-path attacker could show any
		// value on an unauthenticated page. The token's location is not secret.
		out["token_file"] = a.Auth.TokenFile()
		if !st.ExpiresAt.IsZero() {
			out["token_expires_at"] = st.ExpiresAt.Format(time.RFC3339)
		}
	}
	WriteJSON(w, http.StatusOK, out)
}

// setupError maps the setup errors to responses and counts a wrong token against
// the per-IP back-off.
func (a *authAPI) setupError(w http.ResponseWriter, r *http.Request, err error, action, username string) {
	switch {
	case errors.Is(err, auth.ErrAdminExists):
		WriteError(w, http.StatusNotFound, "not_found", "no such route")
	case errors.Is(err, auth.ErrTokenInvalid):
		a.failIP(r, false)
		a.denied(r.Context(), action, "", r, "setup-token")
		WriteError(w, http.StatusUnauthorized, "invalid_token", "That setup token is wrong. Copy it again from the file on the server.")
	case errors.Is(err, auth.ErrSetupExpired):
		WriteError(w, http.StatusForbidden, "setup_token_expired", "The setup token has expired. Restart ServerDock to get a new one.")
	case errors.Is(err, auth.ErrTokenConsumed):
		WriteError(w, http.StatusConflict, "token_consumed", "That setup token was already used.")
	case errors.Is(err, auth.ErrEnrollmentReplaced):
		WriteError(w, http.StatusConflict, "enrollment_replaced", "This enrolment was replaced; scan the new code.")
	case errors.Is(err, auth.ErrBadCode):
		a.failIP(r, false)
		a.denied(r.Context(), action, "", r, "totp")
		WriteError(w, http.StatusUnauthorized, "invalid_code", "That code is wrong. Check your device's clock and try the next code.")
	case errors.Is(err, auth.ErrWeakPassword), errors.Is(err, auth.ErrBadUsername):
		WriteError(w, http.StatusBadRequest, "invalid_input", userMessage(err))
	case errors.Is(err, auth.ErrKeyMismatch):
		WriteError(w, http.StatusInternalServerError, "key_mismatch", err.Error())
	case errors.Is(err, auth.ErrBusy):
		busy(w)
	default:
		a.Log.Error("setup failed", "action", action, "err", err.Error())
		WriteError(w, http.StatusInternalServerError, "internal", "setup failed; nothing was changed")
	}
}

// userMessage is the explanation inside a validation error (never the input).
func userMessage(err error) string {
	parts := strings.Split(err.Error(), "\n")
	return parts[len(parts)-1]
}

func (a *authAPI) rejectTokenInURL(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	for _, k := range []string{"token", "setup_token", "access_token"} {
		if q.Has(k) {
			WriteError(w, http.StatusBadRequest, "invalid_input", "send the setup token in the request body, never in the URL")
			return true
		}
	}
	return false
}

func (a *authAPI) setupEnroll(w http.ResponseWriter, r *http.Request) {
	if a.rejectTokenInURL(w, r) {
		return
	}
	var in struct {
		Token    string `json:"token"`
		Username string `json:"username"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := auth.ValidateUsername(in.Username); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_input", userMessage(err))
		return
	}
	if !a.checkIP(w, r, false) {
		return
	}
	// The account label in the authenticator app is the validated user name.
	res, err := a.Auth.SetupEnroll(r.Context(), in.Token, in.Username)
	if err != nil {
		a.setupError(w, r, err, "auth.setup", "")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]string{"enrollment_id": res.EnrollmentID, "secret": res.Secret, "otpauth_uri": res.URI})
}

func (a *authAPI) setupComplete(w http.ResponseWriter, r *http.Request) {
	if a.rejectTokenInURL(w, r) {
		return
	}
	p := PrincipalFrom(r.Context())
	var in struct {
		Token        string `json:"token"`
		EnrollmentID string `json:"enrollment_id"`
		Username     string `json:"username"`
		Password     string `json:"password"`
		Code         string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	a.hygiene(w, r, p)
	if !a.checkIP(w, r, false) {
		return
	}
	req := auth.SetupComplete{Token: in.Token, EnrollmentID: in.EnrollmentID, Username: in.Username, Password: in.Password, Code: in.Code,
		IPKey: limits.ClientIP(r), UserAgent: r.UserAgent(), HostKind: hostKind(r.Host)}
	// Read-only checks first: a bad request is refused (and counted, and recorded
	// as a denial) before any audit intent or argon2 work.
	if err := a.Auth.PrecheckSetupComplete(r.Context(), req); err != nil {
		a.setupError(w, r, err, "auth.setup", "")
		return
	}
	if p.OldSessionSecret != "" { // revoke an old session before minting anything
		_ = a.Auth.RevokeSession(r.Context(), p.OldSessionSecret)
		ClearCookie(w, SessionCookie)
	}
	out, err := execute(r.Context(), a, "anonymous", "anonymous:auth.setup", "auth.setup", in.Username,
		map[string]any{"username": in.Username, "ip_key": req.IPKey, "method": "setup-token"},
		func(ctx context.Context) (auth.SetupResult, error) { return a.Auth.SetupComplete(ctx, req) })
	if errors.Is(err, audit.ErrAuditUnavailable) {
		a.auditDown(w)
		return
	}
	if err != nil && !errors.Is(err, audit.ErrAuditOutcome) || err != nil && out.Value.SessionSecret == "" {
		a.setupError(w, r, err, "auth.setup", "")
		return
	}
	a.resetIP(r)
	res := out.Value
	// Logged and stored (users.setup_ip, setup_at): whoever set the server up can be
	// compared with who was meant to. Never the password, token or code.
	a.Log.Warn("admin account created", "username", in.Username, "from", req.IPKey, "via", req.HostKind)
	resp := map[string]any{"username": in.Username, "recovery_codes": res.RecoveryCodes, "csrf": a.startSession(w, res.SessionSecret)}
	if err != nil { // the admin exists, but its audit outcome row was not written
		resp["audit"] = "unrecorded"
	}
	WriteJSON(w, http.StatusOK, resp)
}

// ---- sign in --------------------------------------------------------------------

func (a *authAPI) login(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	a.hygiene(w, r, p)
	ctx := r.Context()
	u, exists, err := auth.UserRecord{}, false, error(nil)
	if auth.ValidateUsername(in.Username) == nil {
		u, exists, err = a.Auth.UserByName(ctx, in.Username)
		if err != nil {
			a.Log.Error("user lookup failed", "err", err.Error())
			WriteError(w, http.StatusInternalServerError, "internal", "could not sign in")
			return
		}
	}
	real := exists && u.Admin // a reset user signs in like one that does not exist
	known := false
	if real {
		known, _ = a.Auth.KnownIP(ctx, u.ID, limits.ClientIP(r))
	}
	// Order: IP back-offs, account back-off (unless a known IP), hash semaphore, argon2.
	if !a.checkIP(w, r, known) {
		return
	}
	acct := "<unknown>" // one shared bucket for every name that does not exist
	if real {
		acct = "user:" + strings.ToLower(u.Username)
	}
	if !known {
		if wait := a.Limits.AccountPassword.Check(acct, false); wait > 0 {
			tooMany(w, wait)
			return
		}
	}
	var ok bool
	if real {
		ok, err = a.Hasher.Verify(ctx, u.PasswordHash, in.Password)
	} else {
		err = a.Hasher.VerifyDummy(ctx, in.Password)
	}
	switch {
	case errors.Is(err, auth.ErrBusy):
		busy(w)
		return
	case err != nil:
		a.Log.Error("password check failed", "err", err.Error())
		WriteError(w, http.StatusInternalServerError, "internal", "could not sign in")
		return
	}
	if !ok {
		a.failIP(r, known)
		a.Limits.AccountPassword.Fail(acct, false)
		name := ""
		if real {
			name = u.Username
		}
		a.denied(ctx, "auth.login", name, r, "password")
		WriteError(w, http.StatusUnauthorized, "invalid_credentials", "Wrong username or password. Check both and try again. Forgot it? Ask the server's operator to run `serverdock admin reset`.")
		return
	}
	// Correct password: a pending-2FA token, never a session.
	kind, id := userActor(u.ID)
	out, err := execute(ctx, a, kind, id, "auth.login", u.Username, map[string]any{"username": u.Username, "ip_key": limits.ClientIP(r), "method": "password"},
		func(ctx context.Context) (string, error) { return a.Auth.CreatePending(ctx, u.ID, limits.ClientIP(r)) })
	if errors.Is(err, audit.ErrAuditUnavailable) {
		a.auditDown(w)
		return
	}
	if out.Value == "" {
		a.Log.Error("could not start the second factor", "err", fmt.Sprint(err))
		WriteError(w, http.StatusInternalServerError, "internal", "could not sign in")
		return
	}
	if p.OldSessionSecret != "" { // a session presented at login is revoked before anything is minted
		_ = a.Auth.RevokeSession(ctx, p.OldSessionSecret)
		ClearCookie(w, SessionCookie)
	}
	a.Limits.AccountPassword.Reset(acct)
	SetCookie(w, PendingCookie, out.Value, int(auth.PendingTTL/time.Second))
	WriteJSON(w, http.StatusOK, map[string]any{"two_factor_required": true, "expires_in": int(auth.PendingTTL / time.Second)})
}

func (a *authAPI) twoFA(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	var in struct {
		Code         string `json:"code"`
		RecoveryCode string `json:"recovery_code"`
	}
	if !decode(w, r, &in) {
		return
	}
	if (in.Code == "") == (in.RecoveryCode == "") {
		WriteError(w, http.StatusBadRequest, "invalid_input", "send either code or recovery_code")
		return
	}
	a.hygiene(w, r, p) // a stale session cookie is cleared; a valid one is revoked on success
	ctx := r.Context()
	known, _ := a.Auth.KnownIP(ctx, p.PendingUserID, limits.ClientIP(r))
	if !a.checkIP(w, r, known) {
		return
	}
	acct := "user:" + strconv.FormatInt(p.PendingUserID, 10)
	if wait := a.Limits.Account2FA.Check(acct, false); wait > 0 {
		tooMany(w, wait)
		return
	}
	method := "totp"
	if in.RecoveryCode != "" {
		method = "recovery"
	}
	username, _ := a.Auth.Username(ctx, p.PendingUserID)
	kind, id := userActor(p.PendingUserID)
	out, err := execute(ctx, a, kind, id, "auth.login", username, map[string]any{"username": username, "ip_key": limits.ClientIP(r), "method": method},
		func(ctx context.Context) (auth.LoginResult, error) {
			return a.Auth.CompleteLogin(ctx, auth.LoginInput{PendingSecret: p.PendingSecret, Code: in.Code, RecoveryCode: in.RecoveryCode,
				OldSessionSecret: p.OldSessionSecret, IPKey: limits.ClientIP(r), UserAgent: r.UserAgent()})
		})
	if errors.Is(err, audit.ErrAuditUnavailable) {
		a.auditDown(w)
		return
	}
	unrecorded := errors.Is(err, audit.ErrAuditOutcome) && out.Value.SessionSecret != ""
	if err != nil && !unrecorded {
		var le *auth.LoginError
		tries := -1
		if errors.As(err, &le) {
			tries = le.TriesLeft
		}
		switch {
		case errors.Is(err, auth.ErrBadCode):
			a.failIP(r, known)
			a.Limits.Account2FA.Fail(acct, false)
			a.denied(ctx, "auth.login", username, r, method)
			WriteErrorFields(w, http.StatusUnauthorized, "invalid_code", "That code is wrong. Check your device's clock and try the next code.", map[string]any{"tries_left": tries})
		case errors.Is(err, auth.ErrCodeReused):
			WriteError(w, http.StatusUnauthorized, "code_reused", "That code was already used. Wait for the next one.")
		case errors.Is(err, auth.ErrPendingInvalid):
			ClearCookie(w, PendingCookie)
			WriteError(w, http.StatusUnauthorized, "unauthenticated", "This step expired or ran out of tries. Sign in again.")
		case errors.Is(err, auth.ErrKeyMismatch):
			WriteError(w, http.StatusInternalServerError, "key_mismatch", err.Error())
		default:
			a.Log.Error("second factor failed", "err", err.Error())
			WriteError(w, http.StatusInternalServerError, "internal", "could not sign in")
		}
		return
	}
	a.resetIP(r)
	a.Limits.Account2FA.Reset(acct)
	a.Limits.AccountPassword.Reset("user:" + strings.ToLower(username))
	res := out.Value
	ClearCookie(w, PendingCookie)
	resp := map[string]any{"username": username, "csrf": a.startSession(w, res.SessionSecret), "notice": res.Session.Notice,
		"mfa_fresh_until": res.Session.MFAFreshUntil.Format(time.RFC3339)}
	if unrecorded {
		resp["audit"] = "unrecorded"
	}
	WriteJSON(w, http.StatusOK, resp)
}

// ---- session ------------------------------------------------------------------------

func (a *authAPI) sessionInfo(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	s := p.Session
	name, _ := a.Auth.Username(r.Context(), s.UserID)
	left, _ := a.Auth.RecoveryCodesLeft(r.Context(), s.UserID)
	WriteJSON(w, http.StatusOK, map[string]any{
		"user": map[string]any{"id": s.UserID, "username": name}, "public_id": s.PublicID, "csrf": CSRFToken(p.SessionSecret),
		"mfa_fresh_until": s.MFAFreshUntil.Format(time.RFC3339), "idle_expires_at": s.IdleExpiresAt.Format(time.RFC3339),
		"abs_expires_at": s.AbsExpiresAt.Format(time.RFC3339), "notice": s.Notice, "recovery_codes_left": left,
	})
}

func (a *authAPI) reauth(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	var in struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	ctx := r.Context()
	uid := p.Session.UserID
	if !a.checkIP(w, r, false) {
		return
	}
	acct := "user:" + strconv.FormatInt(uid, 10)
	if wait := a.Limits.Account2FA.Check(acct, false); wait > 0 {
		tooMany(w, wait)
		return
	}
	kind, id := userActor(uid)
	username, _ := a.Auth.Username(ctx, uid)
	out, err := execute(ctx, a, kind, id, "auth.reauth", username, map[string]any{"username": username, "ip_key": limits.ClientIP(r), "method": "totp"},
		func(ctx context.Context) (time.Time, error) { return a.Auth.Reauth(ctx, p.SessionSecret, in.Code) })
	if errors.Is(err, audit.ErrAuditUnavailable) {
		a.auditDown(w)
		return
	}
	unrecorded := errors.Is(err, audit.ErrAuditOutcome) && !out.Value.IsZero()
	if err != nil && !unrecorded {
		var re *auth.ReauthError
		var se *auth.SessionError
		switch {
		case errors.As(err, &re) && errors.Is(err, auth.ErrSessionRevoked):
			a.failIP(r, false)
			a.Limits.Account2FA.Fail(acct, false)
			ClearCookie(w, SessionCookie)
			WriteError(w, http.StatusUnauthorized, "revoked", "Too many wrong codes: this session was signed out.")
		case errors.As(err, &re) && errors.Is(err, auth.ErrBadCode):
			a.failIP(r, false)
			a.Limits.Account2FA.Fail(acct, false)
			a.denied(ctx, "auth.reauth", username, r, "totp")
			WriteErrorFields(w, http.StatusForbidden, "invalid_code", "That code is wrong.", map[string]any{"failures_left": re.FailuresLeft})
		case errors.Is(err, auth.ErrCodeReused):
			WriteError(w, http.StatusForbidden, "code_reused", "That code was already used. Wait for the next one.")
		case errors.As(err, &se):
			ClearCookie(w, SessionCookie)
			WriteError(w, http.StatusUnauthorized, se.Reason, "your session is no longer valid; sign in again")
		default:
			a.Log.Error("step-up failed", "err", err.Error())
			WriteError(w, http.StatusInternalServerError, "internal", "could not check the code")
		}
		return
	}
	a.resetIP(r)
	a.Limits.Account2FA.Reset(acct)
	resp := map[string]any{"mfa_fresh_until": out.Value.Format(time.RFC3339)}
	if unrecorded {
		resp["audit"] = "unrecorded"
	}
	WriteJSON(w, http.StatusOK, resp)
}

func (a *authAPI) logout(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	kind, id := userActor(p.Session.UserID)
	username, _ := a.Auth.Username(r.Context(), p.Session.UserID)
	_, err := execute(r.Context(), a, kind, id, "auth.logout", username, map[string]any{"username": username, "ip_key": limits.ClientIP(r), "method": "session"},
		func(ctx context.Context) (struct{}, error) {
			return struct{}{}, a.Auth.RevokeSession(ctx, p.SessionSecret)
		})
	resp := map[string]any{"ok": true}
	if errors.Is(err, audit.ErrAuditUnavailable) {
		// An action that only removes access always runs: refusing to sign someone
		// out because the audit log is down would be worse than a gap in the log.
		a.Log.Error("logout ran without an audit record: the audit intent could not be written", "user_id", p.Session.UserID)
		if rerr := a.Auth.RevokeSession(r.Context(), p.SessionSecret); rerr != nil {
			a.Log.Error("logout failed", "err", rerr.Error())
			WriteError(w, http.StatusInternalServerError, "internal", "could not sign out")
			return
		}
		ClearCookie(w, SessionCookie)
		resp["audit"] = "unrecorded"
		WriteJSON(w, http.StatusOK, resp)
		return
	}
	ClearCookie(w, SessionCookie)
	if errors.Is(err, audit.ErrAuditOutcome) {
		resp["audit"] = "unrecorded"
	} else if err != nil {
		a.Log.Error("logout failed", "err", err.Error())
		WriteError(w, http.StatusInternalServerError, "internal", "could not sign out")
		return
	}
	WriteJSON(w, http.StatusOK, resp)
}
