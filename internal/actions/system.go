package actions

import (
	"context"
	"encoding/json"

	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
)

// SystemInvoker runs work nobody triggered (scheduled backups, cleanup, crash
// reconciliation) and runs queued jobs on behalf of the actor that queued them.
// It exists exactly once per Registry: cmd/serverdock hands it to `admin reset`
// and the M3a scheduler and reconciler. internal/api and internal/mcp never
// receive it (an import test enforces that).
type SystemInvoker struct{ r *Registry }

// SystemInvoker returns the one-time handle; a second call panics.
func (r *Registry) SystemInvoker() *SystemInvoker {
	r.sysMu.Lock()
	defer r.sysMu.Unlock()
	if r.sysTaken {
		panic("actions: SystemInvoker was already taken")
	}
	r.sysTaken = true
	return &SystemInvoker{r: r}
}

// Invoke runs name as the system actor: reads and sensitive reads are allowed
// (and audited), mutate and destructive actions only when they declare a System
// reason.
func (s *SystemInvoker) Invoke(ctx context.Context, name string, raw json.RawMessage, opts Options) (Result, error) {
	return s.r.invoke(ctx, Actor{kind: KindSystem, id: "system"}, "", name, raw, opts)
}

// InvokeAs runs name under actor's policy (it is checked again, now), with
// executor = 'system' on the audit row. This is the only way to set the
// executor marker; it is not an Options field. A job runner calls it for every
// action a queued job runs, so a revoked scope or an expired 2FA window takes
// effect at run time, not just at enqueue.
func (s *SystemInvoker) InvokeAs(ctx context.Context, actor Actor, name string, raw json.RawMessage, opts Options) (Result, error) {
	return s.r.invoke(ctx, actor, "system", name, raw, opts)
}

// RecordDenied writes a "denied" audit row for action name as actor, with
// executor = 'system', without running or authorising anything. The job runner
// uses it when a job's actor is refused before the action is even invoked (the
// token was revoked, the credentials changed: auth.Store.RefreshJobActor), so the
// refusal leaves the same trace an InvokeAs denial would. The action must exist and
// raw must be valid input for it; reason is what the audit row says and requestID
// is the run's ("job-<id>-a<attempt>").
func (s *SystemInvoker) RecordDenied(ctx context.Context, actor Actor, name string, raw json.RawMessage, reason, requestID string) error {
	act, p, err := s.r.prep(name, raw)
	if err != nil {
		return err
	}
	e := audit.Entry{ActorKind: string(actor.kind), ActorID: actor.id, Executor: "system", Action: act.name, Target: p.target,
		Risk: act.risk.String(), ArgsJSON: p.args, RequestID: requestID, Error: reason}
	if e.ActorKind == "" {
		e.ActorKind, e.ActorID = "invalid", "invalid"
	}
	if e.ActorID == "" {
		e.ActorID = "unknown"
	}
	_, err = s.r.sink.Denied(ctx, e)
	return err
}
