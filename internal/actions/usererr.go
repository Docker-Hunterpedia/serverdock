package actions

import (
	"errors"
	"fmt"
)

// Errors an action's Execute returns for a refusal the caller should see as
// such (the REST layer maps them to 400, 404, 409 and 503). The text of an
// *UserError is written by the action for the caller: it never carries a secret,
// a Docker message or a path. Anything else that comes out of Execute is an
// internal failure and is answered generically.
var (
	// ErrNotFound: the named item does not exist (404).
	ErrNotFound = errors.New("actions: not found")
	// ErrConflict: the request clashes with the current state, for example a
	// name or port that is taken, or a limit that is reached (409).
	ErrConflict = errors.New("actions: conflict")
	// ErrUnavailable: something the action needs (Docker, the local agent) did not
	// answer; nothing was changed unless the text says so (503).
	ErrUnavailable = errors.New("actions: unavailable")
)

// UserError is a refusal with a message for the caller. Is matches its kind
// (ErrInvalidInput, ErrNotFound, ErrConflict or ErrUnavailable).
type UserError struct {
	Kind error
	Msg  string
}

func (e *UserError) Error() string        { return e.Msg }
func (e *UserError) Is(target error) bool { return target == e.Kind }

// InvalidInput is a 400 raised after the policy check (for example a value that
// is only wrong given the current state of the service).
func InvalidInput(format string, a ...any) error {
	return &UserError{Kind: ErrInvalidInput, Msg: fmt.Sprintf(format, a...)}
}

// NotFound is a 404 with a message for the caller.
func NotFound(format string, a ...any) error {
	return &UserError{Kind: ErrNotFound, Msg: fmt.Sprintf(format, a...)}
}

// Conflict is a 409 with a message for the caller.
func Conflict(format string, a ...any) error {
	return &UserError{Kind: ErrConflict, Msg: fmt.Sprintf(format, a...)}
}

// Unavailable is a 503 with a message for the caller.
func Unavailable(format string, a ...any) error {
	return &UserError{Kind: ErrUnavailable, Msg: fmt.Sprintf(format, a...)}
}
