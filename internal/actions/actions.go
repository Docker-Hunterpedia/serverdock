// Package actions is the typed action registry: every operation the control
// plane performs is a registered Def with a risk tier, a scope, a validated
// input type and an audit trail. M1 has no HTTP exposure; M2 adds routes and
// the JSON Schema reflector on top of this.
package actions

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"regexp"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/redact"
)

// MaxInputBytes bounds an action's raw input.
const MaxInputBytes = 64 << 10

// Risk is an action's tier.
type Risk int

// Risk tiers.
const (
	RiskRead Risk = iota + 1
	RiskMutate
	RiskDestructive
)

func (r Risk) String() string {
	switch r {
	case RiskRead:
		return "read"
	case RiskMutate:
		return "mutate"
	case RiskDestructive:
		return "destructive"
	}
	return "invalid"
}

// Scope is a token or user scope; a higher scope includes the lower ones.
type Scope int

// Scopes.
const (
	ScopeRead Scope = iota + 1
	ScopeWrite
	ScopeAdmin
)

// Validator is an action input that can check itself.
type Validator interface{ Validate() error }

// Plan is what a dry run reports.
type Plan struct {
	Summary string   `json:"summary"`
	Steps   []string `json:"steps,omitempty"`
}

// Call is what Execute and DryRun learn about the invocation.
type Call struct {
	Actor     Actor
	RequestID string
}

// Def declares one action. Register checks it and panics on a bad one, so a
// mistake fails at startup (and in the test that registers every built-in).
type Def[In Validator, Out any] struct {
	Name, Summary string
	Risk          Risk
	Scope         Scope // must fit Risk
	// Sensitive actions are audited intent-first and need fresh 2FA from users;
	// tokens, MCP and webhooks are denied. Secret reveals are Sensitive reads.
	Sensitive bool
	// SessionOnly actions refuse every actor except a user session (token, auth
	// and user management).
	SessionOnly bool
	// System, when non-empty, lets the SystemInvoker run a mutate or destructive
	// action unattended; the text is the written reason. A destructive action
	// with a System reason must be listed in docs/security.md "Unattended
	// destructive actions" (a test checks).
	System string
	// WebhookAllowed lets a webhook actor run a mutate action on its bound target.
	WebhookAllowed bool
	Target         func(In) string
	DryRun         func(context.Context, Call, In) (Plan, error)
	Execute        func(context.Context, Call, In) (Out, error)
}

// Errors returned by Invoke.
var (
	ErrUnknownAction = errors.New("actions: unknown action")
	ErrInvalidInput  = errors.New("actions: invalid input")
	ErrDenied        = errors.New("actions: denied")
	ErrNoDryRun      = errors.New("actions: this action has no dry run")
)

// DeniedError says why the policy refused (the reason is safe to show the actor).
type DeniedError struct{ Reason string }

func (e *DeniedError) Error() string   { return "actions: denied: " + e.Reason }
func (e *DeniedError) Is(t error) bool { return t == ErrDenied }

func denied(format string, a ...any) error { return &DeniedError{Reason: fmt.Sprintf(format, a...)} }

// Options are the per-call inputs an actor controls.
type Options struct {
	DryRun bool
	// Confirm must equal the target for a destructive action run by a user.
	Confirm string
	// RequestID correlates audit rows; generated when empty.
	RequestID string
}

// Result of an invocation.
type Result struct {
	Out  any   // the action's output (nil for a dry run)
	Plan *Plan // set for a dry run
	// Audited is true when intent/outcome rows were written.
	Audited bool
	// AuditOutcomeRecorded is false when the action succeeded but its outcome
	// row could not be written (the error is then audit.ErrAuditOutcome).
	AuditOutcomeRecorded bool
}

// Sink is the audit trail the registry writes to.
type Sink interface {
	audit.Appender
	Denied(ctx context.Context, e audit.Entry) (bool, error)
}

type prepared struct {
	target string
	args   string
	rd     *redact.Redactor
	exec   func(context.Context, Call) (any, error)
	dry    func(context.Context, Call) (Plan, error) // nil when the action has none
}

type action struct {
	name, summary string
	risk          Risk
	scope         Scope
	sensitive     bool
	sessionOnly   bool
	system        string
	webhookOK     bool
	inType        reflect.Type
	prepare       func(raw []byte) (*prepared, error)
}

// Registry holds the actions.
type Registry struct {
	sink Sink
	log  *slog.Logger
	now  func() time.Time

	mu      sync.RWMutex
	actions map[string]*action

	sysMu    sync.Mutex
	sysTaken bool
}

// Option configures New.
type Option func(*Registry)

// WithClock injects the clock (tests).
func WithClock(now func() time.Time) Option { return func(r *Registry) { r.now = now } }

// New returns an empty registry writing to sink.
func New(sink Sink, log *slog.Logger, opts ...Option) *Registry {
	if sink == nil {
		panic("actions: New needs an audit sink")
	}
	r := &Registry{sink: sink, log: log, now: time.Now, actions: map[string]*action{}}
	for _, o := range opts {
		o(r)
	}
	return r
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// Register adds d. It panics when the definition breaks a rule:
//   - the scope does not fit the risk (Read: any scope; Mutate: write or
//     higher; Destructive or Sensitive: admin);
//   - In has a free-form field (map, any/interface, json.RawMessage) without
//     the tag sd:"secret" (redacted) or sd:"audit" (explicitly safe to record);
//   - the name is malformed or taken, or required functions are missing.
func Register[In Validator, Out any](r *Registry, d Def[In, Out]) {
	inType := reflect.TypeFor[In]()
	mustf := func(format string, a ...any) {
		panic(fmt.Sprintf("actions: Register(%q): %s", d.Name, fmt.Sprintf(format, a...)))
	}
	switch {
	case !nameRe.MatchString(d.Name):
		mustf("name must look like area.verb")
	case d.Summary == "":
		mustf("Summary is required")
	case d.Execute == nil:
		mustf("Execute is required")
	}
	switch d.Risk {
	case RiskRead:
	case RiskMutate:
		if d.Scope < ScopeWrite {
			mustf("a mutate action needs write scope or higher")
		}
	case RiskDestructive:
		if d.Scope < ScopeAdmin {
			mustf("a destructive action needs admin scope")
		}
	default:
		mustf("Risk is required")
	}
	if d.Scope < ScopeRead || d.Scope > ScopeAdmin {
		mustf("Scope is required")
	}
	if d.Sensitive && d.Scope < ScopeAdmin {
		mustf("a sensitive action needs admin scope")
	}
	if (d.Risk != RiskRead || d.Sensitive || d.WebhookAllowed) && d.Target == nil {
		mustf("Target is required (audit rows, confirmation and webhook binding use it)")
	}
	if d.WebhookAllowed && (d.Risk != RiskMutate || d.Sensitive || d.SessionOnly) {
		mustf("WebhookAllowed is only for non-sensitive mutate actions that are not SessionOnly")
	}
	if err := checkFreeForm(inType, ""); err != nil {
		mustf("%v", err)
	}

	a := &action{name: d.Name, summary: d.Summary, risk: d.Risk, scope: d.Scope, sensitive: d.Sensitive,
		sessionOnly: d.SessionOnly, system: d.System, webhookOK: d.WebhookAllowed, inType: inType}
	a.prepare = func(raw []byte) (*prepared, error) {
		var in In
		if err := decodeStrict(raw, &in); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		if inType.Kind() == reflect.Pointer && reflect.ValueOf(in).IsNil() {
			return nil, fmt.Errorf("%w: input is required", ErrInvalidInput)
		}
		if err := in.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		args, err := redact.ArgsJSON(in)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		p := &prepared{args: args, rd: redact.ForInput(in)}
		if d.Target != nil {
			p.target = d.Target(in)
		}
		p.exec = func(ctx context.Context, c Call) (any, error) { return d.Execute(ctx, c, in) }
		if d.DryRun != nil {
			p.dry = func(ctx context.Context, c Call) (Plan, error) { return d.DryRun(ctx, c, in) }
		}
		return p, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.actions[d.Name]; dup {
		mustf("already registered")
	}
	r.actions[d.Name] = a
}

func decodeStrict(raw []byte, into any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the input")
	}
	return nil
}

// checkFreeForm rejects map / interface / json.RawMessage fields that are not
// tagged sd:"secret" or sd:"audit", at any depth.
func checkFreeForm(t reflect.Type, path string) error {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() && !f.Anonymous {
			continue
		}
		if !f.IsExported() { // an unexported embedded struct: its exported fields are still input
			if err := checkFreeForm(f.Type, path+f.Name+"."); err != nil {
				return err
			}
			continue
		}
		tag := f.Tag.Get("sd")
		if tag != "" && tag != redact.TagSecret && tag != redact.TagAudit {
			return fmt.Errorf("field %s%s has an unknown sd tag %q", path, f.Name, tag)
		}
		if isFreeForm(f.Type) && tag == "" {
			return fmt.Errorf("field %s%s is free-form (%s): tag it sd:\"secret\" (redacted) or sd:\"audit\" (safe to record)", path, f.Name, f.Type)
		}
		if err := checkFreeForm(f.Type, path+f.Name+"."); err != nil {
			return err
		}
	}
	return nil
}

var rawMessage = reflect.TypeFor[json.RawMessage]()

func isFreeForm(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		if t == rawMessage {
			return true
		}
		t = t.Elem()
	}
	return t == rawMessage || t.Kind() == reflect.Map || t.Kind() == reflect.Interface
}

// Info describes a registered action (for docs and the M2 route table).
type Info struct {
	Name, Summary string
	Risk          Risk
	Scope         Scope
	Sensitive     bool
	SessionOnly   bool
	System        string
	WebhookOK     bool
	InputType     reflect.Type
}

// List returns every action sorted by name.
func (r *Registry) List() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Info, 0, len(r.actions))
	for _, a := range r.actions {
		out = append(out, Info{Name: a.name, Summary: a.summary, Risk: a.risk, Scope: a.scope, Sensitive: a.sensitive,
			SessionOnly: a.sessionOnly, System: a.system, WebhookOK: a.webhookOK, InputType: a.inType})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) lookup(name string) (*action, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.actions[name]
	if !ok {
		return nil, ErrUnknownAction
	}
	return a, nil
}

// Invoke runs an action as actor. Flow: size check and strict decode, Validate,
// policy, audit intent, Execute (or DryRun), audit outcome. A policy refusal
// writes one rate-limited "denied" row and returns a *DeniedError.
func (r *Registry) Invoke(ctx context.Context, actor Actor, name string, raw json.RawMessage, opts Options) (Result, error) {
	return r.invoke(ctx, actor, "", name, raw, opts)
}

// Authorize runs the decode, validation and policy steps without executing or
// auditing success (a denial is still recorded). A job queue calls it at enqueue
// time; the job then runs through SystemInvoker.InvokeAs, which checks again.
func (r *Registry) Authorize(ctx context.Context, actor Actor, name string, raw json.RawMessage, opts Options) error {
	act, p, err := r.prep(name, raw)
	if err != nil {
		return err
	}
	return r.check(ctx, actor, "", act, p, opts)
}

func (r *Registry) prep(name string, raw json.RawMessage) (*action, *prepared, error) {
	act, err := r.lookup(name)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > MaxInputBytes {
		return nil, nil, fmt.Errorf("%w: input is larger than %d bytes", ErrInvalidInput, MaxInputBytes)
	}
	p, err := act.prepare(raw)
	return act, p, err
}

func (r *Registry) check(ctx context.Context, actor Actor, executor string, act *action, p *prepared, opts Options) error {
	err := r.authorize(actor, act, p.target, opts)
	if err == nil {
		return nil
	}
	e := audit.Entry{ActorKind: string(actor.kind), ActorID: actor.id, Executor: executor, Action: act.name, Target: p.target,
		Risk: act.risk.String(), DryRun: opts.DryRun, ArgsJSON: p.args, RequestID: requestID(opts), Error: err.Error()}
	if e.ActorKind == "" {
		e.ActorKind, e.ActorID = "invalid", "invalid"
	}
	if e.ActorID == "" {
		e.ActorID = "unknown"
	}
	if _, derr := r.sink.Denied(ctx, e); derr != nil {
		r.log.Error("audit denied-row write failed", "action", act.name, "request_id", e.RequestID)
	}
	return err
}

func (r *Registry) invoke(ctx context.Context, actor Actor, executor, name string, raw json.RawMessage, opts Options) (Result, error) {
	act, p, err := r.prep(name, raw)
	if err != nil {
		return Result{}, err
	}
	if err := r.check(ctx, actor, executor, act, p, opts); err != nil {
		return Result{}, err
	}
	call := Call{Actor: actor, RequestID: requestID(opts)}
	var run func(context.Context) (Result, error)
	if opts.DryRun {
		if p.dry == nil {
			return Result{}, ErrNoDryRun
		}
		run = func(ctx context.Context) (Result, error) {
			plan, err := p.dry(ctx, call)
			return Result{Plan: &plan}, err
		}
	} else {
		run = func(ctx context.Context) (Result, error) {
			out, err := p.exec(ctx, call)
			return Result{Out: out}, err
		}
	}
	if act.risk == RiskRead && !act.sensitive {
		res, err := protect(ctx, run) // plain reads are not audited
		return res, err
	}
	intent := audit.Entry{ActorKind: string(actor.kind), ActorID: actor.id, Executor: executor, Action: act.name, Target: p.target,
		Risk: act.risk.String(), DryRun: opts.DryRun, ArgsJSON: p.args, RequestID: call.RequestID}
	oc, err := audit.Execute(ctx, r.sink, r.log, p.rd, intent, run)
	res := oc.Value
	res.Audited, res.AuditOutcomeRecorded = oc.IntentID != 0, oc.AuditOutcomeRecorded
	return res, err
}

func protect(ctx context.Context, fn func(context.Context) (Result, error)) (res Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			_ = debug.Stack()
			err = fmt.Errorf("action panicked: %v", p)
		}
	}()
	return fn(ctx)
}

func requestID(o Options) string {
	if o.RequestID != "" {
		return o.RequestID
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
