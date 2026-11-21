// Package jobs is the durable job queue behind deploys, restarts and deletes
// (plan 0004, "Jobs and the scheduler"): rows in the jobs table, one scheduler
// goroutine with one "next due" timer, and at most two workers. A job is
// serialized per lock key ("service:<id>") by a partial unique index, so even
// two racing pickers cannot run two jobs of one service.
//
// The package does no work at load (it is linked into agent mode too) and starts
// nothing by itself: the server constructs a Scheduler and runs it.
package jobs

import (
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"
)

// Job kinds in M3a (M6 adds cron kinds).
const (
	KindDeploy        = "deploy"
	KindRestart       = "restart"
	KindServiceDelete = "service_delete"
)

// Job states. There is deliberately no "unknown": a job interrupted by a restart
// is always resolved by the boot pass (failed or succeeded).
const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

// Deploy row statuses the queue itself sets (the runner owns the rest).
const (
	DeployQueued     = "queued"
	DeploySuperseded = "superseded"
)

// Limits.
const (
	MaxWorkers      = 2
	MaxPayload      = 64 << 10 // payload_json
	MaxActorBlob    = 8 << 10
	MaxErrorText    = 512
	DeployAttempts  = 3 // a deploy's pull is tried at most three times
	PruneBatch      = 100
	FinishedJobKeep = 30 * 24 * time.Hour
)

// ErrInvalid is returned for a request that fails validation at the boundary.
var ErrInvalid = errors.New("jobs: invalid request")

// ErrNotCancellable is returned by Cancel for a job that already finished.
var ErrNotCancellable = errors.New("jobs: the job has already finished")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// timeLayout is fixed width and UTC, so stored times sort as strings.
const timeLayout = "2006-01-02T15:04:05.000Z"

// FormatTime renders t the way the jobs and deploys tables store times.
func FormatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// ParseTime is the inverse of FormatTime.
func ParseTime(s string) (time.Time, error) { return time.Parse(timeLayout, s) }

var lockKeyRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^service:[0-9]{1,18}$`) })

// LockKey is the lock key of a service's jobs.
func LockKey(serviceID int64) string { return fmt.Sprintf("service:%d", serviceID) }

// Actor is who queued a job. Blob is the opaque encoding of actions.Actor that
// step 4b adds; this package stores it and never interprets it.
type Actor struct {
	Kind    string
	ID      string
	Session string // the queuing session's public id; empty unless a user session queued it
	Blob    string
}

func (a Actor) validate() error {
	switch {
	case a.Kind == "" || len(a.Kind) > 32:
		return invalid("actor kind")
	case a.ID == "" || len(a.ID) > 128:
		return invalid("actor id")
	case len(a.Session) > 64:
		return invalid("actor session")
	case a.Blob == "" || len(a.Blob) > MaxActorBlob:
		return invalid("actor blob")
	}
	return nil
}

// Job is one jobs row as the scheduler hands it to the runner.
type Job struct {
	ID              int64
	Kind            string
	LockKey         string
	State           string
	Actor           Actor
	Confirm         string
	EnqueuedAt      time.Time
	Payload         []byte
	Attempt         int
	MaxAttempts     int
	RunAfter        time.Time
	TimeoutS        int
	CancelRequested bool
	CancelReason    string
	BootID          string
	RequestID       string
}

// RequestID is the audit request id of one run of a job: "job-<id>-a<attempt>".
func RequestID(id int64, attempt int) string { return fmt.Sprintf("job-%d-a%d", id, attempt) }

// retryable marks an error as worth another attempt (a transient pull failure).
type retryable struct{ err error }

func (r retryable) Error() string { return r.err.Error() }
func (r retryable) Unwrap() error { return r.err }

// Retryable wraps err so the scheduler retries the job (attempt + 1 after
// RetryDelay) while attempts remain. Only the runner knows which failures are
// transient (registry 429 or 5xx, timeout, connection refused, daemon
// unavailable). Anything else, disk_full above all, fails the job at once.
func Retryable(err error) error {
	if err == nil {
		return nil
	}
	return retryable{err}
}

// IsRetryable reports whether err was marked with Retryable.
func IsRetryable(err error) bool {
	var r retryable
	return errors.As(err, &r)
}

// RetryDelay is the wait before attempt n+1 after attempt n failed: 15 s, then 60 s.
func RetryDelay(failedAttempt int) time.Duration {
	if failedAttempt <= 1 {
		return 15 * time.Second
	}
	return 60 * time.Second
}
