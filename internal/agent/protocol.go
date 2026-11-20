package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// Stream protocol: the server opens a yamux stream and writes one JSON line,
// for example {"op":"logs","container":"abc","tail":100}\n. The agent answers
// with raw log bytes until the stream ends.
const (
	// MaxOpLine bounds the op line the agent will read.
	MaxOpLine = 4096
	// OpLogs streams container logs.
	OpLogs = "logs"
	// MinTail and MaxTail bound the log tail.
	MinTail = 1
	MaxTail = 5000
)

// OpSpec describes one stream op. LocalOnly ops are refused on remote agent
// sessions by the hub (docs/security.md rule 9: remote agents are read-only
// until Phase 2); only the in-process local agent may serve them.
type OpSpec struct {
	Name      string
	LocalOnly bool
}

// OpSpecs is the table of every op the agent protocol knows. `logs` is the only
// op that is not LocalOnly. New ops must be LocalOnly unless they are read-only
// and security-reviewed (a test enforces this).
var opSpecs = []OpSpec{
	{Name: OpLogs, LocalOnly: false},
}

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
}

// Validate checks the op fields.
func (o Op) Validate() error {
	if _, ok := SpecFor(o.Op); !ok || o.Op != OpLogs {
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
	dec := json.NewDecoder(bytesReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return Op{}, fmt.Errorf("bad op: %w", err)
	}
	if dec.More() {
		return Op{}, errors.New("bad op: trailing data")
	}
	return o, o.Validate()
}
