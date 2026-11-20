package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)} }

func open(t *testing.T, dir string, clk *clock, sans ...string) *Manager {
	t.Helper()
	m, err := Open(Options{Dir: dir, SANs: sans, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func leafOf(t *testing.T, m *Manager) *x509.Certificate {
	t.Helper()
	c, err := m.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.Leaf
}

func TestCAShapeAndFileModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	clk := newClock()
	m := open(t, dir, clk, "hub.example", "203.0.113.7")
	if m.ca.MaxPathLen != 0 || !m.ca.MaxPathLenZero || !m.ca.IsCA {
		t.Errorf("CA path length: len=%d zero=%v isCA=%v", m.ca.MaxPathLen, m.ca.MaxPathLenZero, m.ca.IsCA)
	}
	if m.ca.NotAfter.Sub(m.ca.NotBefore) < 9*365*24*time.Hour {
		t.Error("CA lifetime too short")
	}
	if _, ok := m.ca.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Error("CA is not ECDSA")
	}
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("pki dir mode %v %v", st.Mode(), err)
	}
	for name, want := range map[string]os.FileMode{"ca.key": 0o600, "hub.pem": 0o600, "ca.crt": 0o644, "ca.fingerprint": 0o644} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || st.Mode().Perm() != want {
			t.Errorf("%s mode %v %v, want %v", name, st.Mode().Perm(), err, want)
		}
	}
	fp, _ := os.ReadFile(filepath.Join(dir, "ca.fingerprint"))
	if strings.TrimSpace(string(fp)) != m.CAFingerprint() || !strings.HasPrefix(m.CAFingerprint(), "sha256:") {
		t.Errorf("fingerprint file %q vs %q", fp, m.CAFingerprint())
	}
	leaf := leafOf(t, m)
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "hub.example" || len(leaf.IPAddresses) != 1 {
		t.Errorf("SANs %v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("EKU %v", leaf.ExtKeyUsage)
	}
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != RoleHub {
		t.Errorf("URIs %v", leaf.URIs)
	}
	if leaf.IsCA {
		t.Error("leaf is a CA")
	}
	// The CA cannot mint an intermediate CA that verifies.
	subKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	subDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: newSerialMust(), Subject: pkix.Name{CommonName: "sub"},
		NotBefore: clk.Now(), NotAfter: clk.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}, m.ca, &subKey.PublicKey, m.caKey)
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := x509.ParseCertificate(subDER)
	under, _ := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: newSerialMust(), Subject: pkix.Name{CommonName: "x"}, DNSNames: []string{"x.example"},
		NotBefore: clk.Now(), NotAfter: clk.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, sub, &subKey.PublicKey, subKey)
	underC, _ := x509.ParseCertificate(under)
	roots, inter := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(m.ca)
	inter.AddCert(sub)
	if _, err := underC.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, DNSName: "x.example", CurrentTime: clk.Now()}); err == nil {
		t.Error("a leaf under a sub-CA verified: MaxPathLenZero is not effective")
	}
}

func newSerialMust() *big.Int { s, _ := newSerial(); return s }

func TestReopenKeepsCAAndLeaf(t *testing.T) {
	dir := t.TempDir()
	clk := newClock()
	m1 := open(t, dir, clk, "hub.example")
	m2 := open(t, dir, clk, "hub.example")
	if m1.CAFingerprint() != m2.CAFingerprint() {
		t.Error("CA changed across restarts")
	}
	if m2.Issued() != 0 {
		t.Errorf("leaf re-issued on a plain restart (%d)", m2.Issued())
	}
	if leafOf(t, m1).SerialNumber.Cmp(leafOf(t, m2).SerialNumber) != 0 {
		t.Error("leaf changed across restarts")
	}
}

func TestHalfACAIsRefusedNotReplaced(t *testing.T) {
	dir := t.TempDir()
	clk := newClock()
	open(t, dir, clk, "hub.example")
	if err := os.Remove(filepath.Join(dir, "ca.crt")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Dir: dir, SANs: []string{"hub.example"}, Now: clk.Now}); err == nil {
		t.Fatal("a half-written CA was replaced")
	}
}

func TestOpenValidation(t *testing.T) {
	clk := newClock()
	for _, sans := range [][]string{nil, {""}, {"bad name"}, {"-x.example"}, {"a..b"}} {
		if _, err := Open(Options{Dir: t.TempDir(), SANs: sans, Now: clk.Now}); err == nil {
			t.Errorf("SANs %q accepted", sans)
		}
	}
	if _, err := Open(Options{SANs: []string{"a"}}); err == nil {
		t.Error("empty dir accepted")
	}
}

func TestLazyRenewalWindow(t *testing.T) {
	clk := newClock()
	m := open(t, t.TempDir(), clk, "hub.example")
	first := leafOf(t, m).SerialNumber
	// 31 days before expiry: no re-issue.
	clk.Add(leafLifetime - 31*24*time.Hour)
	if leafOf(t, m).SerialNumber.Cmp(first) != 0 || m.Issued() != 1 {
		t.Fatalf("re-issued at 31 days left (issued=%d)", m.Issued())
	}
	// 29 days before expiry: re-issue.
	clk.Add(2 * 24 * time.Hour)
	if leafOf(t, m).SerialNumber.Cmp(first) == 0 || m.Issued() != 2 {
		t.Fatalf("not re-issued at 29 days left (issued=%d)", m.Issued())
	}
	// A restart picks up the renewed leaf from disk.
	m2 := open(t, filepath.Dir(filepath.Join(m.dir, "x")), clk, "hub.example")
	if m2.Issued() != 0 {
		t.Error("restart re-issued a fresh leaf")
	}
}

func TestConcurrentHandshakesIssueOnce(t *testing.T) {
	clk := newClock()
	m := open(t, t.TempDir(), clk, "hub.example")
	clk.Add(leafLifetime - 10*24*time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "whatever"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if m.Issued() != 2 {
		t.Errorf("issued %d times, want 2 (initial + one renewal)", m.Issued())
	}
}

func TestSANChangeReissues(t *testing.T) {
	dir := t.TempDir()
	clk := newClock()
	m1 := open(t, dir, clk, "hub.example")
	s1 := leafOf(t, m1).SerialNumber
	m2 := open(t, dir, clk, "hub.example", "203.0.113.9")
	if m2.Issued() != 1 || leafOf(t, m2).SerialNumber.Cmp(s1) == 0 {
		t.Error("SAN added: no re-issue")
	}
	// Same set in another order and case: no re-issue.
	m3 := open(t, dir, clk, "203.0.113.9", "HUB.example")
	if m3.Issued() != 0 {
		t.Error("reordered SANs caused a re-issue")
	}
	// And a change while running (flags cannot change live, but the check is per handshake).
	m3.sans, _ = parseSANs([]string{"other.example"})
	if c, err := m3.GetCertificate(nil); err != nil || c.Leaf.DNSNames[0] != "other.example" {
		t.Errorf("per-handshake SAN check failed: %v", err)
	}
}

func TestSNIIsIgnored(t *testing.T) {
	m := open(t, t.TempDir(), newClock(), "hub.example")
	for _, sni := range []string{"", "hub.example", "evil.example", strings.Repeat("a", 300)} {
		c, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: sni})
		if err != nil || c.Leaf.DNSNames[0] != "hub.example" {
			t.Errorf("SNI %q: %v", sni, err)
		}
	}
	if m.Issued() != 1 {
		t.Errorf("SNI caused issues: %d", m.Issued())
	}
}

// breakDir makes the pki dir unwritable by replacing it with a read-only one.
func breakWrites(t *testing.T, m *Manager) func() {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if err := os.Chmod(m.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	return func() { os.Chmod(m.dir, 0o700) }
}

func TestFailedWriteKeepsServingCurrentLeafWithBackoff(t *testing.T) {
	clk := newClock()
	m := open(t, t.TempDir(), clk, "hub.example")
	cur := leafOf(t, m).SerialNumber
	base := m.attempts()
	defer breakWrites(t, m)()
	clk.Add(leafLifetime - 5*24*time.Hour) // inside the renewal window, leaf still valid
	for i := 0; i < 20; i++ {
		c, err := m.GetCertificate(nil)
		if err != nil {
			t.Fatalf("handshake %d failed: %v", i, err)
		}
		if c.Leaf.SerialNumber.Cmp(cur) != 0 {
			t.Fatal("served an unwritten leaf")
		}
		clk.Add(time.Second) // a burst well inside the 1 minute back-off
	}
	if got := m.attempts() - base; got != 1 {
		t.Errorf("re-issue attempts in the burst = %d, want exactly 1", got)
	}
	// After the back-off, exactly one more attempt.
	clk.Add(RetryBackoff)
	if _, err := m.GetCertificate(nil); err != nil {
		t.Fatal(err)
	}
	if got := m.attempts() - base; got != 2 {
		t.Errorf("attempts after back-off = %d, want 2", got)
	}
}

func TestExpiredLeafWithFailingReissueFailsHandshake(t *testing.T) {
	clk := newClock()
	m := open(t, t.TempDir(), clk, "hub.example")
	defer breakWrites(t, m)()
	clk.Add(leafLifetime + time.Hour)
	if _, err := m.GetCertificate(nil); err == nil {
		t.Fatal("handshake succeeded with an expired leaf")
	}
}

func TestExpiredLeafIsReissuedWhenWritable(t *testing.T) {
	clk := newClock()
	m := open(t, t.TempDir(), clk, "hub.example")
	clk.Add(leafLifetime + time.Hour)
	c, err := m.GetCertificate(nil)
	if err != nil || !valid(c.Leaf, clk.Now()) {
		t.Fatalf("expired leaf not replaced: %v", err)
	}
}

// ---- agent-side verification over a real TLS handshake ----

func handshake(t *testing.T, srv *tls.Config, cli *tls.Config) error {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srv)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		done <- c.(*tls.Conn).Handshake()
	}()
	c, err := tls.Dial("tcp", ln.Addr().String(), cli)
	if err == nil {
		c.Close()
	}
	<-done
	return err
}

func TestAgentVerification(t *testing.T) {
	clk := newClock()
	m := open(t, t.TempDir(), clk, "hub.example", "127.0.0.1")
	good, err := ClientTLSConfig("hub.example", m.CAFingerprint(), clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := handshake(t, m.TLSConfig(), good); err != nil {
		t.Fatalf("good handshake: %v", err)
	}
	// IP host matched against IP SANs.
	ipCfg, _ := ClientTLSConfig("127.0.0.1:7443", m.CAFingerprint(), clk.Now)
	if err := handshake(t, m.TLSConfig(), ipCfg); err != nil {
		t.Errorf("IP SAN handshake: %v", err)
	}
	if good.MinVersion != tls.VersionTLS13 {
		t.Error("client allows TLS < 1.3")
	}

	// SAN mismatch.
	bad, _ := ClientTLSConfig("other.example", m.CAFingerprint(), clk.Now)
	if err := handshake(t, m.TLSConfig(), bad); err == nil {
		t.Error("SAN mismatch accepted")
	}
	// Wrong fingerprint (the pinned CA is not in the chain).
	other := open(t, t.TempDir(), clk, "hub.example")
	wrongFP, _ := ClientTLSConfig("hub.example", other.CAFingerprint(), clk.Now)
	if err := handshake(t, m.TLSConfig(), wrongFP); err == nil {
		t.Error("wrong fingerprint accepted")
	}
	// Another CA presenting a valid-looking hub leaf, with the legitimate fingerprint pinned.
	if err := handshake(t, other.TLSConfig(), good); err == nil {
		t.Error("another CA's hub leaf accepted")
	}
	// Expired leaf (client clock far in the future).
	late, _ := ClientTLSConfig("hub.example", m.CAFingerprint(), func() time.Time { return clk.Now().Add(2 * leafLifetime) })
	if err := handshake(t, m.TLSConfig(), late); err == nil {
		t.Error("expired leaf accepted")
	}
	// Malformed fingerprints.
	for _, fp := range []string{"", "sha256:", "sha256:zz", "md5:00", strings.Repeat("a", 64), "sha256:" + strings.Repeat("a", 63)} {
		if _, err := ClientTLSConfig("hub.example", fp, nil); err == nil {
			t.Errorf("fingerprint %q accepted", fp)
		}
	}
}

// A leaf from the SAME CA without the role marker (for example a future agent
// client certificate) must not be accepted as the hub.
func TestSameCALeafWithoutRoleMarkerRefused(t *testing.T) {
	clk := newClock()
	m := open(t, t.TempDir(), clk, "hub.example")
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: newSerialMust(), Subject: pkix.Name{CommonName: "imposter"},
		DNSNames:  []string{"hub.example"},
		NotBefore: clk.Now().Add(-time.Minute), NotAfter: clk.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, m.ca, &key.PublicKey, m.caKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{der, m.ca.Raw}, PrivateKey: key}}}
	cli, _ := ClientTLSConfig("hub.example", m.CAFingerprint(), clk.Now)
	err = handshake(t, srv, cli)
	if err == nil || !strings.Contains(err.Error(), "role marker") {
		t.Errorf("same-CA leaf without role marker: %v", err)
	}
	// And one with a clientAuth-only EKU is refused for serverAuth.
	der2, _ := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: newSerialMust(), Subject: pkix.Name{CommonName: "client"},
		DNSNames: []string{"hub.example"}, URIs: []*url.URL{{Scheme: "urn", Opaque: "serverdock:role:hub"}},
		NotBefore: clk.Now().Add(-time.Minute), NotAfter: clk.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, m.ca, &key.PublicKey, m.caKey)
	srv2 := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{der2, m.ca.Raw}, PrivateKey: key}}}
	if err := handshake(t, srv2, cli); err == nil {
		t.Error("clientAuth-only leaf accepted as a server")
	}
}

func TestNoCAInChainRefused(t *testing.T) {
	clk := newClock()
	m := open(t, t.TempDir(), clk, "hub.example")
	c, _ := m.GetCertificate(nil)
	srv := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: c.Certificate[:1], PrivateKey: c.PrivateKey}}}
	cli, _ := ClientTLSConfig("hub.example", m.CAFingerprint(), clk.Now)
	if err := handshake(t, srv, cli); err == nil {
		t.Error("leaf-only chain accepted")
	}
}

func TestParseFingerprintRoundTrip(t *testing.T) {
	m := open(t, t.TempDir(), newClock(), "hub.example")
	d, err := ParseFingerprint(m.CAFingerprint())
	if err != nil || d != DigestOf(m.ca.Raw) {
		t.Fatal(err)
	}
	_ = errors.New
	_ = net.IP{}
}
