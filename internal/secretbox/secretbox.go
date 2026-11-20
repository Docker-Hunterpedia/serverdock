// Package secretbox seals small secrets (TOTP seeds, later app secrets) at rest
// with AES-256-GCM under a master key kept in DATA/keys/master.key.
//
// Format: version (1 byte, 0x01) | key id (4 bytes, the first 4 bytes of
// SHA-256(key)) | nonce (12 bytes) | ciphertext and tag. The additional
// authenticated data is "<table>/<row id>/<column>", so a ciphertext copied to
// another row or column does not open.
//
// Errors never contain plaintext or key bytes. Nothing here runs at package load.
package secretbox

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Docker-Hunterpedia/serverdock/internal/datadir"
)

const (
	version   = 1
	keyLen    = 32
	keyIDLen  = 4
	nonceLen  = 12
	tagLen    = 16
	headerLen = 1 + keyIDLen + nonceLen
	keyDir    = "keys"
	keyFile   = "master.key"
)

// ErrWrongKey is returned by Open when the ciphertext was sealed under a
// different key than the one loaded.
var ErrWrongKey = errors.New("secretbox: the ciphertext was sealed with a different key")

// ErrCorrupt is returned by Open when the ciphertext is malformed, was sealed
// for another AAD, or has been changed.
var ErrCorrupt = errors.New("secretbox: the ciphertext cannot be opened (wrong row or column, or damaged)")

// Box seals and opens values under one master key.
type Box struct {
	aead  cipher.AEAD
	keyID [keyIDLen]byte
}

// New returns a Box for a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("secretbox: key must be %d bytes", keyLen)
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(key)
	b := &Box{aead: aead}
	copy(b.keyID[:], sum[:keyIDLen])
	return b, nil
}

// Seal encrypts plaintext for the place named by aad.
func (b *Box) Seal(aad string, plaintext []byte) ([]byte, error) {
	out := make([]byte, headerLen, headerLen+len(plaintext)+tagLen)
	out[0] = version
	copy(out[1:], b.keyID[:])
	nonce := out[1+keyIDLen : headerLen]
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	return b.aead.Seal(out, nonce, plaintext, []byte(aad)), nil
}

// Open decrypts ct, which must have been sealed for the same aad. It returns
// ErrWrongKey when the key id does not match this box's key.
func (b *Box) Open(aad string, ct []byte) ([]byte, error) {
	if len(ct) < headerLen+tagLen || ct[0] != version {
		return nil, ErrCorrupt
	}
	if subtle.ConstantTimeCompare(ct[1:1+keyIDLen], b.keyID[:]) != 1 {
		return nil, ErrWrongKey
	}
	nonce := ct[1+keyIDLen : headerLen]
	pt, err := b.aead.Open(nil, nonce, ct[headerLen:], []byte(aad))
	if err != nil {
		return nil, ErrCorrupt
	}
	return pt, nil
}

// KeyPath is where the master key lives under the data directory.
func KeyPath(dataDir string) string { return filepath.Join(dataDir, keyDir, keyFile) }

// LoadOrCreate returns the Box for DATA/keys/master.key.
//
//   - Missing, and ciphertextsExist reports false: a new random key is written
//     (mode 0600 in a 0700 directory, atomically) and created is true.
//   - Missing, but ciphertexts exist: refused. A new key would orphan them.
//   - Present but unreadable, the wrong size, not a regular file, or readable by
//     group or others: refused. The file is never replaced.
func LoadOrCreate(dataDir string, ciphertextsExist func() (bool, error)) (box *Box, created bool, err error) {
	dir := filepath.Join(dataDir, keyDir)
	path := KeyPath(dataDir)
	if err := ensureKeyDir(dir); err != nil {
		return nil, false, err
	}
	st, err := os.Lstat(path)
	switch {
	case err == nil:
		b, err := readKey(path, st)
		return b, false, err
	case !errors.Is(err, fs.ErrNotExist):
		return nil, false, fmt.Errorf("%s cannot be read: %w. It is never replaced automatically; see the docs (admin reset) if it is lost", path, err)
	}
	if ciphertextsExist != nil {
		exist, err := ciphertextsExist()
		if err != nil {
			return nil, false, fmt.Errorf("secretbox: checking for encrypted data: %w", err)
		}
		if exist {
			return nil, false, fmt.Errorf("%s is missing but encrypted data exists. Restore the key from a backup, or run `serverdock admin reset` to clear the encrypted admin data", path)
		}
	}
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, false, fmt.Errorf("secretbox: %w", err)
	}
	if err := datadir.WriteAtomic(path, key, 0o600); err != nil {
		return nil, false, fmt.Errorf("secretbox: writing %s: %w", path, err)
	}
	box, err = New(key)
	return box, true, err
}

func ensureKeyDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("secretbox: %w", err)
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("secretbox: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must not be accessible to group or others (mode %04o); chmod 700 %s", dir, st.Mode().Perm(), dir)
	}
	return nil
}

func readKey(path string, st fs.FileInfo) (*Box, error) {
	const hint = ". It is never replaced automatically. Stop the server, move it aside and run `serverdock admin reset`, or restore it from a backup"
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file%s", path, hint)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is accessible to group or others (mode %04o); chmod 600 %s", path, st.Mode().Perm(), path)
	}
	if st.Size() != keyLen {
		return nil, fmt.Errorf("%s has the wrong size (%d bytes, want %d)%s", path, st.Size(), keyLen, hint)
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s cannot be read: %w%s", path, err, hint)
	}
	if len(key) != keyLen {
		return nil, fmt.Errorf("%s has the wrong size (%d bytes, want %d)%s", path, len(key), keyLen, hint)
	}
	return New(key)
}

// Column names a database column that holds secretbox ciphertext.
type Column struct{ Table, Column string }

// Columns lists every column that holds ciphertext. Later milestones add theirs
// here, so the "missing key but encrypted data exists" check covers them.
func Columns() []Column {
	return []Column{
		{"users", "totp_seed"},
		{"users", "totp_pending_seed"},
		{"setup_state", "pending_seed"},
	}
}

// CiphertextsExist reports whether any column in cols holds a non-NULL value.
// A table that does not exist yet (the schema has not reached it) counts as empty.
func CiphertextsExist(ctx context.Context, db *sql.DB, cols []Column) (bool, error) {
	for _, c := range cols {
		if !safeIdent(c.Table) || !safeIdent(c.Column) {
			return false, fmt.Errorf("secretbox: bad column name %q.%q", c.Table, c.Column)
		}
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, c.Table).Scan(&n); err != nil {
			return false, err
		}
		if n == 0 {
			continue
		}
		var has bool
		q := `SELECT EXISTS(SELECT 1 FROM "` + c.Table + `" WHERE "` + c.Column + `" IS NOT NULL)`
		if err := db.QueryRowContext(ctx, q).Scan(&has); err != nil {
			return false, err
		}
		if has {
			return true, nil
		}
	}
	return false, nil
}

func safeIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c == '_' || c >= '0' && c <= '9' && i > 0) {
			return false
		}
	}
	return true
}
