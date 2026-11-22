package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/auth"
	"github.com/Docker-Hunterpedia/serverdock/internal/hub"
)

// ActionRunner is the action registry as the REST layer sees it. The API package
// is never given the one-time system invoker: it can only invoke as a user session or an
// API token, so no HTTP request can run as the system actor.
type ActionRunner interface {
	Invoke(ctx context.Context, actor actions.Actor, name string, raw json.RawMessage, opts actions.Options) (actions.Result, error)
	Lookup(name string) (actions.Info, bool)
	List() []actions.Info
}

// verifyLimit is the hard-coded rate limit of audit.verify over REST (a full scan
// of the audit chain): 2 calls a minute per actor.
const verifyLimitPerMinute = 2

func (s *Server) restRoutes() []Route {
	if s.cfg.Actions == nil {
		return nil
	}
	return []Route{
		{Method: "POST", Pattern: "/api/v1/actions/{name}", Auth: SessionOrToken, Handler: http.HandlerFunc(s.runAction)},
		{Method: "GET", Pattern: "/api/v1/openapi.json", Auth: SessionOrToken, Handler: http.HandlerFunc(s.openapi)},
	}
}

func (s *Server) openapi(w http.ResponseWriter, _ *http.Request) {
	doc, err := BuildOpenAPI(s.cfg.Actions.List())
	if err != nil {
		s.log.Error("openapi generation failed", "err", err.Error())
		WriteError(w, http.StatusInternalServerError, "internal", "could not build the API description")
		return
	}
	WriteJSON(w, http.StatusOK, doc) // generated per request, not cached
}

func tokenScope(s string) (actions.Scope, bool) {
	switch s {
	case "read":
		return actions.ScopeRead, true
	case "write":
		return actions.ScopeWrite, true
	case "admin":
		return actions.ScopeAdmin, true
	}
	return 0, false
}

// actorFor maps the request's principal to an action actor. Only a session or a
// token can become one: never the system, MCP or webhook actor.
func actorFor(r *http.Request, p *Principal) (actions.Actor, bool) {
	switch p.Kind {
	case SessionPrincipal:
		// One admin in M2: every session has admin scope.
		a := actions.UserSessionActor(strconv.FormatInt(p.Session.UserID, 10), p.Session.PublicID, actions.ScopeAdmin, p.Session.MFAFreshUntil)
		return a.WithHostKind(hostKind(r.Host)), true // host_kind comes from r.Host, never from the body
	case TokenPrincipal:
		sc, ok := tokenScope(p.Token.Scope)
		if !ok {
			return actions.Actor{}, false
		}
		return actions.TokenActor(strconv.FormatInt(p.Token.ID, 10), sc), true
	}
	return actions.Actor{}, false
}

// runAction is POST /api/v1/actions/{name}: the generic REST handler over the
// registry. The registry does the policy, the audit and the redaction; this layer
// authenticates (the middleware), builds the actor, and maps the outcome.
func (s *Server) runAction(w http.ResponseWriter, r *http.Request) {
	reqID := newRequestID()
	fail := func(status int, code, msg string) {
		var b errBody
		b.Error.Code, b.Error.Message, b.Error.RequestID = code, msg, reqID
		WriteJSON(w, status, b)
	}
	name := r.PathValue("name")
	info, ok := s.cfg.Actions.Lookup(name)
	if !ok || info.ConsoleOnly || info.JobOnly {
		// A console-only or job-only action is not an HTTP action at all: 404, like an unknown one.
		fail(http.StatusNotFound, "unknown_action", "no such action")
		return
	}
	p := PrincipalFrom(r.Context())
	actor, ok := actorFor(r, p)
	if !ok {
		fail(http.StatusUnauthorized, "unauthenticated", "sign in or use an API token")
		return
	}
	if name == "audit.verify" { // hard-coded: a full chain scan, 2 a minute per actor
		if !s.verifyLimiter.Take(string(actor.Kind()) + ":" + actor.ID()) {
			w.Header().Set("Retry-After", "30")
			fail(http.StatusTooManyRequests, "rate_limited", "audit.verify runs at most twice a minute")
			return
		}
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, actions.MaxInputBytes+1))
	if err != nil || len(raw) > actions.MaxInputBytes {
		fail(http.StatusBadRequest, "invalid_input", "the request body is larger than "+strconv.Itoa(actions.MaxInputBytes)+" bytes")
		return
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = []byte("{}")
	}
	// SD-Dry-Run: absent means execute, exactly "1" means dry run, anything else
	// ("true", "yes", "1 ", a repeated header) is refused: a typo must never turn
	// a dry run into a real run, nor the reverse.
	dry := false
	switch v := r.Header.Values("SD-Dry-Run"); {
	case len(v) == 0:
	case len(v) == 1 && v[0] == "1":
		dry = true
	default:
		fail(http.StatusBadRequest, "invalid_input", "SD-Dry-Run must be absent or exactly 1")
		return
	}
	if len(r.Header.Values("SD-Confirm")) > 1 { // the target is free text, but one value only
		fail(http.StatusBadRequest, "invalid_input", "send SD-Confirm once")
		return
	}
	opts := actions.Options{
		DryRun:    dry,
		Confirm:   r.Header.Get("SD-Confirm"),
		RequestID: reqID, // always generated here, never taken from the client
	}
	res, err := s.cfg.Actions.Invoke(r.Context(), actor, name, raw, opts)

	unrecorded := false
	switch {
	case err == nil:
	case errors.Is(err, audit.ErrAuditOutcome) && (res.Out != nil || res.Plan != nil):
		// The action ran; only its audit record is missing. Success, flagged.
		unrecorded = true
	default:
		s.actionError(w, reqID, err)
		return
	}

	// Revoking the caller's own session (password or authenticator change, or a
	// session revoke that names this very session) ends the cookie too.
	if p.Kind == SessionPrincipal && !opts.DryRun && clearsOwnSession(name, raw, p.Session.PublicID) {
		ClearCookie(w, SessionCookie)
	}

	resp := map[string]any{"request_id": reqID}
	if opts.DryRun {
		resp["plan"] = res.Plan
	} else {
		resp["data"] = res.Out
	}
	switch {
	case unrecorded:
		resp["audit"] = "unrecorded"
	case res.Audited && res.AuditOutcomeRecorded:
		resp["audit"] = "recorded"
	case res.Audited:
		resp["audit"] = "unrecorded"
	default:
		resp["audit"] = "not_audited"
	}
	WriteJSON(w, http.StatusOK, resp)
}

// clearsOwnSession: the actions that end the caller's own session.
func clearsOwnSession(name string, raw []byte, ownPublicID string) bool {
	switch name {
	case "auth.password_change", "auth.totp_reenroll_confirm":
		return true
	case "auth.sessions_revoke":
		var in struct {
			PublicID string `json:"public_id"`
		}
		_ = json.Unmarshal(raw, &in)
		return in.PublicID != "" && in.PublicID == ownPublicID
	}
	return false
}

// actionError maps an Invoke error to the REST error table. Messages never carry
// a secret: the registry redacts execution errors before they get here, and an
// unexpected failure is answered generically and logged with the request id.
func (s *Server) actionError(w http.ResponseWriter, reqID string, err error) {
	send := func(status int, code, msg string, extra map[string]any) {
		var b errBody
		b.Error.Code, b.Error.Message, b.Error.RequestID = code, msg, reqID
		out := map[string]any{"error": b.Error}
		for k, v := range extra {
			out[k] = v
		}
		WriteJSON(w, status, out)
	}
	var de *actions.DeniedError
	var le *auth.LoginError
	var re *auth.ReenrollError
	switch {
	case errors.Is(err, actions.ErrUnknownAction):
		send(http.StatusNotFound, "unknown_action", "no such action", nil)
	case errors.Is(err, actions.ErrInvalidInput), errors.Is(err, actions.ErrNoDryRun):
		msg := strings.TrimPrefix(err.Error(), "actions: invalid input: ")
		send(http.StatusBadRequest, "invalid_input", msg, nil)
	case errors.As(err, &de):
		code := "denied"
		if de.Code != "" {
			code = de.Code
		}
		send(http.StatusForbidden, code, de.Reason, nil)
	case errors.Is(err, audit.ErrAuditUnavailable):
		w.Header().Set("Retry-After", "2")
		send(http.StatusServiceUnavailable, "audit_unavailable", "the server is busy or the audit log is not writable; nothing was done", nil)
	case errors.Is(err, hub.ErrTooManyStreams):
		w.Header().Set("Retry-After", "2")
		send(http.StatusServiceUnavailable, "busy", "that agent has too many requests open; try again in a moment", nil)
	case errors.Is(err, hub.ErrLocalOnly):
		send(http.StatusForbidden, "denied", "that operation is only available on the local server (remote agents are read-only)", nil)
	case errors.Is(err, auth.ErrBusy):
		w.Header().Set("Retry-After", "2")
		send(http.StatusServiceUnavailable, "busy", "the server is busy checking passwords; try again in a moment", nil)
	case errors.Is(err, auth.ErrRateLimited):
		w.Header().Set("Retry-After", "30")
		send(http.StatusTooManyRequests, "rate_limited", "too many attempts; try again later", nil)
	case errors.Is(err, auth.ErrWrongPassword):
		send(http.StatusForbidden, "wrong_password", "The current password is wrong.", nil)
	case errors.Is(err, auth.ErrWeakPassword), errors.Is(err, auth.ErrBadUsername), errors.Is(err, auth.ErrInvalidToken):
		parts := strings.Split(err.Error(), "\n")
		send(http.StatusBadRequest, "invalid_input", parts[len(parts)-1], nil)
	case errors.Is(err, auth.ErrBadCode):
		extra := map[string]any{}
		if errors.As(err, &re) {
			extra["tries_left"] = re.TriesLeft
		} else if errors.As(err, &le) {
			extra["tries_left"] = le.TriesLeft
		}
		send(http.StatusForbidden, "invalid_code", "That code is wrong.", extra)
	case errors.Is(err, auth.ErrCodeReused):
		send(http.StatusForbidden, "code_reused", "That code was already used. Wait for the next one.", nil)
	case errors.Is(err, auth.ErrEnrollmentReplaced):
		send(http.StatusConflict, "enrollment_replaced", "This enrolment was replaced; scan the new code.", nil)
	case errors.Is(err, auth.ErrPendingInvalid):
		send(http.StatusConflict, "enrollment_expired", "This enrolment expired or ran out of tries; start again.", nil)
	case errors.Is(err, auth.ErrNoSuchToken), errors.Is(err, auth.ErrNoSuchSession):
		send(http.StatusNotFound, "not_found", "no such item", nil)
	case errors.Is(err, auth.ErrKeyMismatch):
		send(http.StatusInternalServerError, "key_mismatch", err.Error(), nil)
	default:
		s.log.Error("action failed", "request_id", reqID, "err", err.Error())
		send(http.StatusInternalServerError, "action_failed", "the action failed; see the server log (request id "+reqID+")", nil)
	}
}
