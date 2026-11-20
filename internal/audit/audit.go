// Package audit is the append-only, hash-chained audit log (plan 0002 M1 step 8).
//
// Every row's hash is SHA-256(prev_hash || enc(fields)) over a length-prefixed,
// fixed-order encoding that includes the row id and the boot id, so editing a
// field, deleting a row, or swapping rows breaks the chain at a known id. The
// genesis prev_hash is 32 zero bytes. Appends run inside the single writer's
// BEGIN IMMEDIATE transaction, so the chain is linear.
//
// What the chain cannot detect, by design: truncating the tail, or someone who
// can write the file rebuilding the whole chain. The head hash and row count
// are logged at startup and returned by system.info so they can be compared
// with an older copy (M7b also stores them in state backups).
package audit

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/redact"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// Phases and results of a row.
const (
	PhaseIntent  = "intent"
	PhaseOutcome = "outcome"
	PhaseDenied  = "denied"

	ResultPending = "pending"
	ResultOK      = "ok"
	ResultError   = "error"
	ResultDenied  = "denied"
)

// Genesis is the prev_hash of the first row.
var Genesis = make([]byte, sha256.Size)

// Entry is one row to append. ID, TS, BootID and the hashes are filled in by Append.
type Entry struct {
	ActorKind, ActorID string
	Executor           string // "" = NULL; "system" when a job runs it on the actor's behalf
	Action, Target     string
	Risk               string // read | mutate | destructive
	DryRun             bool
	Phase              string
	IntentID           int64 // 0 = NULL; on an outcome, the intent it resolves
	ArgsJSON           string
	Result             string
	Error              string
	Approver           string // "" = NULL
	RequestID          string
}

// Appender is what actions need from the log (and what tests replace to force
// write failures).
type Appender interface {
	Append(ctx context.Context, e Entry) (id int64, err error)
}

// Logger appends to the audit_log table of a store.
type Logger struct {
	st     *store.Store
	log    *slog.Logger
	bootID string
	now    func() time.Time

	denied deniedLimiter
}

// Option configures New.
type Option func(*Logger)

// WithClock injects the clock (tests).
func WithClock(now func() time.Time) Option { return func(l *Logger) { l.now = now } }

// WithBootID fixes the boot id (tests simulating another boot).
func WithBootID(id string) Option { return func(l *Logger) { l.bootID = id } }

// New returns a Logger for st. A new boot id is generated per Logger, so rows
// written by an earlier process are told apart from this one's.
func New(st *store.Store, log *slog.Logger, opts ...Option) (*Logger, error) {
	l := &Logger{st: st, log: log, now: time.Now}
	for _, o := range opts {
		o(l)
	}
	if l.bootID == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		l.bootID = hex.EncodeToString(b[:])
	}
	l.denied.init(l)
	return l, nil
}

// BootID identifies this process's rows.
func (l *Logger) BootID() string { return l.bootID }

// Append writes one row, chaining it to the previous one.
func (l *Logger) Append(ctx context.Context, e Entry) (int64, error) {
	if err := e.validate(); err != nil {
		return 0, err
	}
	var id int64
	err := l.st.WriteTx(ctx, func(tx *sql.Tx) error {
		prev, last := Genesis, int64(0)
		var h []byte
		switch err := tx.QueryRowContext(ctx, `SELECT id, hash FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&last, &h); {
		case err == nil:
			prev = h
		case errors.Is(err, sql.ErrNoRows):
		default:
			return err
		}
		r := row{Entry: e, ID: last + 1, TS: l.now().UTC().Format(time.RFC3339Nano), BootID: l.bootID, Prev: prev}
		r.Hash = r.compute()
		var executor, approver sql.NullString
		var intent sql.NullInt64
		if e.Executor != "" {
			executor = sql.NullString{String: e.Executor, Valid: true}
		}
		if e.Approver != "" {
			approver = sql.NullString{String: e.Approver, Valid: true}
		}
		if e.IntentID != 0 {
			intent = sql.NullInt64{Int64: e.IntentID, Valid: true}
		}
		dry := 0
		if e.DryRun {
			dry = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log
			(id, ts, boot_id, actor_kind, actor_id, executor, action, target, risk, dry_run, phase, intent_id,
			 args_json, result, error, approver, request_id, prev_hash, hash)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.ID, r.TS, r.BootID, e.ActorKind, e.ActorID, executor, e.Action, e.Target, e.Risk, dry, e.Phase, intent,
			e.ArgsJSON, e.Result, e.Error, approver, e.RequestID, r.Prev, r.Hash); err != nil {
			return err
		}
		id = r.ID
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("audit: append: %w", err)
	}
	return id, nil
}

func (e Entry) validate() error {
	switch e.Phase {
	case PhaseIntent, PhaseOutcome, PhaseDenied:
	default:
		return fmt.Errorf("audit: bad phase %q", e.Phase)
	}
	switch e.Result {
	case ResultPending, ResultOK, ResultError, ResultDenied:
	default:
		return fmt.Errorf("audit: bad result %q", e.Result)
	}
	switch e.Risk {
	case "read", "mutate", "destructive":
	default:
		return fmt.Errorf("audit: bad risk %q", e.Risk)
	}
	if e.ActorKind == "" || e.ActorID == "" || e.Action == "" {
		return errors.New("audit: actor and action are required")
	}
	if (e.Phase == PhaseOutcome) != (e.IntentID != 0) {
		return errors.New("audit: an outcome needs an intent_id, and only an outcome has one")
	}
	return nil
}

// row is a stored row, for hashing and verification.
type row struct {
	Entry
	ID     int64
	TS     string
	BootID string
	Prev   []byte
	Hash   []byte
}

// compute returns SHA-256(prev || enc(fields)).
func (r row) compute() []byte {
	var b bytes.Buffer
	b.Write(r.Prev)
	putInt(&b, r.ID)
	putStr(&b, r.TS)
	putStr(&b, r.BootID)
	putStr(&b, r.ActorKind)
	putStr(&b, r.ActorID)
	putOpt(&b, r.Executor)
	putStr(&b, r.Action)
	putStr(&b, r.Target)
	putStr(&b, r.Risk)
	if r.DryRun {
		putInt(&b, 1)
	} else {
		putInt(&b, 0)
	}
	putStr(&b, r.Phase)
	if r.IntentID == 0 {
		b.WriteByte(0)
	} else {
		b.WriteByte(1)
		putInt(&b, r.IntentID)
	}
	putStr(&b, r.ArgsJSON)
	putStr(&b, r.Result)
	putStr(&b, r.Error)
	putOpt(&b, r.Approver)
	putStr(&b, r.RequestID)
	sum := sha256.Sum256(b.Bytes())
	return sum[:]
}

func putInt(b *bytes.Buffer, v int64) {
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], uint64(v))
	b.Write(t[:])
}

func putStr(b *bytes.Buffer, s string) {
	var t [binary.MaxVarintLen64]byte
	b.Write(t[:binary.PutUvarint(t[:], uint64(len(s)))])
	b.WriteString(s)
}

// putOpt encodes an optional string: absent (NULL) differs from present-but-empty.
func putOpt(b *bytes.Buffer, s string) {
	if s == "" {
		b.WriteByte(0)
		return
	}
	b.WriteByte(1)
	putStr(b, s)
}

// Head is the chain's tip.
type Head struct {
	Hash  []byte
	Count int64
}

// HashHex renders the head hash ("" for an empty log).
func (h Head) HashHex() string {
	if h.Count == 0 {
		return ""
	}
	return hex.EncodeToString(h.Hash)
}

// Head returns the head hash and row count.
func (l *Logger) Head(ctx context.Context) (Head, error) { return head(ctx, l.st.R) }

func head(ctx context.Context, db *sql.DB) (Head, error) {
	var h Head
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM audit_log`).Scan(&h.Count); err != nil {
		return h, err
	}
	if h.Count == 0 {
		return h, nil
	}
	err := db.QueryRowContext(ctx, `SELECT hash FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&h.Hash)
	return h, err
}

// LogHead logs the head hash and row count (called once at startup).
func (l *Logger) LogHead(ctx context.Context) error {
	h, err := l.Head(ctx)
	if err != nil {
		return err
	}
	l.log.Info("audit log", "rows", h.Count, "head", h.HashHex(), "boot_id", l.bootID)
	return nil
}

// Status of an intent.
type Status string

const (
	StatusPending Status = "pending" // no outcome yet, written in this boot: probably still running
	StatusUnknown Status = "unknown" // no outcome, written in an earlier boot: it may or may not have run
	StatusOK      Status = "ok"
	StatusError   Status = "error"
	StatusDenied  Status = "denied"
)

// IntentStatus reports what became of an intent: an outcome row (from any
// boot, joined by intent_id) decides; otherwise pending in this boot and
// unknown from an older one. An outcome written in a later boot (the M3a
// reconciler establishing what happened) therefore resolves an unknown intent.
func (l *Logger) IntentStatus(ctx context.Context, intentID int64) (Status, error) {
	var boot string
	if err := l.st.R.QueryRowContext(ctx, `SELECT boot_id FROM audit_log WHERE id = ? AND phase = ?`, intentID, PhaseIntent).Scan(&boot); err != nil {
		return "", fmt.Errorf("audit: intent %d: %w", intentID, err)
	}
	var result string
	switch err := l.st.R.QueryRowContext(ctx, `SELECT result FROM audit_log WHERE intent_id = ? AND phase = ? ORDER BY id DESC LIMIT 1`, intentID, PhaseOutcome).Scan(&result); {
	case err == nil:
		return Status(result), nil
	case errors.Is(err, sql.ErrNoRows):
		if boot == l.bootID {
			return StatusPending, nil
		}
		return StatusUnknown, nil
	default:
		return "", err
	}
}

// Redactor is what the audit flow needs to scrub error text.
type Redactor interface{ Err(error) string }

var _ Redactor = (*redact.Redactor)(nil)
