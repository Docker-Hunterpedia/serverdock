package pki

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func panelOpts(t *testing.T, dir string, clock *time.Time, logs *logBuf, hosts ...string) PanelOptions {
	t.Helper()
	return PanelOptions{
		Dir: dir, Hosts: hosts,
		Now: func() time.Time { return *clock },
		InterfaceAddrs: func() ([]net.Addr, error) {
			return []net.Addr{
				&net.IPNet{IP: net.ParseIP("203.0.113.9"), Mask: net.CIDRMask(24, 32)},
				&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)}, // link-local: not included
				&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
			}, nil
		},
		Log: slog.New(slog.NewTextHandler(logs, nil)),
	}
}

func readLeaf(t *testing.T, dir string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, PanelFile))
	if err != nil {
		t.Fatal(err)
	}
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			t.Fatal("no certificate in panel.pem")
		}
		if blk.Type == "CERTIFICATE" {
			c, err := x509.ParseCertificate(blk.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
}

func TestPanelLeafNotFromHubCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	m, err := Open(Options{Dir: dir, SANs: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var logs logBuf
	p, err := EnsurePanelLeaf(panelOpts(t, dir, &now, &logs))
	if err != nil {
		t.Fatal(err)
	}
	c := readLeaf(t, dir)
	if c.IsCA || !c.BasicConstraintsValid {
		t.Error("the panel leaf must be CA:false")
	}
	if string(c.RawIssuer) != string(c.RawSubject) || c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature) != nil {
		t.Error("not self-signed")
	}
	if c.CheckSignatureFrom(m.ca) == nil || len(c.URIs) != 0 {
		t.Error("the panel leaf chains to the hub CA or carries the hub role URI")
	}
	if len(c.ExtKeyUsage) != 1 || c.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("EKU %v", c.ExtKeyUsage)
	}
	if c.PublicKeyAlgorithm != x509.ECDSA {
		t.Error("not ECDSA")
	}
	if got := c.NotAfter.Sub(c.NotBefore); got > 825*24*time.Hour {
		t.Errorf("validity %v exceeds 825 days", got)
	}
	// SANs: localhost, loopback, a global-unicast interface address; not link-local.
	if len(c.DNSNames) != 1 || c.DNSNames[0] != "localhost" {
		t.Errorf("DNS %v", c.DNSNames)
	}
	var ips []string
	for _, ip := range c.IPAddresses {
		ips = append(ips, ip.String())
	}
	if strings.Join(ips, " ") != "127.0.0.1 ::1 203.0.113.9" {
		t.Errorf("IPs %v", ips)
	}
	if !regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`).MatchString(p.Fingerprint) || p.Fingerprint != PanelFingerprint(c.Raw) {
		t.Errorf("fingerprint %q", p.Fingerprint)
	}
	// The hub CA's key file was never needed: the panel works without one.
	dir2 := filepath.Join(t.TempDir(), "pki")
	if _, err := EnsurePanelLeaf(panelOpts(t, dir2, &now, &logs)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir2, "ca.key")); err == nil {
		t.Error("the panel path created a CA")
	}
}

func TestPanelLeafPathAndModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	now := time.Now()
	var logs logBuf
	p, err := EnsurePanelLeaf(panelOpts(t, dir, &now, &logs))
	if err != nil {
		t.Fatal(err)
	}
	if p.CertFile != filepath.Join(dir, "panel.pem") || !p.Regenerated {
		t.Fatalf("%+v", p)
	}
	for name, mode := range map[string]os.FileMode{"": 0o700, "panel.pem": 0o600, "panel.fingerprint": 0o600} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || st.Mode().Perm() != mode {
			t.Errorf("%q: %v %v", name, st, err)
		}
	}
	fp, _ := os.ReadFile(filepath.Join(dir, PanelFingerprintFile))
	if strings.TrimSpace(string(fp)) != p.Fingerprint {
		t.Error("fingerprint file differs")
	}
	// Unchanged on a second start; no warning on first creation.
	p2, err := EnsurePanelLeaf(panelOpts(t, dir, &now, &logs))
	if err != nil || p2.Regenerated || p2.Fingerprint != p.Fingerprint {
		t.Fatalf("%+v %v", p2, err)
	}
	if logs.String() != "" {
		t.Errorf("unexpected log: %s", logs.String())
	}
}

func TestPanelLeafRegeneratedWithWarning(t *testing.T) {
	now := time.Now()
	cases := map[string]func(dir string){
		"missing":   func(dir string) { os.Remove(filepath.Join(dir, PanelFile)) },
		"garbage":   func(dir string) { os.WriteFile(filepath.Join(dir, PanelFile), []byte("junk"), 0o600) },
		"empty":     func(dir string) { os.WriteFile(filepath.Join(dir, PanelFile), nil, 0o600) },
		"key only":  func(dir string) { keepBlocks(t, dir, "EC PRIVATE KEY") },
		"cert only": func(dir string) { keepBlocks(t, dir, "CERTIFICATE") },
		"mismatch": func(dir string) { // another leaf's certificate with this key
			other := filepath.Join(t.TempDir(), "pki")
			var l logBuf
			EnsurePanelLeaf(panelOpts(t, other, &now, &l))
			a, _ := os.ReadFile(filepath.Join(dir, PanelFile))
			b, _ := os.ReadFile(filepath.Join(other, PanelFile))
			ka, _ := pem.Decode(a)
			var cb []byte
			for rest := b; ; {
				var blk *pem.Block
				blk, rest = pem.Decode(rest)
				if blk == nil {
					break
				}
				if blk.Type == "CERTIFICATE" {
					cb = pem.EncodeToMemory(blk)
				}
			}
			os.WriteFile(filepath.Join(dir, PanelFile), append(pem.EncodeToMemory(ka), cb...), 0o600)
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "pki")
			var logs logBuf
			p1, err := EnsurePanelLeaf(panelOpts(t, dir, &now, &logs))
			if err != nil {
				t.Fatal(err)
			}
			damage(dir)
			p2, err := EnsurePanelLeaf(panelOpts(t, dir, &now, &logs))
			if err != nil || !p2.Regenerated || p2.Fingerprint == p1.Fingerprint {
				t.Fatalf("%+v %v", p2, err)
			}
			if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, p2.Fingerprint) {
				t.Errorf("no WARN with the new fingerprint: %q", out)
			}
			if fp, _ := os.ReadFile(filepath.Join(dir, PanelFingerprintFile)); strings.TrimSpace(string(fp)) != p2.Fingerprint {
				t.Error("fingerprint file not updated")
			}
		})
	}
}

func keepBlocks(t *testing.T, dir, typ string) {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(dir, PanelFile))
	var out []byte
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if blk.Type == typ {
			out = append(out, pem.EncodeToMemory(blk)...)
		}
	}
	os.WriteFile(filepath.Join(dir, PanelFile), out, 0o600)
}

func TestPanelLeafRenewedUnder60Days(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	start := time.Now()
	now := start
	var logs logBuf
	p1, err := EnsurePanelLeaf(panelOpts(t, dir, &now, &logs))
	if err != nil {
		t.Fatal(err)
	}
	// 61 days left: kept.
	now = p1.NotAfter.Add(-61 * 24 * time.Hour)
	if p, err := EnsurePanelLeaf(panelOpts(t, dir, &now, &logs)); err != nil || p.Regenerated {
		t.Fatalf("regenerated with 61 days left: %+v %v", p, err)
	}
	// 59 days left: regenerated, with a WARN.
	now = p1.NotAfter.Add(-59 * 24 * time.Hour)
	p2, err := EnsurePanelLeaf(panelOpts(t, dir, &now, &logs))
	if err != nil || !p2.Regenerated || p2.Fingerprint == p1.Fingerprint {
		t.Fatalf("%+v %v", p2, err)
	}
	if !strings.Contains(logs.String(), "fewer than 60 days") || !strings.Contains(logs.String(), p2.Fingerprint) {
		t.Errorf("log: %s", logs.String())
	}
	if p2.NotAfter.Sub(now) < 800*24*time.Hour {
		t.Errorf("the new leaf is valid until %v", p2.NotAfter)
	}
	// An already expired leaf is replaced too.
	now = p2.NotAfter.Add(time.Hour)
	if p3, err := EnsurePanelLeaf(panelOpts(t, dir, &now, &logs)); err != nil || !p3.Regenerated {
		t.Fatalf("%+v %v", p3, err)
	}
}

func TestPanelLeafRegeneratedWhenPanelHostsChange(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	now := time.Now()
	var logs logBuf
	p1, _ := EnsurePanelLeaf(panelOpts(t, dir, &now, &logs, "panel.example.com", "198.51.100.4"))
	c := readLeaf(t, dir)
	if !contains(c.DNSNames, "panel.example.com") {
		t.Fatalf("DNS %v", c.DNSNames)
	}
	hasIP := false
	for _, ip := range c.IPAddresses {
		hasIP = hasIP || ip.String() == "198.51.100.4"
	}
	if !hasIP {
		t.Fatal("--panel-host IP missing")
	}
	// Same set, any order and case: kept.
	if p, _ := EnsurePanelLeaf(panelOpts(t, dir, &now, &logs, "198.51.100.4", "Panel.Example.com")); p.Regenerated {
		t.Fatal("regenerated for an identical set")
	}
	for _, hosts := range [][]string{{"panel.example.com"}, {"panel.example.com", "198.51.100.4", "other.example"}, nil} {
		p, err := EnsurePanelLeaf(panelOpts(t, dir, &now, &logs, hosts...))
		if err != nil || !p.Regenerated || p.Fingerprint == p1.Fingerprint {
			t.Fatalf("hosts %v: %+v %v", hosts, p, err)
		}
		p1 = p
	}
	if !strings.Contains(logs.String(), "--panel-host set changed") {
		t.Errorf("log: %s", logs.String())
	}
	if _, err := EnsurePanelLeaf(panelOpts(t, dir, &now, &logs, "bad name")); err == nil {
		t.Error("an invalid --panel-host was accepted")
	}
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}
