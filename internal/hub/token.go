package hub

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MinTokenLen is the shortest accepted agent token: 43 base64url characters
// carry 256 bits; hex of 32 bytes is 64.
const MinTokenLen = 43

// TokenFile is the generated token's name inside the data directory.
const TokenFile = "agent.token"

// ValidateToken refuses a token that is too short or uses characters outside
// base64url / hex. The token itself is never included in the error.
func ValidateToken(tok string) error {
	if len(tok) < MinTokenLen {
		return fmt.Errorf("agent token is too short (%d characters, need at least %d: 32 random bytes as base64url or hex)", len(tok), MinTokenLen)
	}
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return errors.New("agent token may only contain base64url characters (A-Z a-z 0-9 - _)")
		}
	}
	return nil
}

// GenerateToken returns 32 random bytes as 43 base64url characters.
func GenerateToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// LoadOrCreateToken returns the token in dir/agent.token, creating it (mode
// 0600, atomically) if the file does not exist. An existing file that is not a
// valid token is an error, never silently replaced. The caller prints the path,
// not the token.
func LoadOrCreateToken(dir string) (token, path string, err error) {
	path = filepath.Join(dir, TokenFile)
	b, err := os.ReadFile(path)
	if err == nil {
		tok := strings.TrimSpace(string(b))
		if err := ValidateToken(tok); err != nil {
			return "", path, fmt.Errorf("%s: %w", path, err)
		}
		if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
			return "", path, fmt.Errorf("%s is readable by other users (mode %v); chmod 600 it", path, st.Mode().Perm())
		}
		return tok, path, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", path, err
	}
	tok, err := GenerateToken()
	if err != nil {
		return "", path, err
	}
	tmp, err := os.CreateTemp(dir, TokenFile+".tmp*")
	if err != nil {
		return "", path, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", path, err
	}
	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		tmp.Close()
		return "", path, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", path, err
	}
	if err := tmp.Close(); err != nil {
		return "", path, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", path, err
	}
	return tok, path, nil
}
