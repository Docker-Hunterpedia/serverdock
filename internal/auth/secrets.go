package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// newSecret returns 32 random bytes as base64url (the value a cookie or token
// carries) and the SHA-256 that is stored in its place.
func newSecret() (secret string, hash []byte, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	secret = base64.RawURLEncoding.EncodeToString(b[:])
	return secret, hashSecret(secret), nil
}

func hashSecret(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func hashRecovery(normalised string) []byte { return hashSecret("recovery:" + normalised) }

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	const digits = "0123456789abcdef"
	out := make([]byte, 2*n)
	for i, c := range b {
		out[2*i], out[2*i+1] = digits[c>>4], digits[c&15]
	}
	return string(out), nil
}

// Password and user name rules (docs/plans/0003, "Passwords").
const (
	minPasswordChars = 12
	maxPasswordBytes = 256
	maxUsernameBytes = 64
)

// ErrWeakPassword is wrapped by CheckPassword errors; the message says what to fix.
var ErrWeakPassword = errors.New("password rejected")

// ErrBadUsername is returned for a user name that fails ValidateUsername.
var ErrBadUsername = errors.New("invalid user name")

// CheckPassword applies the password rules: at least 12 characters and at most
// 256 bytes of valid UTF-8, no composition rules, no normalisation, and not the
// user name.
func CheckPassword(username, password string) error {
	switch {
	case !utf8.ValidString(password):
		return errors.Join(ErrWeakPassword, errors.New("the password must be valid UTF-8"))
	case len(password) > maxPasswordBytes:
		return errors.Join(ErrWeakPassword, errors.New("the password is longer than 256 bytes"))
	case utf8.RuneCountInString(password) < minPasswordChars:
		return errors.Join(ErrWeakPassword, errors.New("the password must have at least 12 characters"))
	case strings.EqualFold(password, username):
		return errors.Join(ErrWeakPassword, errors.New("the password must differ from the user name"))
	}
	return nil
}

// ValidateUsername accepts 1 to 64 bytes of letters, digits and . _ - @ (no
// spaces or control characters), so it is safe in logs, URLs and audit rows.
func ValidateUsername(name string) error {
	if name == "" || len(name) > maxUsernameBytes {
		return errors.Join(ErrBadUsername, errors.New("a user name has 1 to 64 characters"))
	}
	for _, r := range name {
		if r > unicode.MaxASCII || !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-@", r)) {
			return errors.Join(ErrBadUsername, errors.New("a user name may use letters, digits and . _ - @ only"))
		}
	}
	return nil
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
