package api

import (
	"sort"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
)

// BuildOpenAPI describes the REST API as an OpenAPI 3.1 document: one path per
// action that is reachable over HTTP (console-only actions are omitted), generated
// from the registry's types, plus a hand-written fragment for the setup and
// sign-in endpoints, which are not registry actions. The result is deterministic
// (the golden file docs/api/openapi.json is compared against it).
func BuildOpenAPI(list []actions.Info) (map[string]any, error) {
	paths := map[string]any{}
	schemas := map[string]any{}
	sorted := append([]actions.Info(nil), list...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, a := range sorted {
		if a.ConsoleOnly || a.JobOnly {
			continue
		}
		in, err := actions.SchemaOf(a.InputType)
		if err != nil {
			return nil, err
		}
		out, err := actions.SchemaOf(a.OutputType)
		if err != nil {
			return nil, err
		}
		security := []any{map[string]any{"cookieAuth": []any{}}, map[string]any{"bearerAuth": []any{}}}
		if a.SessionOnly || a.Sensitive || a.Risk == actions.RiskDestructive {
			security = []any{map[string]any{"cookieAuth": []any{}}} // tokens are refused for these
		}
		params := []any{
			map[string]any{"name": "SD-Dry-Run", "in": "header", "required": false, "description": "1: report what the action would do, run nothing (only for actions with a dry run)", "schema": map[string]any{"type": "string", "enum": []any{"1"}}},
		}
		if a.Risk == actions.RiskDestructive {
			params = append(params, map[string]any{"name": "SD-Confirm", "in": "header", "required": true, "description": "the exact target of the action", "schema": map[string]any{"type": "string"}})
		}
		schemas[schemaName(a.Name, "Input")] = in
		schemas[schemaName(a.Name, "Output")] = out
		paths["/api/v1/actions/"+a.Name] = map[string]any{"post": map[string]any{
			"operationId": a.Name, "summary": a.Summary, "tags": []any{tagOf(a.Name)},
			"x-sd-risk": a.Risk.String(), "x-sd-scope": scopeName(a.Scope), "x-sd-sensitive": a.Sensitive, "x-sd-session-only": a.SessionOnly,
			"x-sd-removes-access": a.RemovesAccess,
			"security":            security, "parameters": params,
			"requestBody": map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{
				"schema": map[string]any{"$ref": "#/components/schemas/" + schemaName(a.Name, "Input")}}}},
			"responses": map[string]any{
				"200": map[string]any{"description": "The action ran (or, with SD-Dry-Run, its plan).", "content": map[string]any{"application/json": map[string]any{
					"schema": map[string]any{"type": "object", "required": []any{"request_id", "audit"}, "properties": map[string]any{
						"data":       map[string]any{"$ref": "#/components/schemas/" + schemaName(a.Name, "Output")},
						"plan":       map[string]any{"type": "object", "properties": map[string]any{"summary": map[string]any{"type": "string"}, "steps": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}},
						"audit":      map[string]any{"type": "string", "enum": []any{"recorded", "unrecorded", "not_audited"}},
						"request_id": map[string]any{"type": "string"},
					}}}}},
				"400": errRef("invalid_input"), "401": errRef("unauthenticated, token_expired, idle_expired, absolute_expired or revoked"),
				"403": errRef("denied, mfa_required, confirm, csrf or origin"), "404": errRef("unknown_action"), "421": errRef("misdirected"),
				"429": errRef("rate_limited"), "500": errRef("action_failed"), "503": errRef("audit_unavailable or busy"),
			},
		}}
	}
	for k, v := range preSessionPaths() {
		paths[k] = v
	}
	paths["/api/v1/openapi.json"] = map[string]any{"get": map[string]any{
		"operationId": "openapi", "summary": "This document.", "tags": []any{"api"},
		"security":  []any{map[string]any{"cookieAuth": []any{}}, map[string]any{"bearerAuth": []any{}}},
		"responses": map[string]any{"200": map[string]any{"description": "The OpenAPI 3.1 document."}},
	}}
	schemas["Error"] = map[string]any{"type": "object", "required": []any{"error"}, "properties": map[string]any{
		"error": map[string]any{"type": "object", "required": []any{"code", "message", "request_id"}, "properties": map[string]any{
			"code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}, "request_id": map[string]any{"type": "string"}}}}}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{"title": "ServerDock API", "version": "1",
			"description": "Generated from the action registry. Unsafe session requests need the X-SD-CSRF header and a matching Origin; API tokens are sent as Authorization: Bearer and can only run non-sensitive, non-destructive, non-session-only actions."},
		"servers": []any{map[string]any{"url": "/"}},
		"paths":   paths,
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"cookieAuth":  map[string]any{"type": "apiKey", "in": "cookie", "name": SessionCookie, "description": "Session cookie. Unsafe methods also need the " + CSRFHeader + " header (from the sign-in response or GET /api/v1/auth/session) and an Origin equal to https://<Host>."},
				"pendingAuth": map[string]any{"type": "apiKey", "in": "cookie", "name": PendingCookie, "description": "The pending-2FA cookie set by auth.login (valid 5 minutes, 5 tries). It is not a session."},
				"bearerAuth":  map[string]any{"type": "http", "scheme": "bearer", "description": "API token (sd_...). Never accepted in a cookie or query string."},
			},
			"schemas": schemas,
		},
	}, nil
}

func errRef(codes string) map[string]any {
	return map[string]any{"description": "Error (" + codes + ").", "content": map[string]any{"application/json": map[string]any{
		"schema": map[string]any{"$ref": "#/components/schemas/Error"}}}}
}

func schemaName(action, kind string) string {
	b := []byte(action)
	up := true
	out := make([]byte, 0, len(b)+len(kind))
	for _, c := range b {
		if c == '.' || c == '_' {
			up = true
			continue
		}
		if up && c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		up = false
		out = append(out, c)
	}
	return string(out) + kind
}

func tagOf(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] == '.' {
			return name[:i]
		}
	}
	return name
}

func scopeName(s actions.Scope) string {
	switch s {
	case actions.ScopeRead:
		return "read"
	case actions.ScopeWrite:
		return "write"
	}
	return "admin"
}

func obj(props map[string]any, required ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		r := make([]any, len(required))
		for i, v := range required {
			r[i] = v
		}
		o["required"] = r
	}
	return o
}

func body(schema map[string]any) map[string]any {
	return map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": schema}}}
}

func okJSON(desc string, schema map[string]any) map[string]any {
	return map[string]any{"description": desc, "content": map[string]any{"application/json": map[string]any{"schema": schema}}}
}

func str() map[string]any { return map[string]any{"type": "string"} }

// preSessionPaths is the hand-written fragment: the setup and sign-in endpoints
// are not registry actions (they run before any session exists).
func preSessionPaths() map[string]any {
	none := []any{}
	pw := map[string]any{"type": "string", "writeOnly": true, "format": "password"}
	return map[string]any{
		"/api/v1/setup/status": map[string]any{"get": map[string]any{
			"operationId": "setup.status", "summary": "Whether first-run setup is open, and until when.", "tags": []any{"setup"}, "security": none,
			"responses": map[string]any{"200": okJSON("Setup state. Once an admin exists only setup_required (false) and server_time are returned.", obj(map[string]any{
				"setup_required": map[string]any{"type": "boolean"}, "token_expires_at": map[string]any{"type": "string", "format": "date-time"},
				"token_file": str(), "server_time": map[string]any{"type": "string", "format": "date-time"}}, "setup_required", "server_time"))}}},
		"/api/v1/setup/enroll": map[string]any{"post": map[string]any{
			"operationId": "setup.enroll", "summary": "Check the setup token and start the authenticator enrolment (404 once an admin exists).", "tags": []any{"setup"}, "security": none,
			"requestBody": body(obj(map[string]any{"token": map[string]any{"type": "string", "writeOnly": true, "format": "password"}, "username": str()}, "token", "username")),
			"responses": map[string]any{"200": okJSON("The key and the otpauth URI, shown once.", obj(map[string]any{"enrollment_id": str(), "secret": str(), "otpauth_uri": str()}, "enrollment_id", "secret", "otpauth_uri")),
				"401": errRef("invalid_token"), "403": errRef("setup_token_expired"), "404": errRef("not_found"), "409": errRef("token_consumed"), "429": errRef("rate_limited")}}},
		"/api/v1/setup/complete": map[string]any{"post": map[string]any{
			"operationId": "setup.complete", "summary": "Create the admin with a password and a confirmed authenticator; signs in.", "tags": []any{"setup"}, "security": none,
			"requestBody": body(obj(map[string]any{"token": pw, "enrollment_id": pw, "username": str(), "password": pw, "code": str()}, "token", "enrollment_id", "username", "password", "code")),
			"responses": map[string]any{"200": okJSON("Signed in (session cookie set). The recovery codes are shown once.", obj(map[string]any{
				"username": str(), "recovery_codes": map[string]any{"type": "array", "items": str()}, "csrf": str(), "audit": str()}, "username", "recovery_codes", "csrf")),
				"400": errRef("invalid_input"), "401": errRef("invalid_token or invalid_code"), "403": errRef("setup_token_expired"), "404": errRef("not_found"),
				"409": errRef("token_consumed or enrollment_replaced"), "429": errRef("rate_limited"), "503": errRef("audit_unavailable or busy")}}},
		"/api/v1/auth/login": map[string]any{"post": map[string]any{
			"operationId": "auth.login", "summary": "Password step: sets the pending-2FA cookie (never a session).", "tags": []any{"auth"}, "security": none,
			"requestBody": body(obj(map[string]any{"username": str(), "password": pw}, "username", "password")),
			"responses": map[string]any{"200": okJSON("Second factor required.", obj(map[string]any{"two_factor_required": map[string]any{"type": "boolean"}, "expires_in": map[string]any{"type": "integer"}}, "two_factor_required", "expires_in")),
				"401": errRef("invalid_credentials"), "429": errRef("rate_limited"), "503": errRef("audit_unavailable or busy")}}},
		"/api/v1/auth/2fa": map[string]any{"post": map[string]any{
			"operationId": "auth.2fa", "summary": "Second factor: a TOTP code or a recovery code; sets the session cookie.", "tags": []any{"auth"}, "security": []any{map[string]any{"pendingAuth": []any{}}},
			"requestBody": body(obj(map[string]any{"code": str(), "recovery_code": str()})),
			"responses": map[string]any{"200": okJSON("Signed in.", obj(map[string]any{"username": str(), "csrf": str(), "notice": map[string]any{"type": "object"}, "mfa_fresh_until": map[string]any{"type": "string", "format": "date-time"}}, "username", "csrf")),
				"400": errRef("invalid_input"), "401": errRef("invalid_code, code_reused or unauthenticated"), "429": errRef("rate_limited"), "503": errRef("audit_unavailable")}}},
		"/api/v1/auth/session": map[string]any{"get": map[string]any{
			"operationId": "auth.session", "summary": "The signed-in user, the CSRF token and the sign-in notice.", "tags": []any{"auth"}, "security": []any{map[string]any{"cookieAuth": []any{}}},
			"responses": map[string]any{"200": okJSON("The session.", map[string]any{"type": "object"}), "401": errRef("unauthenticated, idle_expired, absolute_expired or revoked")}}},
		"/api/v1/auth/reauth": map[string]any{"post": map[string]any{
			"operationId": "auth.reauth", "summary": "Step-up: a TOTP code extends the fresh-2FA window by 5 minutes.", "tags": []any{"auth"}, "security": []any{map[string]any{"cookieAuth": []any{}}},
			"requestBody": body(obj(map[string]any{"code": str()}, "code")),
			"responses": map[string]any{"200": okJSON("The new window.", obj(map[string]any{"mfa_fresh_until": map[string]any{"type": "string", "format": "date-time"}}, "mfa_fresh_until")),
				"403": errRef("invalid_code or code_reused"), "401": errRef("revoked"), "429": errRef("rate_limited")}}},
		"/api/v1/auth/logout": map[string]any{"post": map[string]any{
			"operationId": "auth.logout", "summary": "Sign out (always runs, even if the audit log is down).", "tags": []any{"auth"}, "security": []any{map[string]any{"cookieAuth": []any{}}},
			"requestBody": body(obj(map[string]any{})),
			"responses":   map[string]any{"200": okJSON("Signed out.", obj(map[string]any{"ok": map[string]any{"type": "boolean"}, "audit": str()}, "ok"))}}},
	}
}
