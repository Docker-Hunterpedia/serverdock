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
	"encoding/hex"
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

// lostKeyHint is the one recovery sentence every lost-or-wrong-key refusal ends with.
const lostKeyHint = "Restore the key from a backup; `serverdock admin reset` clears the admin sign-in data, and with `--forget-app-secrets` also deletes the app environment variables and registry credentials the lost key protected"

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

// KeyID returns the box's key id as hex (the first 4 bytes of SHA-256(key)).
// It is not secret: it is stored in the database so a swapped key file is
// noticed at start.
func (b *Box) KeyID() string { return hex.EncodeToString(b.keyID[:]) }

// KeyIDMetaKey is the meta-table key that records the id of the master key.
const KeyIDMetaKey = "secretbox_key_id"

// CiphertextIDer is an optional part of Meta: it reports the key id in the header
// of one existing ciphertext, so a key can be checked when no id is recorded.
type CiphertextIDer interface {
	// CiphertextKeyID returns the key id (hex) of one stored ciphertext, if any.
	CiphertextKeyID(ctx context.Context) (id string, ok bool, err error)
}

// Meta stores the id of the master key next to the data it protects.
type Meta interface {
	// KeyID returns the recorded key id, if any.
	KeyID(ctx context.Context) (id string, ok bool, err error)
	// SetKeyID records the key id.
	SetKeyID(ctx context.Context, id string) error
}

// KeyPath is where the master key lives under the data directory.
func KeyPath(dataDir string) string { return filepath.Join(dataDir, keyDir, keyFile) }

// LoadOrCreate returns the Box for DATA/keys/master.key.
//
//   - Missing, no key id recorded in the database, and ciphertextsExist reports
//     false: a new random key is written (mode 0600 in a 0700 directory,
//     atomically), its id is recorded through meta, and created is true.
//   - Missing, but a key id is recorded or ciphertexts exist: refused. A new key
//     would orphan them. `admin reset` clears the admin ciphertexts and the recorded
//     id; with app ciphertexts it also needs --forget-app-secrets.
//   - Present but unreadable, the wrong size, not a regular file, or readable by
//     group or others: refused. The file is never replaced.
//   - Present, and a recorded id differs from the key's: refused. (A key with no
//     recorded id, as an earlier version left it, is adopted: its id is recorded.)
//
// The caller holds the data-directory lock. Leftover master.key.tmp* files from
// a crashed write are removed here.
func LoadOrCreate(ctx context.Context, dataDir string, meta Meta, ciphertextsExist func() (bool, error)) (box *Box, created bool, err error) {
	dir := filepath.Join(dataDir, keyDir)
	path := KeyPath(dataDir)
	if err := ensureKeyDir(dir); err != nil {
		return nil, false, err
	}
	var recorded string
	var haveRecorded bool
	if meta != nil {
		if recorded, haveRecorded, err = meta.KeyID(ctx); err != nil {
			return nil, false, fmt.Errorf("secretbox: reading the recorded key id: %w", err)
		}
	}
	st, err := os.Lstat(path)
	switch {
	case err == nil:
		b, err := readKey(path, st)
		if err != nil {
			return nil, false, err
		}
		if haveRecorded && recorded != b.KeyID() {
			return nil, false, fmt.Errorf("%s does not match the key this database was written with (key id %s, expected %s). %s", path, b.KeyID(), recorded, lostKeyHint)
		}
		if !haveRecorded && meta != nil {
			// No recorded id (an earlier version left it): adopt the key only if
			// it matches the ciphertexts that already exist.
			if ci, ok := meta.(CiphertextIDer); ok {
				id, have, err := ci.CiphertextKeyID(ctx)
				if err != nil {
					return nil, false, fmt.Errorf("secretbox: reading a ciphertext's key id: %w", err)
				}
				if have && id != b.KeyID() {
					return nil, false, fmt.Errorf("%s does not match the key the stored ciphertexts were sealed with (key id %s, expected %s). %s", path, b.KeyID(), id, lostKeyHint)
				}
			}
			if err := meta.SetKeyID(ctx, b.KeyID()); err != nil {
				return nil, false, fmt.Errorf("secretbox: recording the key id: %w", err)
			}
		}
		return b, false, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, false, fmt.Errorf("%s cannot be read: %w. It is never replaced automatically; see the docs (admin reset) if it is lost", path, err)
	}
	if haveRecorded {
		return nil, false, fmt.Errorf("%s is missing but the database records its key (id %s). %s", path, recorded, lostKeyHint)
	}
	if ciphertextsExist != nil {
		exist, err := ciphertextsExist()
		if err != nil {
			return nil, false, fmt.Errorf("secretbox: checking for encrypted data: %w", err)
		}
		if exist {
			return nil, false, fmt.Errorf("%s is missing but encrypted data exists. %s", path, lostKeyHint)
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
	if err != nil {
		return nil, false, err
	}
	if meta != nil {
		// A crash between the two writes leaves a key with no recorded id, which the
		// next start adopts.
		if err := meta.SetKeyID(ctx, box.KeyID()); err != nil {
			return nil, false, fmt.Errorf("secretbox: recording the key id: %w", err)
		}
	}
	return box, true, nil
}

// SQLMeta keeps the key id in the meta table (key, value) of the database.
type SQLMeta struct{ R, W *sql.DB }

// KeyID implements Meta.
func (m SQLMeta) KeyID(ctx context.Context) (string, bool, error) {
	var v string
	err := m.R.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, KeyIDMetaKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// CiphertextKeyID implements CiphertextIDer: the key id in the header (bytes 1 to
// 4) of the first non-NULL ciphertext found in Columns().
func (m SQLMeta) CiphertextKeyID(ctx context.Context) (string, bool, error) {
	for _, c := range Columns() {
		var n int
		if err := m.R.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, c.Table).Scan(&n); err != nil {
			return "", false, err
		}
		if n == 0 {
			continue
		}
		var id sql.NullString
		q := `SELECT lower(hex(substr("` + c.Column + `", 2, 4))) FROM "` + c.Table + `" WHERE "` + c.Column + `" IS NOT NULL AND length("` + c.Column + `") >= 5 LIMIT 1`
		err := m.R.QueryRowContext(ctx, q).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if id.Valid && id.String != "" {
			return id.String, true, nil
		}
	}
	return "", false, nil
}

// SetKeyID implements Meta.
func (m SQLMeta) SetKeyID(ctx context.Context, id string) error {
	_, err := m.W.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, KeyIDMetaKey, id)
	return err
}

func ensureKeyDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("secretbox: %w", err)
	}
	// Half-written keys from a crashed atomic write (the caller holds the lock).
	if tmps, _ := filepath.Glob(filepath.Join(dir, keyFile+".tmp*")); len(tmps) > 0 {
		for _, t := range tmps {
			_ = os.Remove(t)
		}
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

// KeyState says whether the key file on disk is the one the database was
// written with.
type KeyState int

const (
	// KeyOK: a usable key whose id matches the recorded id (or none is recorded yet).
	KeyOK KeyState = iota
	// KeyMissing: there is no key file.
	KeyMissing
	// KeyWrong: a usable key whose id differs from the recorded one.
	KeyWrong
	// KeyUnusable: the file exists but cannot be used (damaged, wrong size, loose mode).
	KeyUnusable
)

// CheckKey inspects the key file without creating, replacing or adopting anything.
func CheckKey(ctx context.Context, dataDir string, meta Meta) (KeyState, error) {
	path := KeyPath(dataDir)
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return KeyMissing, nil
	}
	if err != nil {
		return KeyUnusable, nil
	}
	b, err := readKey(path, st)
	if err != nil {
		return KeyUnusable, nil
	}
	if meta != nil {
		recorded, ok, err := meta.KeyID(ctx)
		if err != nil {
			return KeyUnusable, fmt.Errorf("secretbox: reading the recorded key id: %w", err)
		}
		if ok && recorded != b.KeyID() {
			return KeyWrong, nil
		}
		if !ok {
			// No recorded id: compare with the header of an existing ciphertext.
			if ci, can := meta.(CiphertextIDer); can {
				id, have, err := ci.CiphertextKeyID(ctx)
				if err != nil {
					return KeyUnusable, fmt.Errorf("secretbox: reading a ciphertext's key id: %w", err)
				}
				if have && id != b.KeyID() {
					return KeyWrong, nil
				}
			}
		}
	}
	return KeyOK, nil
}

// Column names a database column that holds secretbox ciphertext.
type Column struct{ Table, Column string }

// AdminColumns lists the ciphertext columns of the admin's sign-in data. `admin
// reset` clears them.
func AdminColumns() []Column {
	return []Column{
		{"users", "totp_seed"},
		{"users", "totp_pending_seed"},
		{"setup_state", "pending_seed"},
	}
}

// AppColumns lists the ciphertext columns of app data (environment variables and
// registry credentials). `admin reset` never touches them unless the operator
// passes --forget-app-secrets and the master key is lost.
func AppColumns() []Column {
	return []Column{
		{"service_env_versions", "ciphertext"},
		{"registries", "secret"},
	}
}

// Columns lists every column that holds ciphertext. Later milestones add theirs
// here, so the "missing key but encrypted data exists" check covers them.
func Columns() []Column { return append(AdminColumns(), AppColumns()...) }

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
