package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/auth"
	"github.com/Docker-Hunterpedia/serverdock/internal/datadir"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
	"github.com/Docker-Hunterpedia/serverdock/internal/testbin"
)

// The secret sentinel suite (G9, SEC7). A real binary serves the real flows over
// HTTPS through the real embedded Caddy (at DEBUG, through the test-only
// environment variable); every secret that flows through is registered as a
// sentinel, and at the end none may appear anywhere except where it is meant to:
// the one response that hands it to its owner.

// pattern is one sentinel in one form.
type pattern struct{ name, form, value string }

type sentinels struct{ pats []pattern }

func (s *sentinels) str(name, v string) {
	if len(v) >= 16 { // shorter values would match by chance
		s.pats = append(s.pats, pattern{name, "raw", v})
	}
}

// bytes registers a binary secret in every textual form it could be written in.
func (s *sentinels) bytes(name string, raw []byte) {
	forms := map[string]string{
		"raw": string(raw), "hex": hex.EncodeToString(raw), "HEX": strings.ToUpper(hex.EncodeToString(raw)),
		"base64": base64.StdEncoding.EncodeToString(raw), "base64url": base64.URLEncoding.EncodeToString(raw),
		"base64raw": base64.RawStdEncoding.EncodeToString(raw), "base64urlraw": base64.RawURLEncoding.EncodeToString(raw),
		"base32": base32.StdEncoding.EncodeToString(raw), "base32raw": base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw),
	}
	for form, v := range forms {
		if len(v) >= 16 {
			s.pats = append(s.pats, pattern{name, form, v})
		}
	}
}

// find returns the sentinels found in data, skipping any whose value is in allowed.
func (s *sentinels) find(data []byte, allowed []string) []string {
	var hits []string
	for _, p := range s.pats {
		ok := false
		for _, a := range allowed {
			if a == p.value {
				ok = true
			}
		}
		for _, a := range allowed { // a value that contains, or is contained in, an allowed one is the allowed one
			if a != "" && (strings.Contains(a, p.value) || strings.Contains(p.value, a)) {
				ok = true
			}
		}
		if !ok && bytes.Contains(data, []byte(p.value)) {
			hits = append(hits, p.name+" ("+p.form+")")
		}
	}
	return hits
}

func TestSentinelScannerHasTeeth(t *testing.T) {
	var s sentinels
	s.str("password", "correct horse battery sentinel")
	s.bytes("seed", []byte("0123456789abcdefghij"))
	s.bytes("key", bytes.Repeat([]byte{0xAB}, 32))
	for _, leak := range []string{"x correct horse battery sentinel x", string(base64.StdEncoding.EncodeToString([]byte("0123456789abcdefghij"))),
		base32.StdEncoding.EncodeToString([]byte("0123456789abcdefghij")), strings.Repeat("ab", 32), "0123456789abcdefghij"} {
		if len(s.find([]byte(leak), nil)) == 0 {
			t.Errorf("the scanner missed %q", leak)
		}
	}
	if hits := s.find([]byte("nothing to see here, move along please"), nil); len(hits) != 0 {
		t.Errorf("false positive: %v", hits)
	}
	// An allowed value is skipped.
	if hits := s.find([]byte("correct horse battery sentinel"), []string{"correct horse battery sentinel"}); len(hits) != 0 {
		t.Errorf("an allowed value was reported: %v", hits)
	}
}

// ---- a browser-like client over real HTTPS --------------------------------------------

type recorded struct {
	what  string
	text  []byte
	allow func() []string
}

type sclient struct {
	t        *testing.T
	addr     string
	hc       *http.Client
	cookies  map[string]string
	csrf     string
	records  *[]recorded
	headerSn *[]string // Authorization and Cookie header values that were sent
}

func (c *sclient) do(method, path string, body any, hdr map[string]string, allow func() []string) (int, map[string]any) {
	c.t.Helper()
	var bodyBytes []byte
	switch b := body.(type) {
	case nil:
	case string:
		bodyBytes = []byte(b)
	default:
		bodyBytes, _ = json.Marshal(b)
	}
	build := func() *http.Request {
		req, err := http.NewRequest(method, "https://"+c.addr+path, bytes.NewReader(bodyBytes))
		if err != nil {
			c.t.Fatal(err)
		}
		if method == "POST" {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", "https://"+c.addr)
			if c.cookies[api_sessionCookie] != "" && c.csrf != "" {
				req.Header.Set("X-SD-CSRF", c.csrf)
			}
		}
		var names []string
		for n := range c.cookies {
			names = append(names, n)
		}
		sort.Strings(names)
		var parts []string
		for _, n := range names {
			parts = append(parts, n+"="+c.cookies[n])
		}
		if len(parts) > 0 {
			req.Header.Set("Cookie", strings.Join(parts, "; "))
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		return req
	}
	if hv := build().Header.Get("Cookie"); hv != "" {
		*c.headerSn = append(*c.headerSn, hv)
	}
	if hv := hdr["Authorization"]; hv != "" {
		*c.headerSn = append(*c.headerSn, hv)
	}
	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ { // Caddy may need a moment after start
		if resp, err = c.hc.Do(build()); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var hv strings.Builder
	for k, vs := range resp.Header {
		for _, v := range vs {
			hv.WriteString(k + ": " + v + "\n")
		}
	}
	*c.records = append(*c.records, recorded{what: method + " " + path + " -> " + fmt.Sprint(resp.StatusCode), text: append([]byte(hv.String()), raw...), allow: allow})
	for _, sc := range resp.Cookies() {
		if sc.MaxAge < 0 {
			delete(c.cookies, sc.Name)
		} else {
			c.cookies[sc.Name] = sc.Value
		}
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	if v, ok := out["csrf"].(string); ok {
		c.csrf = v
	}
	return resp.StatusCode, out
}

const api_sessionCookie = "__Host-sd_session"

func al(items ...any) func() []string {
	return func() []string {
		var out []string
		for _, it := range items {
			switch v := it.(type) {
			case *string:
				out = append(out, *v)
			case *[]string:
				out = append(out, *v...)
			case string:
				out = append(out, v)
			}
		}
		return out
	}
}

func snapDB(t *testing.T, data string) map[string][]byte {
	out := map[string][]byte{}
	for _, n := range []string{"serverdock.db", "serverdock.db-wal"} {
		if b, err := os.ReadFile(filepath.Join(data, n)); err == nil {
			out[n] = b
		}
	}
	return out
}

func TestSecretsNeverLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and takes about 15 seconds")
	}
	base := testbin.ShortDir(t)
	data := filepath.Join(base, "data")
	httpsAddr := testbin.FreeAddr(t)
	p := testbin.Start(t, testbin.Options{
		Args: []string{"server", "--data", data, "--control-socket", filepath.Join(base, "c.sock"), "--listen", testbin.FreeAddr(t), "--https-listen", httpsAddr},
		Env:  []string{"SERVERDOCK_TEST_PROXY_LOG_LEVEL=DEBUG"},
	})
	for _, w := range []string{"control plane listening", "proxy listening"} {
		if err := p.WaitLog(w, 40*time.Second); err != nil {
			t.Fatal(err)
		}
	}

	var records []recorded
	var sentHeaders []string
	var S sentinels
	snaps := map[string]map[string][]byte{}
	snap := func(label string) { snaps[label] = snapDB(t, data) }
	newClient := func() *sclient {
		return &sclient{t: t, addr: httpsAddr, cookies: map[string]string{}, records: &records, headerSn: &sentHeaders,
			hc: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true},
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 30 * time.Second}}
	}
	c := newClient()

	// The typed secrets.
	P1 := "correct horse battery SENTINEL-one"
	P2 := "a brand new passphrase SENTINEL-two"
	wrongPW := "wrong-password-SENTINEL-three"
	malformed := "MALFORMED-JSON-SENTINEL-five"
	badToken := "sd_" + strings.Repeat("Q", 43)
	S.str("password", P1)
	S.str("new password", P2)
	S.str("wrong password", wrongPW)
	S.str("malformed body", malformed)
	S.str("bad API token", badToken)

	tokB, err := os.ReadFile(filepath.Join(data, "setup-token"))
	if err != nil {
		t.Fatal(err)
	}
	setupTok := strings.TrimSpace(string(tokB))
	S.str("setup token", setupTok)
	S.str("setup token body", strings.TrimPrefix(setupTok, "sd_setup_"))

	// ---- first run: an abandoned enrolment, then the real one ----
	var seedAb32, eidA, seed2B32, eid2 string
	_, en := c.do("POST", "/api/v1/setup/enroll", map[string]string{"token": setupTok, "username": "admin"}, nil, al(&seedAb32, &eidA))
	seedAb32, _ = en["secret"].(string)
	eidA, _ = en["enrollment_id"].(string)
	seedA, err := decodeB32(seedAb32)
	if err != nil {
		t.Fatalf("enrol 1: %v %v", err, en)
	}
	S.bytes("abandoned setup seed", seedA)
	S.str("abandoned enrollment_id", eidA)
	_, en2 := c.do("POST", "/api/v1/setup/enroll", map[string]string{"token": setupTok, "username": "admin"}, nil, al(&seed2B32, &eid2))
	seed2B32, _ = en2["secret"].(string)
	eid2, _ = en2["enrollment_id"].(string)
	seed2, _ := decodeB32(seed2B32)
	S.bytes("setup TOTP seed", seed2)
	S.str("enrollment_id", eid2)
	if uri, _ := en2["otpauth_uri"].(string); !strings.Contains(uri, seed2B32) {
		t.Fatalf("the otpauth URI does not carry the key: %s", uri)
	}
	ctr := auth.TOTPCounter(time.Now())
	var rec1 []string
	var sess1, csrf1 string
	code, done := c.do("POST", "/api/v1/setup/complete", map[string]string{"token": setupTok, "enrollment_id": eid2, "username": "admin", "password": P1,
		"code": auth.TOTPCode(seed2, ctr)}, nil, al(&rec1, &sess1, &csrf1))
	if code != 200 {
		t.Fatalf("setup/complete: %d %v", code, done)
	}
	for _, r := range done["recovery_codes"].([]any) {
		rec1 = append(rec1, r.(string))
		addRecovery(&S, r.(string))
	}
	sess1, csrf1 = c.cookies[api_sessionCookie], c.csrf
	S.str("session id", sess1)
	S.str("CSRF token", csrf1)
	snap("after setup")
	c.do("GET", "/api/v1/auth/session", nil, nil, al(&csrf1))
	c.do("POST", "/api/v1/auth/logout", map[string]string{}, nil, al())

	// ---- deliberate failures: none may echo or store what was typed ----
	fail := func() {
		bad := newClient()
		bad.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": wrongPW}, nil, al())
		// the password submitted as the user name
		bad.do("POST", "/api/v1/auth/login", map[string]string{"username": P1, "password": "x"}, nil, al())
		bad.do("POST", "/api/v1/auth/login", `{"username":"admin","password":"`+malformed, nil, al()) // broken JSON
		bad.do("POST", "/api/v1/auth/login", `{"username":"admin","password":"`+malformed+`","extra":1}`, nil, al())
		bad.do("POST", "/api/v1/actions/system.info", map[string]string{}, map[string]string{"Authorization": "Bearer " + badToken}, al())
		bad.do("GET", "/api/v1/openapi.json", nil, map[string]string{"Authorization": "Bearer " + badToken}, al())
		bad.do("POST", "/api/v1/setup/enroll", map[string]string{"token": setupTok, "username": "admin"}, nil, al()) // setup is closed now
	}
	fail()

	// ---- sign in with TOTP (the next time step), then step up ----
	var pend1, sess2, csrf2 string
	login := func(user, pw string, pend *string) {
		l := newClient()
		*c = *l
		c.cookies = map[string]string{}
		code, _ := c.do("POST", "/api/v1/auth/login", map[string]string{"username": user, "password": pw}, nil, al(pend))
		if code != 200 {
			t.Fatalf("login: %d", code)
		}
		*pend = c.cookies["__Host-sd_pending"]
		S.str("pending id", *pend)
	}
	login("admin", P1, &pend1)
	code, r2 := c.do("POST", "/api/v1/auth/2fa", map[string]string{"code": auth.TOTPCode(seed2, ctr+1)}, nil, al(&sess2, &csrf2))
	if code != 200 {
		t.Fatalf("2fa: %d %v", code, r2)
	}
	sess2, csrf2 = c.cookies[api_sessionCookie], c.csrf
	S.str("session id", sess2)
	S.str("CSRF token", csrf2)
	snap("after login")
	waitForCounter(ctr + 1) // the next step's code is then accepted for step-up
	if code, r := c.do("POST", "/api/v1/auth/reauth", map[string]string{"code": auth.TOTPCode(seed2, ctr+2)}, nil, al()); code != 200 {
		t.Fatalf("reauth: %d %v", code, r)
	}
	c.do("POST", "/api/v1/auth/reauth", map[string]string{"code": "000000"}, nil, al()) // a wrong step-up code

	// ---- API tokens: create (the one response that carries it), use, list ----
	var tok1, tok2 string
	code, tr := c.do("POST", "/api/v1/actions/tokens.create", map[string]any{"name": "sentinel-ci", "scope": "admin", "expires_in_days": 30}, nil, al(&tok1))
	if code != 200 {
		t.Fatalf("tokens.create: %d %v", code, tr)
	}
	tok1 = tr["data"].(map[string]any)["token"].(string)
	S.str("API token", tok1)
	S.str("API token body", strings.TrimPrefix(tok1, "sd_"))
	code, tr = c.do("POST", "/api/v1/actions/tokens.create", map[string]any{"name": "sentinel-second", "scope": "read"}, nil, al(&tok2))
	tok2 = tr["data"].(map[string]any)["token"].(string)
	S.str("API token 2", tok2)
	for _, path := range []string{"/api/v1/actions/system.info", "/api/v1/actions/tokens.list"} {
		tc := newClient()
		if code, _ := tc.do("POST", path, map[string]string{}, map[string]string{"Authorization": "Bearer " + tok1}, al()); code != 200 && path == "/api/v1/actions/system.info" {
			t.Fatalf("using the token: %d", code)
		}
	}
	// A token cannot manage tokens; the refusal says nothing secret.
	tc := newClient()
	tc.do("POST", "/api/v1/actions/tokens.create", map[string]any{"name": "x", "scope": "read"}, map[string]string{"Authorization": "Bearer " + tok1}, al())
	c.do("POST", "/api/v1/actions/tokens.list", map[string]string{}, nil, al())
	snap("after tokens")

	// ---- recovery codes regenerated ----
	var rec2 []string
	code, rr := c.do("POST", "/api/v1/actions/auth.recovery_regenerate", map[string]string{}, nil, al(&rec2))
	if code != 200 {
		t.Fatalf("recovery_regenerate: %d %v", code, rr)
	}
	for _, v := range rr["data"].(map[string]any)["codes"].([]any) {
		rec2 = append(rec2, v.(string))
		addRecovery(&S, v.(string))
	}

	// ---- re-enrolment: one abandoned, one confirmed ----
	var rSeedAb32, rEidA, rSeedB32, rEidB string
	_, b1 := c.do("POST", "/api/v1/actions/auth.totp_reenroll_begin", map[string]string{}, nil, al(&rSeedAb32, &rEidA))
	d1 := b1["data"].(map[string]any)
	rSeedAb32, rEidA = d1["secret"].(string), d1["enrollment_id"].(string)
	rA, _ := decodeB32(rSeedAb32)
	S.bytes("abandoned re-enrolment seed", rA)
	S.str("abandoned re-enrolment id", rEidA)
	_, b2 := c.do("POST", "/api/v1/actions/auth.totp_reenroll_begin", map[string]string{}, nil, al(&rSeedB32, &rEidB))
	d2 := b2["data"].(map[string]any)
	rSeedB32, rEidB = d2["secret"].(string), d2["enrollment_id"].(string)
	seedR, _ := decodeB32(rSeedB32)
	S.bytes("re-enrolled TOTP seed", seedR)
	S.str("re-enrolment id", rEidB)
	snap("after reenroll begin")
	if code, r := c.do("POST", "/api/v1/actions/auth.totp_reenroll_confirm", map[string]any{"enrollment_id": rEidB, "code": auth.TOTPCode(seedR, auth.TOTPCounter(time.Now()))}, nil, al()); code != 200 {
		t.Fatalf("reenroll confirm: %d %v", code, r)
	}
	// The old session and both tokens are gone.
	if code, _ := newClientWith(newClient, "", tok1).do("POST", "/api/v1/actions/system.info", map[string]string{}, map[string]string{"Authorization": "Bearer " + tok1}, al()); code != 401 {
		t.Errorf("a token survived the re-enrolment: %d", code)
	}

	// ---- sign in with the NEW seed, change the password, sign in with a recovery code ----
	var pend2, sess3, csrf3 string
	login("admin", P1, &pend2)
	code, r3 := c.do("POST", "/api/v1/auth/2fa", map[string]string{"code": auth.TOTPCode(seedR, auth.TOTPCounter(time.Now())+1)}, nil, al(&sess3, &csrf3))
	if code != 200 {
		t.Fatalf("2fa after re-enrolment: %d %v", code, r3)
	}
	sess3, csrf3 = c.cookies[api_sessionCookie], c.csrf
	S.str("session id", sess3)
	S.str("CSRF token", csrf3)
	if code, r := c.do("POST", "/api/v1/actions/auth.password_change", map[string]any{"current_password": P1, "new_password": P2}, nil, al()); code != 200 {
		t.Fatalf("password change: %d %v", code, r)
	}
	snap("after password change")
	var pend3, sess4, csrf4 string
	login("admin", P2, &pend3)
	code, r4 := c.do("POST", "/api/v1/auth/2fa", map[string]string{"recovery_code": rec2[0]}, nil, al(&sess4, &csrf4))
	if code != 200 {
		t.Fatalf("2fa with a recovery code: %d %v", code, r4)
	}
	sess4, csrf4 = c.cookies[api_sessionCookie], c.csrf
	S.str("session id", sess4)
	S.str("CSRF token", csrf4)
	fail()
	// A wrong TOTP and a reused recovery code.
	d := newClient()
	var pend4 string
	d.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": P2}, nil, al(&pend4))
	pend4 = d.cookies["__Host-sd_pending"]
	S.str("pending id", pend4)
	d.do("POST", "/api/v1/auth/2fa", map[string]string{"code": "000000"}, nil, al())
	d.do("POST", "/api/v1/auth/2fa", map[string]string{"recovery_code": rec2[0]}, nil, al())
	// The API description and the session page.
	var openapi []byte
	{
		req, _ := http.NewRequest("GET", "https://"+httpsAddr+"/api/v1/openapi.json", nil)
		req.Header.Set("Cookie", api_sessionCookie+"="+sess4)
		sentHeaders = append(sentHeaders, api_sessionCookie+"="+sess4)
		resp, err := c.hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		openapi, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || len(openapi) < 1000 {
			t.Fatalf("openapi: %d (%d bytes)", resp.StatusCode, len(openapi))
		}
	}
	snap("end of flows")

	// The headers that were sent are sentinels too (the whole Cookie and Authorization values).
	for _, h := range sentHeaders {
		S.str("a sent Cookie/Authorization header", h)
	}
	// The master key and the panel's private key.
	key, err := os.ReadFile(filepath.Join(data, "keys", "master.key"))
	if err != nil || len(key) != 32 {
		t.Fatalf("master key: %v", err)
	}
	S.bytes("master key", key)
	pemB, err := os.ReadFile(filepath.Join(data, "pki", "panel.pem"))
	if err != nil {
		t.Fatal(err)
	}
	for rest := pemB; ; {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type == "EC PRIVATE KEY" {
			S.bytes("panel private key", blk.Bytes)
			lines := strings.Split(strings.TrimSpace(string(pem.EncodeToMemory(blk))), "\n")
			for _, ln := range lines[1 : len(lines)-1] { // each wrapped PEM line
				S.str("panel private key (PEM line)", ln)
			}
		}
	}

	// ---- stop the server, then the console reset ----
	if code := p.Stop(); code != 0 {
		t.Fatalf("server exit %d", code)
	}
	logs := p.Logs()
	res := testbin.Run(t, testbin.Options{Args: []string{"admin", "reset", "--yes", "--data", data}})
	if res.Code != 0 {
		t.Fatalf("admin reset: %d\n%s%s", res.Code, res.Stdout, res.Stderr)
	}
	snap("after admin reset")

	// ---- the scan ----
	fails := 0
	report := func(where string, hits []string) {
		if len(hits) > 0 {
			fails++
			t.Errorf("SECRET LEAK in %s: %s", where, strings.Join(hits, ", "))
		}
	}
	// 1. Every response: nothing but what was meant for its recipient.
	for _, r := range records {
		var allowed []string
		if r.allow != nil {
			allowed = r.allow()
		}
		report("response "+r.what, S.find(r.text, allowed))
	}
	report("the OpenAPI document", S.find(openapi, nil))
	// 2. Server and Caddy logs (DEBUG) and the reset's output.
	report("the server and Caddy logs at DEBUG", S.find([]byte(logs), nil))
	report("the admin reset output", S.find([]byte(res.Stdout+res.Stderr), nil))
	if !strings.Contains(logs, `"level":"debug"`) {
		t.Error("Caddy did not log at DEBUG: the test would prove nothing about its logs")
	}
	// 3. The database and its WAL, at every snapshot, and the final files.
	for label, files := range snaps {
		for name, b := range files {
			report("snapshot "+label+" "+name, S.find(b, nil))
		}
	}
	if wal := snaps["after tokens"]["serverdock.db-wal"]; len(wal) == 0 {
		t.Error("the mid-flow WAL snapshot is empty: the WAL check would be vacuous")
	}
	// 4. Every file under DATA that is not a secret path.
	secret := datadir.SecretPaths()
	scanned := 0
	filepath.WalkDir(data, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(data, path)
		for _, sp := range secret {
			if rel == sp || strings.HasSuffix(sp, "/") && strings.HasPrefix(rel, sp) {
				return nil
			}
		}
		b, _ := os.ReadFile(path)
		scanned++
		report("DATA/"+rel, S.find(b, nil))
		return nil
	})
	if scanned < 2 {
		t.Errorf("only %d DATA files scanned", scanned)
	}
	// 5. The audit log: every column of every row.
	ro, err := store.OpenReadOnly(data, store.ReadOnlyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	rows, err := ro.DB.Query(`SELECT action || '|' || actor_kind || '|' || actor_id || '|' || target || '|' || args_json || '|' || error || '|' || request_id FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	var auditText strings.Builder
	n := 0
	for rows.Next() {
		var s string
		rows.Scan(&s)
		auditText.WriteString(s + "\n")
		n++
	}
	rows.Close()
	if n < 20 {
		t.Errorf("only %d audit rows: the flows did not run", n)
	}
	report("the audit log", S.find([]byte(auditText.String()), nil))

	// ---- the sentinels did flow where they were meant to ----
	meant := map[string][]string{"enrol": {seed2B32, eid2}, "token": {tok1}, "recovery codes": {rec1[0], rec2[0]}, "session": {sess1, sess4}}
	var all strings.Builder
	for _, r := range records {
		all.Write(r.text)
	}
	for what, vals := range meant {
		for _, v := range vals {
			if v == "" || !strings.Contains(all.String(), v) {
				t.Errorf("the %s sentinel never appeared in its own response: the flow did not exercise it", what)
			}
		}
	}
	t.Logf("scanned %d responses, %d snapshots, %d patterns, %d audit rows; %d leaks", len(records), len(snaps), len(S.pats), n, fails)
}

func addRecovery(S *sentinels, display string) {
	S.str("recovery code", display)
	norm := strings.ToUpper(strings.ReplaceAll(display, "-", ""))
	S.str("recovery code (normalised)", norm)
}

func newClientWith(mk func() *sclient, session, token string) *sclient {
	c := mk()
	if session != "" {
		c.cookies[api_sessionCookie] = session
	}
	return c
}

// waitForCounter sleeps until the TOTP time step is at least c.
func waitForCounter(c int64) {
	for auth.TOTPCounter(time.Now()) < c {
		time.Sleep(200 * time.Millisecond)
	}
}
