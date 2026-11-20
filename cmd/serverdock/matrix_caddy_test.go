package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The subset of the route matrix (internal/api/routematrix_test.go, which runs
// in-process) that must also hold through the real embedded Caddy, TLS and the
// unix socket: the Host allowlist, the Origin and content-type rules, credential
// handling on a few auth routes, the SPA fallback with its CSP, and the :80
// redirect. A request that behaves differently here than in-process means the
// proxy changed something it must not.
func TestRouteMatrixSubsetThroughRealCaddy(t *testing.T) {
	addrs, stop, _ := panelServer(t, newData(t))
	c := insecureClient()
	defer c.CloseIdleConnections()
	base := "https://" + addrs.HTTPS

	type tc struct {
		name, method, path string
		host               string // "" = the address used
		hdr                map[string]string
		body               string
		want               int
		code               string // error code in the envelope, "" = not checked
	}
	jsonHdr := map[string]string{"Content-Type": "application/json", "Origin": base}
	cases := []tc{
		{"healthz", "GET", "/healthz", "", nil, "", 200, ""},
		{"setup status is public", "GET", "/api/v1/setup/status", "", nil, "", 200, ""},
		{"openapi needs a session or a token", "GET", "/api/v1/openapi.json", "", nil, "", 401, "unauthenticated"},
		{"a listed panel host is accepted", "GET", "/api/v1/setup/status", "panel.example.com", nil, "", 200, ""},
		{"a foreign Host is misdirected", "GET", "/api/v1/setup/status", "evil.example", nil, "", 421, "misdirected"},
		{"a foreign Host is misdirected on a POST too", "POST", "/api/v1/auth/login", "evil.example", jsonHdr, `{}`, 421, "misdirected"},
		{"the session route needs a session", "GET", "/api/v1/auth/session", "", nil, "", 401, "unauthenticated"},
		{"an action needs a credential", "POST", "/api/v1/actions/system.info", "", jsonHdr, `{}`, 401, "unauthenticated"},
		{"bad credentials are 401 and say what to do", "POST", "/api/v1/auth/login", "", jsonHdr, `{"username":"admin","password":"x"}`, 401, "invalid_credentials"},
		{"a foreign Origin is refused", "POST", "/api/v1/auth/login", "", map[string]string{"Content-Type": "application/json", "Origin": "https://evil.test"}, `{}`, 403, "origin"},
		{"the pending-2FA route has no cookie", "POST", "/api/v1/auth/2fa", "", jsonHdr, `{"code":"123456"}`, 401, "unauthenticated"},
		{"setup enrol with a wrong token", "POST", "/api/v1/setup/enroll", "", jsonHdr, `{"token":"sd_setup_nope","username":"admin"}`, 401, "invalid_token"},
		{"a token in the URL is refused", "POST", "/api/v1/setup/enroll?token=x", "", jsonHdr, `{"token":"x","username":"admin"}`, 400, "invalid_input"},
		{"the debug prefix is never the SPA", "GET", "/debug/pprof/", "", nil, "", 404, ""},
		{"an API path that does not exist", "GET", "/api/v1/nope", "", nil, "", 404, ""},
	}
	for _, k := range cases {
		t.Run(k.name, func(t *testing.T) {
			req, err := http.NewRequest(k.method, base+k.path, strings.NewReader(k.body))
			if err != nil {
				t.Fatal(err)
			}
			if k.host != "" {
				req.Host = k.host
			}
			for h, v := range k.hdr {
				req.Header.Set(h, v)
			}
			var resp *http.Response
			for i := 0; i < 50; i++ { // Caddy may need a moment after Load
				if resp, err = c.Do(req.Clone(req.Context())); err == nil {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != k.want {
				t.Fatalf("%s %s: %d, want %d: %s", k.method, k.path, resp.StatusCode, k.want, b)
			}
			if k.code != "" && !strings.Contains(string(b), `"code":"`+k.code+`"`) {
				t.Errorf("body %s lacks code %q", b, k.code)
			}
		})
	}

	t.Run("a non-JSON POST is refused", func(t *testing.T) {
		req, _ := http.NewRequest("POST", base+"/api/v1/auth/login", strings.NewReader("username=a"))
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Origin", base)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 && resp.StatusCode != 415 {
			t.Errorf("text/plain: %d", resp.StatusCode)
		}
	})

	t.Run("the SPA fallback carries the CSP and no-cache", func(t *testing.T) {
		resp, err := c.Get(base + "/settings/security")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		csp := resp.Header.Get("Content-Security-Policy")
		if resp.StatusCode != 200 || !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "unsafe-inline") {
			t.Errorf("%d CSP %q", resp.StatusCode, csp)
		}
		if resp.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("Cache-Control %q", resp.Header.Get("Cache-Control"))
		}
	})

	t.Run("the envelope is the one the in-process matrix checks", func(t *testing.T) {
		resp, err := c.Get(base + "/api/v1/auth/session")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var env struct {
			Error struct {
				Code      string `json:"code"`
				Message   string `json:"message"`
				RequestID string `json:"request_id"`
			} `json:"error"`
		}
		if err := jsonDecode(resp.Body, &env); err != nil || env.Error.Code == "" || env.Error.RequestID == "" {
			t.Errorf("%+v %v", env, err)
		}
	})

	t.Run(":80 redirects a POST and never reaches the API", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "http://"+addrs.Listen+"/api/v1/auth/login", strings.NewReader(`{}`))
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 308 {
			t.Errorf(":80 POST: %d", resp.StatusCode)
		}
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func jsonDecode(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }
