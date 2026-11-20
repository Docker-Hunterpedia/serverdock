// Package auth holds the password-hashing and (from M2 step 5) the sign-in
// building blocks. It has no package-level work: nothing here runs at load, so
// agent mode, which links it, pays nothing (docs/plans/0003, footprint rules).
package auth

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// argon2id parameters for new hashes (OWASP's 19 MiB, t = 2, p = 1). They are
// written into every PHC string, so they can be raised later.
const (
	argonMemoryKiB = 19 * 1024
	argonTime      = 2
	argonThreads   = 1
	argonSaltLen   = 16
	argonKeyLen    = 32

	// Upper bounds accepted when parsing a stored hash: a tampered database must
	// not be able to make a login allocate gigabytes.
	maxArgonMemoryKiB = 256 * 1024
	maxArgonTime      = 10
	maxArgonThreads   = 8
)

// errBadPHC is returned for a malformed or out-of-bounds hash string.
var errBadPHC = errors.New("auth: malformed password hash")

type phcParams struct {
	memory  uint32
	time    uint32
	threads uint8
	salt    []byte
	key     []byte
}

func encodePHC(p phcParams) string {
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", p.memory, p.time, p.threads, b64.EncodeToString(p.salt), b64.EncodeToString(p.key))
}

// parsePHC reads $argon2id$v=19$m=…,t=…,p=…$salt$key strictly: that exact
// algorithm and version, parameters within bounds, standard unpadded base64.
func parsePHC(s string) (phcParams, error) {
	var p phcParams
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return p, errBadPHC
	}
	kv := strings.Split(parts[3], ",")
	if len(kv) != 3 {
		return p, errBadPHC
	}
	var vals [3]uint64
	for i, name := range []string{"m=", "t=", "p="} {
		v, ok := strings.CutPrefix(kv[i], name)
		if !ok || v == "" || (len(v) > 1 && v[0] == '0') {
			return p, errBadPHC
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return p, errBadPHC
		}
		vals[i] = n
	}
	m, t, th := vals[0], vals[1], vals[2]
	if m < 8 || m > maxArgonMemoryKiB || t < 1 || t > maxArgonTime || th < 1 || th > maxArgonThreads || m < 8*th {
		return p, errBadPHC
	}
	b64 := base64.RawStdEncoding.Strict()
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return p, errBadPHC
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > 64 {
		return p, errBadPHC
	}
	return phcParams{memory: uint32(m), time: uint32(t), threads: uint8(th), salt: salt, key: key}, nil
}

func equalKeys(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }
