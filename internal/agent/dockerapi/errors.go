package dockerapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
)

// Typed errors for the statuses callers branch on. Match with errors.Is.
var (
	ErrNotFound    = errors.New("dockerapi: not found")
	ErrConflict    = errors.New("dockerapi: conflict")
	ErrNotModified = errors.New("dockerapi: not modified")
	// ErrDiskFull is a pull that failed because the daemon ran out of space.
	ErrDiskFull = errors.New("dockerapi: no space left on device")
	// ErrLineTooLong is a Docker JSON line over MaxJSONLine.
	ErrLineTooLong = errors.New("dockerapi: JSON line from Docker too long")
)

// maxErrMsg bounds the Docker message kept in an error.
const maxErrMsg = 256

// APIError is a non-success Engine API response. Msg is Docker's message,
// cleaned (it can contain text from a registry or an image) and capped.
type APIError struct {
	Op     string
	Status int
	Msg    string
}

func (e *APIError) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("dockerapi: %s: status %d", e.Op, e.Status)
	}
	return fmt.Sprintf("dockerapi: %s: status %d: %s", e.Op, e.Status, e.Msg)
}

// Is maps 404, 409 and 304 to the sentinels, and a "no space left" message to
// ErrDiskFull.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrConflict:
		return e.Status == http.StatusConflict
	case ErrNotModified:
		return e.Status == http.StatusNotModified
	case ErrDiskFull:
		return isNoSpace(e.Msg)
	}
	return false
}

func isNoSpace(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "no space left on device")
}

// newAPIError builds an APIError from a status and the (small) response body;
// op is the request path, cut at the query.
func newAPIError(op string, status int, body []byte) *APIError {
	op, _, _ = strings.Cut(op, "?")
	var m struct {
		Message string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(body, &m) == nil {
		msg = m.Message
	} else {
		msg = string(body)
	}
	return &APIError{Op: op, Status: status, Msg: agent.CleanText(msg, maxErrMsg)}
}

// statusErr returns nil when st is one of ok, otherwise an *APIError.
func statusErr(op string, st int, body []byte, ok ...int) error {
	for _, o := range ok {
		if st == o {
			return nil
		}
	}
	return newAPIError(op, st, body)
}

// PullError is a pull that Docker reported as failed in its progress stream
// (an `error` or `errorDetail` line, for example "manifest unknown" or "pull
// access denied"). Msg is the registry's text, cleaned and capped: it is what
// the user needs to see.
type PullError struct{ Msg string }

func (e *PullError) Error() string { return "dockerapi: pull: " + e.Msg }
