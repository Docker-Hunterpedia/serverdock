package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// The CSRF header never reaches a Caddy log line, in the request object Caddy logs
// at DEBUG and in error lines; other headers are untouched, and Caddy's own
// redaction of Cookie and Authorization still shows.
func TestRedactEncoderHidesTheCSRFHeader(t *testing.T) {
	e := &redactEncoder{}
	if err := e.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "https://127.0.0.1/api/v1/x", nil)
	req.Header.Set("X-SD-CSRF", "csrf-sentinel-value-0123456789")
	req.Header.Set("Origin", "https://127.0.0.1")
	req.Header.Set("Cookie", "__Host-sd_session=session-sentinel-0123456789")
	req = caddyhttpRequest(req)
	// Fields attached with With (what reverse_proxy does) go through AddObject.
	withEnc := e.Clone()
	if err := withEnc.AddObject("request", caddyhttp.LoggableHTTPRequest{Request: req}); err != nil {
		t.Fatal(err)
	}
	wb, err := withEnc.EncodeEntry(zapcore.Entry{Level: zapcore.DebugLevel, Message: "m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := wb.String(); strings.Contains(out, "csrf-sentinel-value") || !strings.Contains(out, `"X-Sd-Csrf":["REDACTED"]`) {
		t.Errorf("a With field leaked: %s", out)
	}
	for _, tc := range []struct {
		name   string
		fields []zapcore.Field
	}{
		{"request", []zapcore.Field{zap.Object("request", caddyhttp.LoggableHTTPRequest{Request: req})}},
		{"with an error", []zapcore.Field{zap.Error(errTest{}), zap.Object("request", caddyhttp.LoggableHTTPRequest{Request: req})}},
	} {
		buf, err := e.Clone().EncodeEntry(zapcore.Entry{Level: zapcore.DebugLevel, Message: "m"}, tc.fields)
		if err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if strings.Contains(out, "csrf-sentinel-value") || strings.Contains(out, "session-sentinel") {
			t.Errorf("%s: a secret is in the log line: %s", tc.name, out)
		}
		if !strings.Contains(out, `"X-Sd-Csrf":["REDACTED"]`) || !strings.Contains(out, `"Origin":["https://127.0.0.1"]`) {
			t.Errorf("%s: unexpected line: %s", tc.name, out)
		}
	}
}

type errTest struct{}

func (errTest) Error() string { return "boom" }

func caddyhttpRequest(r *http.Request) *http.Request { return r }

// The hardening cases: a header map inside a slice, and reflected header values
// (http.Header and map[string][]string), are redacted without being mutated.
func TestRedactEncoderHardening(t *testing.T) {
	e := &redactEncoder{}
	if err := e.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	const secret = "csrf-sentinel-value-0123456789"
	encode := func(fields ...zapcore.Field) string {
		buf, err := e.Clone().EncodeEntry(zapcore.Entry{Level: zapcore.DebugLevel, Message: "m"}, fields)
		if err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	hdr := http.Header{"X-Sd-Csrf": {secret}, "Origin": {"https://x"}}
	plain := map[string][]string{"x-sd-csrf": {secret}, "Accept": {"*/*"}}
	cases := map[string]string{
		"http.Header":         encode(zap.Reflect("h", hdr)),
		"map[string][]string": encode(zap.Reflect("h", plain)),
		"zap.Any header":      encode(zap.Any("h", hdr)),
		"in a slice":          encode(zap.Reflect("reqs", []any{map[string]any{"headers": map[string]any{"X-Sd-Csrf": []any{secret}}}})),
		"slice of headers":    encode(zap.Reflect("hs", []http.Header{hdr})),
	}
	for name, out := range cases {
		if strings.Contains(out, secret) {
			t.Errorf("%s: the secret is in the line: %s", name, out)
		}
	}
	if out := cases["http.Header"]; !strings.Contains(out, `"Origin":["https://x"]`) || !strings.Contains(out, "REDACTED") {
		t.Errorf("other headers must survive: %s", out)
	}
	// The caller's values are not mutated.
	if hdr["X-Sd-Csrf"][0] != secret || plain["x-sd-csrf"][0] != secret {
		t.Error("the encoder changed the caller's map")
	}
	// A With field that is reflected goes through AddReflected.
	w := e.Clone()
	if err := w.AddReflected("h", hdr); err != nil {
		t.Fatal(err)
	}
	b, _ := w.EncodeEntry(zapcore.Entry{Message: "m"}, nil)
	if strings.Contains(b.String(), secret) {
		t.Errorf("a With reflected field leaked: %s", b.String())
	}
}
