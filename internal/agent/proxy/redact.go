package proxy

import (
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
		if f.Type == zapcore.ObjectMarshalerType {
			if om, ok := f.Interface.(zapcore.ObjectMarshaler); ok {
				f.Interface = redactingObject{om}
			}
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

// redactMap replaces, at any depth, the value of a header named in redactedHeaders.
func redactMap(m map[string]any) {
	for k, v := range m {
		switch x := v.(type) {
		case map[string]any:
			redactMap(x)
		default:
			for _, h := range redactedHeaders() {
				if strings.EqualFold(k, h) {
					m[k] = []any{"REDACTED"}
				}
			}
		}
	}
}
