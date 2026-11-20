package actions

import "time"

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
}

// UserActor is a signed-in user session; mfaFreshUntil is when its fresh-2FA
// window ends.
func UserActor(id string, scope Scope, mfaFreshUntil time.Time) Actor {
	return Actor{kind: KindUser, id: id, scope: scope, mfaFreshUntil: mfaFreshUntil}
}

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

// authorize applies the risk policy:
//
//	actor    | read     | sensitive          | mutate                            | destructive
//	user     | scope    | scope + fresh MFA  | scope                             | scope + fresh MFA + Confirm == Target
//	token    | scope    | denied             | scope (not SessionOnly)           | denied
//	mcp      | read only| denied             | denied                            | denied
//	webhook  | denied   | denied             | WebhookAllowed and Target == bound| denied
//	system   | allowed  | allowed (audited)  | only when System != ""            | only when System != ""
//
// SessionOnly actions refuse every actor except a user.
func (r *Registry) authorize(a Actor, act *action, target string, opts Options) error {
	if a.kind == "" {
		return denied("no actor")
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
