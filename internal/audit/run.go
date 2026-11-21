package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// Errors of the intent/outcome flow.
var (
	// ErrAuditUnavailable: the intent row could not be written, so nothing was
	// executed or revealed.
	ErrAuditUnavailable = errors.New("audit unavailable; action not run")
	// ErrAuditOutcome: the outcome row could not be written (the action may
	// have run). The returned error wraps it; for a successful action the
	// result is returned alongside with AuditOutcomeRecorded=false.
	ErrAuditOutcome = errors.New("action ran but its audit outcome could not be recorded")
)

// IntentWait is the longest Execute waits for the intent row to be written.
const IntentWait = 2 * time.Second

// Outcome is the result of Execute.
type Outcome[T any] struct {
	Value T
	// AuditOutcomeRecorded is false when the action succeeded but its outcome
	// row could not be written (the error is then ErrAuditOutcome).
	AuditOutcomeRecorded bool
	// IntentID is the intent row's id (0 if none was written).
	IntentID int64
}

// Execute runs fn between an intent row and an outcome row.
//
//   - The intent write fails: fn is not run and the error is ErrAuditUnavailable.
//   - fn fails or panics: an error outcome is written (error text redacted). If
//     that write also fails, the execution error is returned wrapped with
//     ErrAuditOutcome.
//   - fn succeeds but the outcome write fails: the value comes back with
//     AuditOutcomeRecorded=false and ErrAuditOutcome, and an error is logged
//     naming the action, target and request id (never the arguments).
func Execute[T any](ctx context.Context, a Appender, log *slog.Logger, rd Redactor, intent Entry, fn func(context.Context) (T, error)) (Outcome[T], error) {
	intent.Phase, intent.Result, intent.IntentID, intent.Error = PhaseIntent, ResultPending, 0, ""
	// The intent write is bounded: a stuck writer must not hold a request open
	// (it fails as "audit unavailable" and the action is not run).
	ictx, cancel := context.WithTimeout(ctx, IntentWait)
	intentID, err := a.Append(ictx, intent)
	cancel()
	if err != nil {
		log.Error("audit intent write failed; action not run", "action", intent.Action, "target", intent.Target, "request_id", intent.RequestID)
		return Outcome[T]{}, fmt.Errorf("%w: %w", ErrAuditUnavailable, err)
	}
	var out Outcome[T]
	out.IntentID = intentID
	value, execErr := protect(ctx, fn)
	outcome := intent
	outcome.Phase, outcome.IntentID = PhaseOutcome, intentID
	if execErr != nil {
		outcome.Result = ResultError
		outcome.Error = rd.Err(execErr)
	} else {
		outcome.Result = ResultOK
	}
	_, werr := a.Append(context.WithoutCancel(ctx), outcome) // the outcome is written even if the caller went away
	switch {
	case execErr != nil && werr != nil:
		log.Error("audit outcome write failed after a failed action", "action", intent.Action, "target", intent.Target, "request_id", intent.RequestID)
		return out, fmt.Errorf("%w: %w", execErr, ErrAuditOutcome)
	case execErr != nil:
		return out, execErr
	case werr != nil:
		log.Error("audit outcome write failed after a successful action", "action", intent.Action, "target", intent.Target, "request_id", intent.RequestID)
		out.Value = value
		return out, ErrAuditOutcome
	}
	out.Value, out.AuditOutcomeRecorded = value, true
	return out, nil
}

// protect runs fn, turning a panic into an error (the stack goes to the log of
// the caller's choice, not the audit row).
func protect[T any](ctx context.Context, fn func(context.Context) (T, error)) (v T, err error) {
	defer func() {
		if p := recover(); p != nil {
			_ = debug.Stack()
			err = fmt.Errorf("action panicked: %v", p)
		}
	}()
	return fn(ctx)
}
