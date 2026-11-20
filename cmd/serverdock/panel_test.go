package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/pki"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func panelServer(t *testing.T, data string) (serverAddrs, func() error, *lockedBuf) {
	t.Helper()
	var logs lockedBuf
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan serverAddrs, 1)
	done := make(chan error, 1)
	cfg := serverConfig{Data: data, ControlSocket: shortSock(t), Listen: testbinFree(t), HTTPSListen: testbinFree(t), Proxy: true}
	go func() {
		done <- runServer(ctx, cfg, slog.New(slog.NewTextHandler(&logs, nil)), func(a serverAddrs) { ready <- a })
	}()
	select {
	case a := <-ready:
		return a, func() error { cancel(); return <-done }, &logs
	case err := <-done:
		t.Fatalf("server exited early: %v", err)
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("not ready")
	}
	return serverAddrs{}, nil, nil
}

func panelFingerprintFile(t *testing.T, data string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(data, "pki", "panel.fingerprint"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// handshake returns the fingerprint of the leaf the server presents.
func handshake(t *testing.T, addr, serverName string) string {
	t.Helper()
	var conn *tls.Conn
	var err error
	for i := 0; i < 50; i++ { // Caddy may need a moment after Load
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", addr,
			&tls.Config{InsecureSkipVerify: true, ServerName: serverName})
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("TLS to %s with SNI %q: %v", addr, serverName, err)
	}
	defer conn.Close()
	return pki.PanelFingerprint(conn.ConnectionState().PeerCertificates[0].Raw)
}

// certmagic picks a certificate by SNI. Browsers send none for an IP literal and
// any name for a hostname, so default_sni and fallback_sni (both "localhost")
// must make every handshake get the one panel leaf.
func TestPanelSNIHandshakes(t *testing.T) {
	data := newData(t)
	// Bind all addresses so a loopback address that is NOT in the leaf's SANs
	// (127.0.0.2, where the OS routes it) reaches the same listener.
	cfg := serverConfig{Data: data, ControlSocket: shortSock(t), Listen: testbinFree(t), Proxy: true}
	_, port, _ := net.SplitHostPort(testbinFree(t))
	cfg.HTTPSListen = "0.0.0.0:" + port
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan serverAddrs, 1)
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, cfg, quietLog(), func(a serverAddrs) { ready <- a }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(15 * time.Second):
		t.Fatal("not ready")
	}
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	want := panelFingerprintFile(t, data)
	addr := "127.0.0.1:" + port
	if got := handshake(t, addr, ""); got != want { // an IP literal: no SNI at all
		t.Errorf("no SNI: got %s, want the panel leaf %s", got, want)
	}
	if got := handshake(t, addr, "localhost"); got != want {
		t.Errorf("SNI localhost: got %s, want %s", got, want)
	}
	if got := handshake(t, addr, "example.invalid"); got != want {
		t.Errorf("SNI example.invalid: got %s, want %s", got, want)
	}
	// An address that is not in the leaf's SANs, with no SNI.
	if c, err := net.DialTimeout("tcp", "127.0.0.2:"+port, time.Second); err == nil {
		c.Close()
		if got := handshake(t, "127.0.0.2:"+port, ""); got != want {
			t.Errorf("no SNI to an address outside the SANs: got %s, want %s", got, want)
		}
	} else {
		t.Logf("127.0.0.2 is not routable here; skipped the outside-the-SANs address")
	}
}

func TestPanelServedOnHTTPSAndHTTPOnlyRedirects(t *testing.T) {
	data := newData(t)
	addrs, stop, _ := panelServer(t, data)
	c := insecureClient()
	defer c.CloseIdleConnections()
	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ {
		if resp, err = c.Get("https://" + addrs.HTTPS + "/healthz"); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("https healthz: %v %v", resp, err)
	}
	if got := pki.PanelFingerprint(resp.TLS.PeerCertificates[0].Raw); got != panelFingerprintFile(t, data) {
		t.Errorf("served leaf %s != panel.fingerprint", got)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"ok"`) {
		t.Errorf("body %q", body)
	}

	// :80 answers 308 for every method and path and never reaches the control
	// plane. The port in Location is the one of --https-listen.
	_, httpsPort, _ := net.SplitHostPort(addrs.HTTPS)
	for _, m := range []string{"GET", "POST", "HEAD"} {
		for _, p := range []string{"/", "/healthz", "/api/v1/x?a=b", "/agent/connect"} {
			req, _ := http.NewRequest(m, "http://"+addrs.Listen+p, strings.NewReader("x"))
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			host, _, _ := net.SplitHostPort(addrs.Listen)
			if want := "https://" + host + ":" + httpsPort + p; resp.StatusCode != 308 || resp.Header.Get("Location") != want {
				t.Errorf("%s %s: %d Location %q, want 308 %q", m, p, resp.StatusCode, resp.Header.Get("Location"), want)
			}
		}
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestPanelFingerprintLoggedEveryStart(t *testing.T) {
	data := newData(t)
	var seen []string
	for i := 0; i < 2; i++ {
		_, stop, logs := panelServer(t, data)
		if err := stop(); err != nil {
			t.Fatal(err)
		}
		fp := panelFingerprintFile(t, data)
		if !strings.Contains(logs.String(), "panel certificate") || !strings.Contains(logs.String(), fp) {
			t.Fatalf("start %d did not log the panel fingerprint %s:\n%s", i, fp, logs.String())
		}
		if strings.Contains(logs.String(), "level=WARN") && strings.Contains(logs.String(), "regenerated") {
			t.Errorf("start %d regenerated the leaf:\n%s", i, logs.String())
		}
		seen = append(seen, fp)
	}
	if seen[0] != seen[1] {
		t.Error("the leaf changed between restarts")
	}
}

func TestNoPanelLeafWithoutProxy(t *testing.T) {
	data := newData(t)
	_, stop := startServer(t, serverConfig{Data: data, ControlSocket: shortSock(t)})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "pki", "panel.pem")); err == nil {
		t.Error("--proxy=false created a panel leaf")
	}
}

func TestPanelFlags(t *testing.T) {
	var buf bytes.Buffer
	c, err := parseServerFlags(nil, &buf)
	if err != nil || c.HTTPSListen != ":443" || len(c.PanelHosts) != 0 {
		t.Fatalf("%+v %v", c, err)
	}
	c, err = parseServerFlags([]string{"--https-listen", ":8443", "--panel-host", "a.example", "--panel-host", "203.0.113.4"}, &buf)
	if err != nil || c.HTTPSListen != ":8443" || len(c.PanelHosts) != 2 {
		t.Fatalf("%+v %v", c, err)
	}
	if err := (serverConfig{Listen: ":80", HTTPSListen: ":80", Proxy: true}).validate(); err == nil {
		t.Error("the same HTTP and HTTPS address was accepted")
	}
	if err := (serverConfig{HTTPSListen: "nope"}).validate(); err == nil {
		t.Error("a bad --https-listen was accepted")
	}
	quiet := quietLog()
	if err := runServer(context.Background(), serverConfig{Data: newData(t), ControlSocket: shortSock(t), Proxy: true,
		Listen: "127.0.0.1:0", HTTPSListen: "127.0.0.1:1", PanelHosts: []string{"bad name"}}, quiet, nil); err == nil {
		t.Error("a bad --panel-host was accepted")
	}
}
