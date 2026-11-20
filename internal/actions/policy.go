package actions

import (
	"strings"
	"time"
)

// ActorKind says who is acting.
type ActorKind string

// Actor kinds.
const (
	KindUser    ActorKind = "user"
	KindToken   ActorKind = "token"
	KindMCP     ActorKind = "mcp"
	KindWebhook ActorKind = "webhook"
	KindSystem  ActorKind = "system"
)

// Actor is who an action runs for. Its fields are unexported and there is no
// exported constructor for KindSystem: unattended work goes through the one-time
// SystemInvoker, so API and MCP code cannot mint a system actor.
type Actor struct {
	kind          ActorKind
	id            string
	scope         Scope
	mfaFreshUntil time.Time
	boundTarget   string
	session       string // the signed-in session's public id (user actors from REST)
	hostKind      string // how the panel was reached: ip, localhost or name ("" when unknown)
}

// UserActor is a signed-in user session; mfaFreshUntil is when its fresh-2FA
// window ends.
func UserActor(id string, scope Scope, mfaFreshUntil time.Time) Actor {
	return Actor{kind: KindUser, id: id, scope: scope, mfaFreshUntil: mfaFreshUntil}
}

// UserSessionActor is UserActor that also knows which session it is (its public
// id), so an action can tell "this session" from "the other sessions".
func UserSessionActor(id, sessionPublicID string, scope Scope, mfaFreshUntil time.Time) Actor {
	a := UserActor(id, scope, mfaFreshUntil)
	a.session = sessionPublicID
	return a
}

// WithHostKind returns the actor with how the panel was reached ("ip", "localhost"
// or "name"), as the REST layer derives it from the request's Host. It is server
// state, never client input (no action input field carries it).
func (a Actor) WithHostKind(kind string) Actor { a.hostKind = kind; return a }

// HostKind is how the panel was reached ("" when unknown).
func (a Actor) HostKind() string { return a.hostKind }

// TokenActor is an API token with a scope.
func TokenActor(id string, scope Scope) Actor { return Actor{kind: KindToken, id: id, scope: scope} }

// MCPActor is an MCP client; it only ever has read scope.
func MCPActor(tokenID string) Actor { return Actor{kind: KindMCP, id: tokenID, scope: ScopeRead} }

// WebhookActor is a webhook bound to one target.
func WebhookActor(hookID, boundTarget string) Actor {
	return Actor{kind: KindWebhook, id: hookID, boundTarget: boundTarget}
}

// Kind and ID identify the actor.
func (a Actor) Kind() ActorKind { return a.kind }
func (a Actor) ID() string      { return a.id }

// SessionID is the public id of the session a user actor runs in ("" otherwise).
func (a Actor) SessionID() string { return a.session }

// authorize applies the risk policy:
//
//	actor    | read     | sensitive          | mutate                            | destructive
//	user     | scope    | scope + fresh MFA  | scope                             | scope + fresh MFA + Confirm == Target
//	token    | scope    | denied             | scope (not SessionOnly)           | denied
//	mcp      | read only| denied             | denied                            | denied
//	webhook  | denied   | denied             | WebhookAllowed and Target == bound| denied
//	system   | allowed  | allowed (audited)  | only when System != ""            | only when System != ""
//
// SessionOnly actions refuse every actor except a user. ConsoleOnly actions
// refuse every actor except the system (the console recovery command).
func (r *Registry) authorize(a Actor, act *action, target string, opts Options) error {
	if a.kind == "" {
		return denied("no actor")
	}
	if act.consoleOnly && a.kind != KindSystem {
		return denied("%s can only be run from the console", act.name)
	}
	if act.sessionOnly && a.kind != KindUser {
		return denied("%s is only available to a signed-in user session", act.name)
	}
	switch a.kind {
	case KindUser:
		if a.scope < act.scope {
			return denied("needs a higher scope")
		}
		if act.sensitive || act.risk == RiskDestructive {
			if !a.mfaFreshUntil.After(r.now()) {
				return denied("fresh two-factor confirmation required")
			}
		}
		if act.risk == RiskDestructive && (target == "" || opts.Confirm != target) {
			return denied("type the exact target to confirm")
		}
	case KindToken:
		if act.sensitive {
			return denied("API tokens cannot run sensitive actions")
		}
		if act.risk == RiskDestructive {
			return denied("API tokens cannot run destructive actions")
		}
		if a.scope < act.scope {
			return denied("the token's scope is too low")
		}
	case KindMCP:
		if act.sensitive || act.risk != RiskRead || act.scope > ScopeRead {
			return denied("MCP clients are read-only")
		}
	case KindWebhook:
		if act.sensitive || act.risk != RiskMutate || !act.webhookOK {
			return denied("webhooks cannot run this action")
		}
		if target == "" || target != a.boundTarget {
			return denied("the webhook is not bound to this target")
		}
	case KindSystem:
		if act.risk != RiskRead && act.system == "" {
			return denied("not allowed to run unattended")
		}
	default:
		return denied("unknown actor")
	}
	return nil
}

// UnlistedUnattendedDestructive returns the destructive actions with a System
// reason whose name does not appear (in backticks) in the "Unattended destructive
// actions" section of docs/security.md (doc is the file's text). Every registry
// that can hold such an action is checked with it: the server's, and the one
// `admin reset` builds.
func UnlistedUnattendedDestructive(list []Info, doc string) []string {
	const heading = "## Unattended destructive actions"
	_, rest, ok := strings.Cut(doc, heading)
	section := ""
	if ok {
		section, _, _ = strings.Cut(rest, "\n## ")
	}
	var missing []string
	for _, a := range list {
		if a.Risk == RiskDestructive && a.System != "" && !strings.Contains(section, "`"+a.Name+"`") {
			missing = append(missing, a.Name)
		}
	}
	return missing
}
