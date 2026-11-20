package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 specifies HMAC-SHA-1; authenticator apps default to it
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP (RFC 6238): HMAC-SHA-1, 6 digits, 30 s, a 20-byte seed. A code is
// accepted within one step either way (±1). The store records the last accepted
// counter so no code is ever used twice.
const (
	totpDigits  = 6
	totpPeriod  = 30
	totpSkew    = 1
	totpSeedLen = 20
)

// NewTOTPSeed returns a fresh random seed.
func NewTOTPSeed() ([]byte, error) {
	seed := make([]byte, totpSeedLen)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	return seed, nil
}

// hotp is RFC 4226 with a variable number of digits (the RFC 6238 test vectors
// use 8).
func hotp(key []byte, counter uint64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, bin%mod)
}

// TOTPCounter is the time step containing t.
func TOTPCounter(t time.Time) int64 { return t.Unix() / totpPeriod }

// TOTPCode is the 6-digit code for the time step counter.
func TOTPCode(seed []byte, counter int64) string { return hotp(seed, uint64(counter), totpDigits) }

// VerifyTOTP checks code against the seed within ±1 step of now and returns the
// counter it matched. The caller must still record that counter with the store
// (a counter at or below the last accepted one is a reuse). code must be exactly
// six ASCII digits.
func VerifyTOTP(seed []byte, code string, now time.Time) (counter int64, ok bool) {
	if len(code) != totpDigits {
		return 0, false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < '0' || code[i] > '9' {
			return 0, false
		}
	}
	c := TOTPCounter(now)
	var matched int64
	found := 0
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		// Compare every candidate (no early exit) in constant time.
		if subtle.ConstantTimeCompare([]byte(code), []byte(TOTPCode(seed, c+d))) == 1 {
			matched, found = c+d, 1
		}
	}
	return matched, found == 1
}

// b32 is unpadded base32 (a function, not a package variable: no work at load).
func b32() *base32.Encoding { return base32.StdEncoding.WithPadding(base32.NoPadding) }

// TOTPSecret is the seed as the base32 text authenticator apps accept.
func TOTPSecret(seed []byte) string { return b32().EncodeToString(seed) }

// TOTPURI is the otpauth:// URI for the seed. label is the account name shown in
// the app (the user name).
func TOTPURI(seed []byte, label string) string {
	v := url.Values{}
	v.Set("secret", TOTPSecret(seed))
	v.Set("issuer", "ServerDock")
	v.Set("algorithm", "SHA1")
	v.Set("digits", "6")
	v.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape("ServerDock:"+label) + "?" + v.Encode()
}

// recovery codes: 10 x 16 base32 characters (80 bits), shown as xxxx-xxxx-xxxx-xxxx.
const (
	recoveryCount = 10
	recoveryChars = 16
)

// NewRecoveryCodes returns the codes to show the user (once) and their SHA-256
// hashes to store.
func NewRecoveryCodes() (display []string, hashes [][]byte, err error) {
	for i := 0; i < recoveryCount; i++ {
		raw := make([]byte, 10) // 80 bits = 16 base32 characters
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, err
		}
		code := b32().EncodeToString(raw)
		display = append(display, strings.ToLower(code[0:4]+"-"+code[4:8]+"-"+code[8:12]+"-"+code[12:16]))
		hashes = append(hashes, hashRecovery(code))
	}
	return display, hashes, nil
}

// normaliseRecovery upper-cases and drops dashes and spaces; ok is false unless
// the result is 16 base32 characters.
func normaliseRecovery(s string) (string, bool) {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(s)))
	if len(s) != recoveryChars {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= 'A' && c <= 'Z' || c >= '2' && c <= '7') {
			return "", false
		}
	}
	return s, true
}
