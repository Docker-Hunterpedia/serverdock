package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/datadir"
)

// The panel leaf (ADR 0010, decision 1) is a self-signed ECDSA P-256 certificate
// for the HTTPS panel. It has its own code path here: it is never issued by the
// hub CA and nothing in this file touches ca.key.
const (
	// PanelLifetime is the leaf's validity: the longest Apple platforms accept
	// for a TLS server certificate.
	PanelLifetime = 825 * 24 * time.Hour
	// PanelRenewBefore: at start, a leaf with less than this left is regenerated.
	// There is no running renewal (no timer).
	PanelRenewBefore = 60 * 24 * time.Hour

	// PanelFile holds the key and the certificate (0600, replaced atomically).
	PanelFile = "panel.pem"
	// PanelFingerprintFile holds the public fingerprint.
	PanelFingerprintFile = "panel.fingerprint"

	hostsPrefix = "hosts:"
)

// PanelOptions configures EnsurePanelLeaf.
type PanelOptions struct {
	// Dir is DATA/pki; it is created 0700 whether or not the agent listener is on.
	Dir string
	// Hosts are the --panel-host names (DNS names or IPs), added to the SANs.
	Hosts []string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// InterfaceAddrs lists the host's addresses; nil means net.InterfaceAddrs.
	InterfaceAddrs func() ([]net.Addr, error)
	// Log receives the WARN line when a leaf is regenerated; nil discards.
	Log *slog.Logger
}

// Panel describes the panel leaf in use. Everything in it is public.
type Panel struct {
	// CertFile is DATA/pki/panel.pem (key and certificate).
	CertFile string
	// Fingerprint is SHA-256 over the DER, colon-separated upper-case hex, the
	// form browsers show.
	Fingerprint string
	NotBefore   time.Time
	NotAfter    time.Time
	// Regenerated is true when this call wrote a new leaf.
	Regenerated bool
}

// PanelFingerprint formats the SHA-256 of der the way browsers show it.
func PanelFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	var b strings.Builder
	for i, c := range sum {
		if i > 0 {
			b.WriteByte(':')
		}
		fmt.Fprintf(&b, "%02X", c)
	}
	return b.String()
}

// EnsurePanelLeaf loads DATA/pki/panel.pem or creates it. It regenerates the
// leaf when it is missing or unreadable, half written (bad PEM, key and
// certificate that do not match), not self-signed, lacking a base SAN, valid for
// less than PanelRenewBefore, or when the --panel-host set differs from the one it
// was created for. Every regeneration of a leaf that existed before logs a WARN
// with the new fingerprint. Nothing else depends on this key, so replacing a bad
// file is safe.
func EnsurePanelLeaf(o PanelOptions) (*Panel, error) {
	if o.Dir == "" {
		return nil, errors.New("pki: Dir is required")
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	hosts, err := parseSANs(o.Hosts)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	if err := os.Chmod(o.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	pemPath := filepath.Join(o.Dir, PanelFile)
	fpPath := filepath.Join(o.Dir, PanelFingerprintFile)

	reason := ""
	if cert, rerr := loadSelfSigned(pemPath); rerr != nil {
		reason = rerr.Error()
	} else if r := panelStale(cert, hosts, now()); r != "" {
		reason = r
	} else {
		p := panelInfo(pemPath, cert)
		// Keep the public fingerprint file in step with the certificate in use.
		if b, _ := os.ReadFile(fpPath); strings.TrimSpace(string(b)) != p.Fingerprint {
			_ = datadir.WriteAtomic(fpPath, []byte(p.Fingerprint+"\n"), 0o600)
		}
		return p, nil
	}

	_, pemErr := os.Stat(pemPath)
	_, fpErr := os.Stat(fpPath)
	firstTime := errors.Is(pemErr, os.ErrNotExist) && errors.Is(fpErr, os.ErrNotExist)

	addrs := o.InterfaceAddrs
	if addrs == nil {
		addrs = net.InterfaceAddrs
	}
	cert, err := newPanelLeaf(pemPath, hosts, addrs, now())
	if err != nil {
		return nil, err
	}
	p := panelInfo(pemPath, cert)
	p.Regenerated = true
	if err := datadir.WriteAtomic(fpPath, []byte(p.Fingerprint+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	if !firstTime {
		log.Warn("panel certificate regenerated; browsers will show the new fingerprint", "reason", reason, "fingerprint", p.Fingerprint)
	}
	return p, nil
}

func panelInfo(path string, c *x509.Certificate) *Panel {
	return &Panel{CertFile: path, Fingerprint: PanelFingerprint(c.Raw), NotBefore: c.NotBefore, NotAfter: c.NotAfter}
}

// hostsMarker is stored in the certificate subject so a changed --panel-host set
// is noticed without any other state.
func hostsMarker(h sanSet) string {
	all := append(append([]string{}, h.dns...), h.ip...)
	sort.Strings(all)
	return hostsPrefix + strings.Join(all, ",")
}

func panelStale(c *x509.Certificate, hosts sanSet, now time.Time) string {
	if !now.Add(PanelRenewBefore).Before(c.NotAfter) {
		return "fewer than 60 days of validity left"
	}
	if now.Before(c.NotBefore) {
		return "not valid yet"
	}
	want := hostsMarker(hosts)
	got := ""
	for _, ou := range c.Subject.OrganizationalUnit {
		if strings.HasPrefix(ou, hostsPrefix) {
			got = ou
		}
	}
	if got != want {
		return "the --panel-host set changed"
	}
	has := func(name string) bool {
		for _, d := range c.DNSNames {
			if d == name {
				return true
			}
		}
		return false
	}
	hasIP := func(ip string) bool {
		for _, a := range c.IPAddresses {
			if a.String() == ip {
				return true
			}
		}
		return false
	}
	if !has("localhost") || !hasIP("127.0.0.1") || !hasIP("::1") {
		return "missing a base subject alternative name"
	}
	for _, d := range hosts.dns {
		if !has(d) {
			return "missing a --panel-host name"
		}
	}
	for _, ip := range hosts.ip {
		if !hasIP(ip) {
			return "missing a --panel-host address"
		}
	}
	return ""
}

// loadSelfSigned reads panel.pem and checks it is one self-signed leaf whose
// key matches. Any problem is reported as the reason to regenerate.
func loadSelfSigned(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("panel.pem is missing")
		}
		return nil, fmt.Errorf("panel.pem is unreadable: %v", err)
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
			if certDER == nil {
				certDER = blk.Bytes
			}
		}
	}
	if keyDER == nil || certDER == nil {
		return nil, errors.New("panel.pem is incomplete")
	}
	key, err := x509.ParseECPrivateKey(keyDER)
	if err != nil {
		return nil, errors.New("panel.pem has an invalid key")
	}
	c, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, errors.New("panel.pem has an invalid certificate")
	}
	pub, ok := c.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, errors.New("panel.pem: the certificate does not match its key")
	}
	if c.IsCA || string(c.RawIssuer) != string(c.RawSubject) || c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature) != nil {
		return nil, errors.New("panel.pem is not a self-signed leaf")
	}
	return c, nil
}

func newPanelLeaf(path string, hosts sanSet, addrs func() ([]net.Addr, error), now time.Time) (*x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	dns := append([]string{"localhost"}, hosts.dns...)
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	seen := map[string]bool{"127.0.0.1": true, "::1": true}
	add := func(ip net.IP) {
		if k := ip.String(); !seen[k] {
			seen[k] = true
			ips = append(ips, ip)
		}
	}
	for _, v := range hosts.ip {
		add(net.ParseIP(v))
	}
	if as, err := addrs(); err == nil {
		for _, a := range as {
			if n, ok := a.(*net.IPNet); ok && n.IP.IsGlobalUnicast() {
				add(n.IP)
			}
		}
	}
	notBefore := now.Add(-clockSkew)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         "serverdock panel",
			OrganizationalUnit: []string{hostsMarker(hosts)},
		},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(PanelLifetime), // 825 days in all
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, // CA: false
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	buf := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	buf = append(buf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	if err := datadir.WriteAtomic(path, buf, 0o600); err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	return c, nil
}
