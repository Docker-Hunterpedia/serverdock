package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

// Agent op protocol v1 (ADR 0012). One yamux stream per op. The hub writes one
// request line; the agent answers with progress or event lines and exactly one
// result line. Every line is one JSON object followed by "\n" and nothing else.
const (
	// ProtocolVersion is the highest version this build speaks.
	ProtocolVersion = 1
	// MaxRequestLine bounds a v1 request line (without its newline).
	MaxRequestLine = 64 << 10
	// MaxProgressLine bounds a progress or event line (without its newline).
	MaxProgressLine = 1 << 10
	// DefaultMaxResult bounds a result's payload unless the op says otherwise.
	DefaultMaxResult = 16 << 10
	// MaxHelloResult bounds the `hello` result payload.
	MaxHelloResult = 256
	// MaxErrorLine bounds a failed result line (without its newline).
	MaxErrorLine = 1 << 10
	// MaxOpBytes bounds the bytes the hub accepts from one op (events excepted).
	MaxOpBytes = 4 << 20
	// HelloTimeout bounds the handshake.
	HelloTimeout = 5 * time.Second
	// resultSlack is the envelope allowance around a result payload.
	resultSlack = 64
)

// OpEvents is the v1 op that streams events (added in a later step); its event
// lines are not counted against MaxOpBytes.
const OpEvents = "events"

// Error codes carried by a failed result.
const (
	CodeInvalid        = "invalid"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodeRefusedUnowned = "refused_unowned"
	CodePolicy         = "policy"
	CodeDocker         = "docker"
	CodeTimeout        = "timeout"
	CodeStalled        = "stalled"
	CodeDiskFull       = "disk_full"
	CodeCancelled      = "cancelled"
)

var errorCodes = map[string]bool{
	CodeInvalid: true, CodeNotFound: true, CodeConflict: true, CodeRefusedUnowned: true,
	CodePolicy: true, CodeDocker: true, CodeTimeout: true, CodeStalled: true,
	CodeDiskFull: true, CodeCancelled: true,
}

// ValidErrorCode reports whether code is one of the protocol's error codes.
func ValidErrorCode(code string) bool { return errorCodes[code] }

// ErrLineTooLong is returned by ReadLine for a line over the limit.
var ErrLineTooLong = errors.New("agent: line too long")

// ReadLine reads one "\n"-terminated line of at most limit bytes (the newline
// is not counted and is not returned). A line over the limit, or a stream that
// ends before the newline, is an error: nothing partial is ever returned.
func ReadLine(br *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > limit+1 { // +1: the newline itself
			return nil, ErrLineTooLong
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

// decodeStrict decodes exactly one JSON object from b into dst: unknown fields
// are refused and so is every byte after the object (spaces included), since
// the line's own newline has already been removed.
func decodeStrict(b []byte, dst any) error {
	if len(b) == 0 || b[0] != '{' {
		return errors.New("not a JSON object")
	}
	// encoding/json matches keys case-insensitively and keeps the last of a
	// duplicate: refuse both, so what a policy check sees is what is decoded.
	if err := checkKeys(b, reflect.TypeOf(dst)); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if int(dec.InputOffset()) != len(b) {
		return errors.New("trailing bytes after the JSON object")
	}
	return nil
}

// checkKeys walks the JSON object in b (the same bytes decodeStrict then
// decodes) and refuses a duplicate key and any key that is not exactly a json
// tag name of the destination struct (recursively for nested struct fields).
// Values of non-struct fields (json.RawMessage, maps, slices) are skipped here;
// their own decoder applies the same rule when they are decoded.
func checkKeys(b []byte, t reflect.Type) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("not a JSON object")
	}
	return checkObject(dec, t)
}

func structOf(t reflect.Type) (reflect.Type, bool) {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t != nil && t.Kind() == reflect.Struct {
		return t, true
	}
	return nil, false
}

// checkObject runs after the opening brace has been read.
func checkObject(dec *json.Decoder, t reflect.Type) error {
	st, isStruct := structOf(t)
	seen := map[string]bool{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := kt.(string)
		if seen[key] {
			return fmt.Errorf("duplicate key %q", CleanText(key, 64))
		}
		seen[key] = true
		var ft reflect.Type
		if isStruct {
			f, ok := fieldByTag(st, key)
			if !ok {
				return fmt.Errorf("unknown field %q", CleanText(key, 64))
			}
			ft = f.Type
		}
		vt, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := vt.(json.Delim); ok {
			if d == '{' {
				if _, nested := structOf(ft); nested {
					if err := checkObject(dec, ft); err != nil {
						return err
					}
					continue
				}
			}
			if err := skipContainer(dec); err != nil {
				return err
			}
		}
	}
	_, err := dec.Token() // the closing brace
	return err
}

// skipContainer consumes tokens up to the end of the container just opened.
func skipContainer(dec *json.Decoder) error {
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
	}
	return nil
}

func fieldByTag(t reflect.Type, key string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		if name == key {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// DecodeArgs decodes an op's args strictly into dst. Absent or null args decode
// as an empty object.
func DecodeArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	if err := decodeStrict(raw, dst); err != nil {
		return fmt.Errorf("bad args: %w", err)
	}
	return nil
}

// Request is a v1 request line.
type Request struct {
	V    int             `json:"v"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

// requestPrefix starts every v1 request the hub writes (json.Marshal emits the
// fields in order). The agent routes on it: anything else is a v0 line.
const requestPrefix = `{"v":`

// IsV1Request reports whether a line is routed to the v1 path.
func IsV1Request(line []byte) bool { return bytes.HasPrefix(line, []byte(requestPrefix)) }

// MarshalRequest builds a request line (with its newline). args may be nil.
//
// v is the session's negotiated version. `hello` is frozen: its envelope is
// always v:1, args {hub_max_v}, result {v, agent}, so a future v2 hub and a v1
// agent can still negotiate (ADR 0012).
func MarshalRequest(op string, args any, v int) ([]byte, error) {
	if op == OpHello {
		v = 1
	}
	r := Request{V: v, Op: op}
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		r.Args = b
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxRequestLine {
		return nil, errors.New("request line too long")
	}
	return append(b, '\n'), nil
}

// ParseV1Request decodes a request line (newline removed): at most
// MaxRequestLine bytes, no unknown envelope field, no byte after the object,
// version 1 and a known op.
func ParseV1Request(line []byte) (Request, error) { return parseV1Request(line, SpecFor) }

// parseV1Request is ParseV1Request against a lookup (the agent's own table).
// Any 1 <= v <= ProtocolVersion is accepted, except for `hello`, which is
// always exactly v:1.
func parseV1Request(line []byte, lookup func(string) (OpSpec, bool)) (Request, error) {
	var r Request
	if len(line) > MaxRequestLine {
		return r, errors.New("request line too long")
	}
	if err := decodeStrict(line, &r); err != nil {
		return Request{}, fmt.Errorf("bad request: %w", err)
	}
	if r.V < 1 || r.V > ProtocolVersion || (r.Op == OpHello && r.V != 1) {
		return Request{}, fmt.Errorf("unsupported protocol version %d", r.V)
	}
	if _, ok := lookup(r.Op); !ok || r.Op == OpLogs {
		return Request{}, fmt.Errorf("unknown op %q", CleanText(r.Op, 64))
	}
	return r, nil
}

// Line types.
const (
	LineProgress = "progress"
	LineEvent    = "event"
	LineResult   = "result"
)

// OpError is the error body of a failed result.
type OpError struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

func (e *OpError) Error() string { return e.Code + ": " + e.Msg }

// Line is one response line from the agent.
type Line struct {
	T   string          `json:"t"`
	P   json.RawMessage `json:"p,omitempty"`
	E   json.RawMessage `json:"e,omitempty"`
	OK  *bool           `json:"ok,omitempty"`
	R   json.RawMessage `json:"r,omitempty"`
	Err *OpError        `json:"err,omitempty"`
}

// ParseAgentLine decodes and validates one response line (newline removed)
// against the op's spec: strict decode, trailing bytes refused, the type's size
// cap, the fields that belong to the type and no others. The error text of a
// failed result is cleaned. A returned error is a protocol violation.
func ParseAgentLine(line []byte, spec OpSpec) (Line, error) {
	var l Line
	if !utf8.Valid(line) {
		return Line{}, errors.New("invalid UTF-8")
	}
	if err := decodeStrict(line, &l); err != nil {
		return Line{}, err
	}
	switch l.T {
	case LineProgress:
		if len(line) > MaxProgressLine || len(l.P) == 0 || l.E != nil || l.OK != nil || l.R != nil || l.Err != nil {
			return Line{}, errors.New("bad progress line")
		}
	case LineEvent:
		if spec.Name != OpEvents {
			return Line{}, errors.New("event line from an op that has no events")
		}
		if len(line) > MaxProgressLine || len(l.E) == 0 || l.P != nil || l.OK != nil || l.R != nil || l.Err != nil {
			return Line{}, errors.New("bad event line")
		}
	case LineResult:
		if l.OK == nil || l.P != nil || l.E != nil {
			return Line{}, errors.New("bad result line")
		}
		if *l.OK {
			if len(l.R) == 0 || l.Err != nil || len(l.R) > spec.maxResult() {
				return Line{}, errors.New("bad or oversize result")
			}
		} else {
			if l.Err == nil || l.R != nil || len(line) > MaxErrorLine || !ValidErrorCode(l.Err.Code) {
				return Line{}, errors.New("bad error result")
			}
			l.Err.Msg = CleanText(l.Err.Msg, MaxReasonText)
		}
	default:
		return Line{}, errors.New("unknown line type")
	}
	return l, nil
}

func (s OpSpec) maxResult() int {
	if s.MaxResult > 0 {
		return s.MaxResult
	}
	return DefaultMaxResult
}

// MaxLineBytes is the most a hub reads for one line of this op.
func (s OpSpec) MaxLineBytes() int {
	return max(s.maxResult()+resultSlack, MaxProgressLine, MaxErrorLine)
}

// HelloArgs is the `hello` request.
type HelloArgs struct {
	HubMaxV int `json:"hub_max_v"`
}

// HelloResult is the `hello` result.
type HelloResult struct {
	V     int    `json:"v"`
	Agent string `json:"agent"`
}
