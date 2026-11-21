package agent

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Stream protocol v0 (the `logs` op, unchanged): the server opens a yamux
// stream and writes one JSON line, for example
// {"op":"logs","container":"abc","tail":100}\n. The agent answers with raw log
// bytes until the stream ends. Every other op speaks v1 (v1.go, ADR 0012).
const (
	// MaxOpLine bounds the v0 op line the agent will read.
	MaxOpLine = 4096
	// OpLogs streams container logs.
	OpLogs = "logs"
	// OpHello is the version handshake.
	OpHello = "hello"
	// The local-only v1 ops (plan 0004, ADR 0012). Their handlers live in
	// internal/agentops; the table below is the only place their limits are set.
	OpImagePull        = "image_pull"
	OpImageInspect     = "image_inspect"
	OpContainerCreate  = "container_create"
	OpContainerStart   = "container_start"
	OpContainerRestart = "container_restart"
	OpContainerInspect = "container_inspect"
	OpContainerStop    = "container_stop"
	OpContainerRemove  = "container_remove"
	OpContainerList    = "container_list"
	OpContainerPorts   = "container_ports"
	OpNetworkEnsure    = "network_ensure"
	OpNetworkInspect   = "network_inspect"
	OpHealthCheck      = "health_check"
	// MinTail and MaxTail bound the log tail.
	MinTail = 1
	MaxTail = 5000
)

// OpSpec describes one stream op. The zero value is local-only: the hub refuses
// an op on a remote agent session unless RemoteOK is set (docs/security.md
// rule 9: remote agents are read-only until Phase 2), so forgetting the field
// fails safe.
//
// MaxResult, MaxProgressLines and Timeout apply to v1 ops (v1.go): MaxResult 0
// means DefaultMaxResult; MaxProgressLines 0 means the op sends no progress
// lines; Timeout 0 means the op is bounded by its context only (`events`), so
// every other op sets one.
type OpSpec struct {
	Name             string
	RemoteOK         bool
	MaxResult        int
	MaxProgressLines int
	Timeout          time.Duration
}

// opSpecs is the table of every op the agent protocol knows. `hello` and
// `logs` are the only ops with RemoteOK. A new op must stay local-only unless
// it is read-only and security-reviewed (a test pins the allowlist).
var opSpecs = []OpSpec{
	{Name: OpLogs, RemoteOK: true},
	{Name: OpHello, RemoteOK: true, MaxResult: MaxHelloResult, Timeout: HelloTimeout},

	// Local-only (RemoteOK false): they change Docker state or read it widely.
	// Every one except `events` has a Timeout; `events` is bounded by its
	// context (the hub's reconnect loop owns it).
	{Name: OpImagePull, MaxProgressLines: MaxPullProgressLines, Timeout: 30 * time.Minute},
	{Name: OpImageInspect, MaxResult: 32 << 10, Timeout: shortOpTimeout},
	{Name: OpContainerCreate, Timeout: 2 * shortOpTimeout},
	{Name: OpContainerStart, Timeout: shortOpTimeout},
	{Name: OpContainerRestart, Timeout: stopOpTimeout},
	{Name: OpContainerInspect, Timeout: shortOpTimeout},
	{Name: OpContainerStop, Timeout: stopOpTimeout},
	{Name: OpContainerRemove, Timeout: shortOpTimeout},
	{Name: OpContainerList, MaxResult: 512 << 10, Timeout: shortOpTimeout},
	{Name: OpContainerPorts, MaxResult: 512 << 10, Timeout: shortOpTimeout},
	{Name: OpNetworkEnsure, Timeout: shortOpTimeout},
	{Name: OpNetworkInspect, MaxResult: 64 << 10, Timeout: shortOpTimeout},
	{Name: OpEvents, Timeout: 0},
	{Name: OpHealthCheck, Timeout: shortOpTimeout},
}

const (
	// MaxPullProgressLines bounds the progress lines of one image_pull (the
	// agent sends at most two a second, so this is far above a real pull).
	MaxPullProgressLines = 20000
	// shortOpTimeout covers an op that makes a few short Docker calls (each is
	// bounded at 30 s by the Docker client).
	shortOpTimeout = 45 * time.Second
	// stopOpTimeout covers a stop or restart: the longest grace (300 s), the
	// Docker client's margin (15 s) and the inspect before it.
	stopOpTimeout = 390 * time.Second
)

// Specs returns a copy of the op table.
func Specs() []OpSpec { return append([]OpSpec(nil), opSpecs...) }

// SpecFor returns the spec of a known op.
func SpecFor(name string) (OpSpec, bool) {
	for _, s := range opSpecs {
		if s.Name == name {
			return s, true
		}
	}
	return OpSpec{}, false
}

var containerIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// ValidContainerID reports whether id is a safe Docker container ID or name.
func ValidContainerID(id string) bool { return containerIDRe.MatchString(id) }

// Op is the first line of a stream.
type Op struct {
	Op        string `json:"op"`
	Container string `json:"container"`
	Tail      int    `json:"tail"`
	// Follow is optional: absent means true. The hub never sends it to a v0
	// session (an M1/M2 agent refuses the unknown field).
	Follow *bool `json:"follow,omitempty"`
}

// Following reports whether the log stream follows new output.
func (o Op) Following() bool { return o.Follow == nil || *o.Follow }

// Validate checks the op fields.
func (o Op) Validate() error {
	// Only `logs` speaks v0; every other op is a v1 request.
	if o.Op != OpLogs {
		return fmt.Errorf("unknown op %q", o.Op)
	}
	if !ValidContainerID(o.Container) {
		return errors.New("invalid container id")
	}
	if o.Tail < MinTail || o.Tail > MaxTail {
		return fmt.Errorf("tail must be %d..%d", MinTail, MaxTail)
	}
	return nil
}

// ParseOp decodes and validates an op line. Unknown fields are rejected.
func ParseOp(line []byte) (Op, error) {
	if len(line) > MaxOpLine {
		return Op{}, errors.New("op line too long")
	}
	var o Op
	if err := decodeStrict(bytes.TrimSuffix(line, []byte("\n")), &o); err != nil {
		return Op{}, fmt.Errorf("bad op: %w", err)
	}
	return o, o.Validate()
}
