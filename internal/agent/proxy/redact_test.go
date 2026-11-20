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
