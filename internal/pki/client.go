package pki

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"time"
)

// ClientTLSConfig returns the agent's TLS config for dialing host. It pins the
// hub CA by fingerprint instead of trusting any system root:
//
//  1. find the CA in the peer chain whose SHA-256 matches the fingerprint;
//  2. verify the leaf against that CA only, for the dialed host (an IP host is
//     matched against IP SANs) and the serverAuth EKU;
//  3. require the hub role URI SAN.
//
// TLS 1.3 only. The standard verification is replaced by VerifyConnection (so
// InsecureSkipVerify is set on purpose); now is injectable for tests.
func ClientTLSConfig(host, fingerprint string, now func() time.Time) (*tls.Config, error) {
	want, err := ParseFingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	if host == "" {
		return nil, errors.New("pki: host is required")
	}
	if now == nil {
		now = time.Now
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by VerifyConnection below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if err := verifyHub(cs.PeerCertificates, host, want, now()); err != nil {
				return fmt.Errorf("%w: %v", ErrVerify, err)
			}
			return nil
		},
	}, nil
}

// ErrVerify marks every hub-verification failure, so callers can tell a pin or
// name mismatch from a network error.
var ErrVerify = errors.New("hub certificate verification failed")

func verifyHub(chain []*x509.Certificate, host string, want [32]byte, now time.Time) error {
	if len(chain) == 0 {
		return errors.New("hub sent no certificate")
	}
	leaf := chain[0]
	var ca *x509.Certificate
	for _, c := range chain[1:] {
		if DigestOf(c.Raw) == want {
			ca = c
			break
		}
	}
	if ca == nil {
		return errors.New("hub certificate chain does not contain the pinned CA")
	}
	if !ca.IsCA {
		return errors.New("pinned certificate is not a CA")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: now,
		DNSName:     hostOnly(host),
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("hub certificate rejected: %w", err)
	}
	for _, u := range leaf.URIs {
		if u.String() == RoleHub {
			return nil
		}
	}
	return errors.New("hub certificate lacks the hub role marker")
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}
