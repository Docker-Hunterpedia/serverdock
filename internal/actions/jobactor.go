package actions

import (
	"encoding/json"
	"errors"
	"time"
)

// Errors of the job actor encoding.
var (
	// ErrSystemJobActor: a job blob must never name the system actor; user-triggered
	// work keeps the user's (or token's) identity, and only the executor column says
	// "system".
	ErrSystemJobActor = errors.New("actions: a job cannot run as the system actor")
	ErrBadJobActor    = errors.New("actions: invalid job actor")
)

const maxJobBlob = 8 << 10

// jobBlobV1 is the stored form of an Actor. Field names are short because it sits
// in every job row. It carries no secret: a session's public id is the id shown in
// the sessions list, never the cookie value.
type jobBlobV1 struct {
	V      int    `json:"v"`
	Kind   string `json:"k"`
	ID     string `json:"id"`
	Scope  int    `json:"s,omitempty"`
	MFA    int64  `json:"m,omitempty"` // fresh-2FA window end, unix seconds
	Sess   string `json:"sess,omitempty"`
	Host   string `json:"h,omitempty"`
	Target string `json:"b,omitempty"` // a webhook's bound target
}

// JobBlob encodes the actor for a job row: kind, id, scope, the end of the
// fresh-2FA window, the session public id and the host kind. It returns "" for the
// system actor and for an empty one, so an enqueue that stores the result is
// refused (jobs.Actor needs a non-empty blob).
func (a Actor) JobBlob() string {
	if a.kind == "" || a.kind == KindSystem {
		return ""
	}
	b := jobBlobV1{V: 1, Kind: string(a.kind), ID: a.id, Scope: int(a.scope), Sess: a.session, Host: a.hostKind, Target: a.boundTarget}
	if !a.mfaFreshUntil.IsZero() {
		b.MFA = a.mfaFreshUntil.Unix()
	}
	out, err := json.Marshal(b)
	if err != nil || len(out) > maxJobBlob {
		return ""
	}
	return string(out)
}

// ActorFromJob rebuilds the actor a job was queued by. Only the holder of the
// one-time SystemInvoker can do it, so HTTP and MCP code cannot mint an actor from
// a string. It refuses the system kind (ErrSystemJobActor) and every unknown kind,
// version or malformed field (ErrBadJobActor). The caller refreshes the result
// against the database (auth.Store.RefreshJobActor) before running anything.
func (s *SystemInvoker) ActorFromJob(blob string) (Actor, error) {
	if len(blob) == 0 || len(blob) > maxJobBlob {
		return Actor{}, ErrBadJobActor
	}
	var b jobBlobV1
	if err := decodeStrict([]byte(blob), &b); err != nil {
		// The decoder's text stays in the log: the caller's error ends up in a job row.
		s.r.log.Warn("job actor blob refused", "err", err.Error())
		return Actor{}, ErrBadJobActor
	}
	if b.V != 1 {
		return Actor{}, ErrBadJobActor
	}
	kind := ActorKind(b.Kind)
	if kind == KindSystem {
		return Actor{}, ErrSystemJobActor
	}
	if b.ID == "" || len(b.ID) > 128 || len(b.Sess) > 64 || len(b.Host) > 16 || len(b.Target) > 256 {
		return Actor{}, ErrBadJobActor
	}
	scope := Scope(b.Scope)
	var a Actor
	switch kind {
	case KindUser, KindToken:
		if scope < ScopeRead || scope > ScopeAdmin {
			return Actor{}, ErrBadJobActor
		}
	case KindMCP:
		scope = ScopeRead
	case KindWebhook:
		scope = 0
	default:
		return Actor{}, ErrBadJobActor
	}
	a = Actor{kind: kind, id: b.ID, scope: scope, session: b.Sess, hostKind: b.Host, boundTarget: b.Target}
	if kind == KindUser && b.MFA > 0 {
		a.mfaFreshUntil = time.Unix(b.MFA, 0).UTC()
	}
	return a, nil
}
