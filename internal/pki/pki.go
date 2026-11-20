// Package pki is the hub's private certificate authority (ADR 0009).
//
// The CA signs only the hub's agent-listener leaf (and, in Phase 2, agent
// client certificates with the clientAuth EKU only). The M2 panel certificate
// is a separate self-signed leaf and is never issued here. There is no timer
// and no goroutine: the leaf is renewed lazily, inside the TLS handshake.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// RoleHub is the URI SAN every hub leaf carries. Agents require it, so a
// future leaf from this CA (for example an agent client cert) can never be
// accepted as "the hub".
const RoleHub = "urn:serverdock:role:hub"

const (
	caLifetime   = 10 * 365 * 24 * time.Hour
	leafLifetime = 365 * 24 * time.Hour
	// RenewBefore is how long before NotAfter the leaf is re-issued.
	RenewBefore = 30 * 24 * time.Hour
	// RetryBackoff is the minimum gap between failed re-issue attempts.
	RetryBackoff = time.Minute
	clockSkew    = 5 * time.Minute

	caKeyFile = "ca.key"
	caCrtFile = "ca.crt"
	fpFile    = "ca.fingerprint"
	leafFile  = "hub.pem" // leaf key and chain in one file, replaced atomically
)

// Options configures Open.
type Options struct {
	// Dir is DATA/pki; it is created 0700.
	Dir string
	// SANs are the DNS names or IP addresses the hub leaf must carry. Required.
	SANs []string
	// Now is the clock (tests inject one); nil means time.Now.
	Now func() time.Time
	// Log receives renewal failures (once per back-off); nil means a discard logger.
	Log *slog.Logger
}

// Manager owns the CA and the cached hub leaf.
type Manager struct {
	dir  string
	sans sanSet
	now  func() time.Time
	log  *slog.Logger

	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey

	mu        sync.Mutex
	cert      *tls.Certificate // leaf + CA, with Leaf parsed
	nextRetry time.Time
	issued    int // number of leaf issues, for tests
	tried     int // number of re-issue attempts (including failures), for tests
}

// Open loads or creates the CA and the hub leaf under opts.Dir.
func Open(opts Options) (*Manager, error) {
	if opts.Dir == "" {
		return nil, errors.New("pki: Dir is required")
	}
	sans, err := parseSANs(opts.SANs)
	if err != nil {
		return nil, err
	}
	if len(sans.dns)+len(sans.ip) == 0 {
		return nil, errors.New("pki: at least one SAN is required")
	}
	m := &Manager{dir: opts.Dir, sans: sans, now: opts.Now, log: opts.Log}
	if m.now == nil {
		m.now = time.Now
	}
	if m.log == nil {
		m.log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	if err := os.Chmod(m.dir, 0o700); err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	if err := m.loadOrCreateCA(); err != nil {
		return nil, err
	}
	if err := m.loadLeaf(); err != nil {
		m.log.Warn("hub certificate not loaded; issuing a new one", "reason", err.Error())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.needsRenewal(m.now()) {
		if err := m.reissue(m.now()); err != nil {
			return nil, err
		}
	}
	return m, nil
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// CAFingerprint returns "sha256:<hex>" of the CA certificate. It is public.
func (m *Manager) CAFingerprint() string { return Fingerprint(m.ca.Raw) }

// Fingerprint formats the SHA-256 of a DER certificate as "sha256:<hex>".
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// DigestOf returns the SHA-256 of a DER certificate.
func DigestOf(der []byte) [sha256.Size]byte { return sha256.Sum256(der) }

// ParseFingerprint validates "sha256:<64 hex>" and returns the digest.
func ParseFingerprint(s string) ([sha256.Size]byte, error) {
	var out [sha256.Size]byte
	h, ok := strings.CutPrefix(s, "sha256:")
	if !ok || len(h) != 2*sha256.Size {
		return out, errors.New("fingerprint must be sha256:<64 hex digits>")
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return out, errors.New("fingerprint must be sha256:<64 hex digits>")
	}
	copy(out[:], b)
	return out, nil
}

// Issued reports how many hub leaves were issued (tests).
func (m *Manager) Issued() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.issued
}

// attempts reports how many re-issues were attempted, failed or not (tests).
func (m *Manager) attempts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tried
}

// TLSConfig returns the server config: TLS 1.3, certificate chosen lazily.
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: m.GetCertificate}
}

// GetCertificate returns the hub leaf. It ignores SNI (the agent verifies the
// name against the leaf itself). If the leaf has under 30 days left, or the
// configured SAN set changed, it is re-issued first. A failed re-issue keeps
// serving the current leaf while it is still valid and is retried no sooner
// than RetryBackoff later; an expired leaf that cannot be replaced fails the
// handshake.
func (m *Manager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if m.needsRenewal(now) && !now.Before(m.nextRetry) {
		if err := m.reissue(now); err != nil {
			m.nextRetry = now.Add(RetryBackoff)
			m.log.Error("hub certificate re-issue failed; retrying later", "retry_after", RetryBackoff.String(), "err", err.Error())
		}
	}
	if m.cert == nil || !valid(m.cert.Leaf, now) {
		return nil, errors.New("pki: no valid hub certificate")
	}
	return m.cert, nil
}

func valid(c *x509.Certificate, now time.Time) bool {
	return !now.Before(c.NotBefore) && now.Before(c.NotAfter)
}

// needsRenewal: no leaf, under RenewBefore left, or a different SAN set. Caller holds mu.
func (m *Manager) needsRenewal(now time.Time) bool {
	if m.cert == nil {
		return true
	}
	leaf := m.cert.Leaf
	if !now.Add(RenewBefore).Before(leaf.NotAfter) {
		return true
	}
	return !m.sans.equalsCert(leaf)
}

// reissue signs a new leaf and writes it atomically. The in-memory leaf only
// changes when the write succeeded. Caller holds mu.
func (m *Manager) reissue(now time.Time) error {
	m.tried++
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := newSerial()
	if err != nil {
		return err
	}
	role, _ := url.Parse(RoleHub)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "serverdock hub"},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(leafLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              m.sans.dns,
		IPAddresses:           m.sans.ipAddrs(),
		URIs:                  []*url.URL{role},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.ca, &key.PublicKey, m.caKey)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	var pemBuf []byte
	pemBuf = append(pemBuf, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})...)
	pemBuf = append(pemBuf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	if err := writeAtomic(filepath.Join(m.dir, leafFile), pemBuf, 0o600); err != nil {
		return err
	}
	m.cert = &tls.Certificate{Certificate: [][]byte{der, m.ca.Raw}, PrivateKey: key, Leaf: leaf}
	m.nextRetry = time.Time{}
	m.issued++
	return nil
}

// loadLeaf reads hub.pem; any problem leaves m.cert nil so Open re-issues.
func (m *Manager) loadLeaf() error {
	b, err := os.ReadFile(filepath.Join(m.dir, leafFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var keyDER, certDER []byte
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		switch blk.Type {
		case "EC PRIVATE KEY":
			keyDER = blk.Bytes
		case "CERTIFICATE":
			certDER = blk.Bytes
		}
	}
	if keyDER == nil || certDER == nil {
		return errors.New("hub.pem is incomplete")
	}
	key, err := x509.ParseECPrivateKey(keyDER)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return err
	}
	if err := leaf.CheckSignatureFrom(m.ca); err != nil {
		return fmt.Errorf("hub leaf is not signed by this CA: %w", err)
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return errors.New("hub leaf does not match its key")
	}
	m.cert = &tls.Certificate{Certificate: [][]byte{certDER, m.ca.Raw}, PrivateKey: key, Leaf: leaf}
	return nil
}

func (m *Manager) loadOrCreateCA() error {
	keyPath, crtPath := filepath.Join(m.dir, caKeyFile), filepath.Join(m.dir, caCrtFile)
	kb, kerr := os.ReadFile(keyPath)
	cb, cerr := os.ReadFile(crtPath)
	if kerr == nil && cerr == nil {
		kblk, _ := pem.Decode(kb)
		cblk, _ := pem.Decode(cb)
		if kblk == nil || cblk == nil {
			return errors.New("pki: ca.key or ca.crt is not valid PEM; refusing to replace the CA")
		}
		key, err := x509.ParseECPrivateKey(kblk.Bytes)
		if err != nil {
			return fmt.Errorf("pki: ca.key: %w", err)
		}
		crt, err := x509.ParseCertificate(cblk.Bytes)
		if err != nil {
			return fmt.Errorf("pki: ca.crt: %w", err)
		}
		m.ca, m.caKey = crt, key
		return nil
	}
	if kerr == nil || cerr == nil || !(errors.Is(kerr, os.ErrNotExist) && errors.Is(cerr, os.ErrNotExist)) {
		// Half a CA, or an unreadable one: replacing it would silently orphan every pinned agent.
		return fmt.Errorf("pki: CA files are incomplete or unreadable (key: %v, cert: %v); refusing to create a new CA", kerr, cerr)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := newSerial()
	if err != nil {
		return err
	}
	now := m.now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "serverdock hub CA"},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true, // signs leaves only, never another CA
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	crt, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	// Key first: a crash between the two writes leaves "half a CA", which Open
	// refuses rather than replaces.
	if err := writeAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := writeAtomic(crtPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(m.dir, fpFile), []byte(Fingerprint(der)+"\n"), 0o644); err != nil {
		return err
	}
	m.ca, m.caKey = crt, key
	return nil
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
}

// writeAtomic writes path through a temp file, fsync, rename and a directory fsync.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// sanSet is a normalised, sorted SAN list.
type sanSet struct {
	dns []string
	ip  []string
}

func parseSANs(in []string) (sanSet, error) {
	var s sanSet
	seen := map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			return s, errors.New("pki: empty SAN")
		}
		if ip := net.ParseIP(v); ip != nil {
			k := ip.String()
			if !seen[k] {
				seen[k] = true
				s.ip = append(s.ip, k)
			}
			continue
		}
		d := strings.ToLower(strings.TrimSuffix(v, "."))
		if !validDNSName(d) {
			return s, fmt.Errorf("pki: %q is not a DNS name or IP address", v)
		}
		if !seen[d] {
			seen[d] = true
			s.dns = append(s.dns, d)
		}
	}
	sort.Strings(s.dns)
	sort.Strings(s.ip)
	return s, nil
}

func validDNSName(d string) bool {
	if d == "" || len(d) > 253 {
		return false
	}
	for _, label := range strings.Split(d, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func (s sanSet) ipAddrs() []net.IP {
	out := make([]net.IP, len(s.ip))
	for i, v := range s.ip {
		out[i] = net.ParseIP(v)
	}
	return out
}

func (s sanSet) equalsCert(c *x509.Certificate) bool {
	dns := make([]string, len(c.DNSNames))
	for i, d := range c.DNSNames {
		dns[i] = strings.ToLower(d)
	}
	ips := make([]string, len(c.IPAddresses))
	for i, ip := range c.IPAddresses {
		ips[i] = ip.String()
	}
	sort.Strings(dns)
	sort.Strings(ips)
	return strSliceEq(dns, s.dns) && strSliceEq(ips, s.ip)
}

func strSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
