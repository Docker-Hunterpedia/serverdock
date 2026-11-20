package actions

import (
	"context"
	"encoding/json"
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
