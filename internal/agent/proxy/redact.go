package proxy

import (
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

// Caddy redacts the Cookie, Set-Cookie and Authorization headers when it logs a
// request, but not our CSRF header. At DEBUG (and in an error line such as a
// failed upstream dial) the request, headers included, is logged, so the CSRF
// token would land in the log. This log encoder, used only on the server's panel
// config, wraps Caddy's JSON output and replaces the values of the headers in
// redactedHeaders before they are written. The agent's Caddy does not use it.

// redactedHeaders are lower-case header names whose values never reach a log.
func redactedHeaders() []string { return []string{"x-sd-csrf"} }

const redactEncoderName = "sd_redact"

var registerOnce sync.Once

// registerRedactEncoder registers the module on first use (not at package load:
// the agent never needs it).
func registerRedactEncoder() { registerOnce.Do(func() { caddy.RegisterModule(&redactEncoder{}) }) }

type redactEncoder struct{ zapcore.Encoder }

// CaddyModule implements caddy.Module.
func (*redactEncoder) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "caddy.logging.encoders." + redactEncoderName, New: func() caddy.Module { return new(redactEncoder) }}
}

// Provision implements caddy.Provisioner: Caddy's default JSON layout.
func (e *redactEncoder) Provision(caddy.Context) error {
	cfg := zap.NewProductionEncoderConfig()
	cfg.EncodeTime = zapcore.EpochTimeEncoder
	cfg.EncodeDuration = zapcore.SecondsDurationEncoder
	cfg.EncodeLevel = zapcore.LowercaseLevelEncoder
	e.Encoder = zapcore.NewJSONEncoder(cfg)
	return nil
}

// Clone implements zapcore.Encoder.
func (e *redactEncoder) Clone() zapcore.Encoder { return &redactEncoder{Encoder: e.Encoder.Clone()} }

// AddReflected implements zapcore.ObjectEncoder: a header map attached as a
// reflected field is redacted too.
func (e *redactEncoder) AddReflected(key string, v any) error {
	return e.Encoder.AddReflected(key, redactValue(v))
}

// AddObject implements zapcore.ObjectEncoder. Fields attached to a logger with
// With (reverse_proxy does that for the request it logs at DEBUG) are written to
// the encoder through this method, not through EncodeEntry.
func (e *redactEncoder) AddObject(key string, m zapcore.ObjectMarshaler) error {
	return e.Encoder.AddObject(key, redactingObject{m})
}

// EncodeEntry implements zapcore.Encoder.
func (e *redactEncoder) EncodeEntry(ent zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	out := make([]zapcore.Field, len(fields))
	for i, f := range fields {
		switch f.Type {
		case zapcore.ObjectMarshalerType:
			if om, ok := f.Interface.(zapcore.ObjectMarshaler); ok {
				f.Interface = redactingObject{om}
			}
		case zapcore.ReflectType:
			f.Interface = redactValue(f.Interface)
		}
		out[i] = f
	}
	return e.Encoder.EncodeEntry(ent, out)
}

// redactingObject re-emits a logged object with the redacted headers replaced.
type redactingObject struct{ zapcore.ObjectMarshaler }

func (o redactingObject) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	m := zapcore.NewMapObjectEncoder()
	if err := o.ObjectMarshaler.MarshalLogObject(m); err != nil {
		return err
	}
	redactMap(m.Fields)
	keys := make([]string, 0, len(m.Fields))
	for k := range m.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := enc.AddReflected(k, m.Fields[k]); err != nil {
			return err
		}
	}
	return nil
}

// redactMap redacts, at any depth, the value of a header named in redactedHeaders.
func redactMap(m map[string]any) {
	for k, v := range m {
		if isRedacted(k) {
			m[k] = []any{"REDACTED"}
			continue
		}
		m[k] = redactValue(v)
	}
}

func isRedacted(name string) bool {
	for _, h := range redactedHeaders() {
		if strings.EqualFold(name, h) {
			return true
		}
	}
	return false
}

// redactValue returns v with every redacted header replaced: maps are changed
// in place, slices element by element, and http.Header and map[string][]string
// values (what a reflected field would hold) are copied first, never mutated.
func redactValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		redactMap(x)
		return x
	case []any:
		for i := range x {
			x[i] = redactValue(x[i])
		}
		return x
	case http.Header:
		return redactHeader(x)
	case map[string][]string:
		return redactHeader(x)
	}
	// Any other slice or array (a []http.Header, say) is rebuilt element by element;
	// a type that is not a collection is returned as it is.
	if rv := reflect.ValueOf(v); v != nil && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) {
		out := make([]any, rv.Len())
		changed := false
		for i := range out {
			el := rv.Index(i).Interface()
			out[i] = redactValue(el)
			changed = changed || !reflect.DeepEqual(out[i], el)
		}
		if changed {
			return out
		}
	}
	return v
}

func redactHeader(h map[string][]string) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, vs := range h {
		if isRedacted(k) {
			out[k] = []string{"REDACTED"}
			continue
		}
		out[k] = vs
	}
	return out
}
