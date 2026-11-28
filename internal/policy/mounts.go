package policy

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxMounts is the most mounts one service may have.
const MaxMounts = 10

// Mount kinds, as a snapshot and the database record them.
const (
	MountVolume = "volume"
	MountBind   = "bind"
)

// ValidateVolumeName checks the short name of a named volume: ^[a-z][a-z0-9-]{0,29}$.
// The Docker name is sd-<install id[:8]>-s<service id>-<name>.
func ValidateVolumeName(s string) error {
	if s == "" || len(s) > 30 || s[0] < 'a' || s[0] > 'z' {
		return errors.New("a volume name is 1 to 30 characters and starts with a lowercase letter")
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return errors.New("a volume name holds only lowercase letters, digits and -")
		}
	}
	return nil
}

// ValidateMountTarget checks the path a mount appears at inside the container:
// absolute, canonical (path.Clean), at most 255 bytes, printable ASCII with no
// spaces or ":", not "/", and not under /proc, /sys or /dev.
func ValidateMountTarget(p string) error {
	if len(p) == 0 || len(p) > 255 || p[0] != '/' {
		return errors.New("a mount target must be an absolute path of at most 255 bytes")
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c <= ' ' || c >= 0x7f || c == ':' {
			return errors.New("a mount target holds only printable ASCII, without spaces or :")
		}
	}
	if path.Clean(p) != p {
		return errors.New("a mount target must be a clean path (no //, ., .. or trailing /)")
	}
	if p == "/" {
		return errors.New("a mount target may not be /")
	}
	for _, bad := range []string{"/proc", "/sys", "/dev"} {
		if p == bad || strings.HasPrefix(p, bad+"/") {
			return fmt.Errorf("a mount target may not be %s or under it", bad)
		}
	}
	return nil
}

// ValidateBindSourceShape checks only the shape of a bind source as entered:
// absolute, canonical, at most 255 bytes, printable characters, no ".." and no "," or ":". Whether it
// is allowed (bind roots, denied paths, overlaps) is decided where the host is
// known, not here.
func ValidateBindSourceShape(p string) error {
	if len(p) == 0 || len(p) > 255 || p[0] != '/' {
		return errors.New("a bind source must be an absolute path of at most 255 bytes")
	}
	if !utf8.ValidString(p) {
		return errors.New("a bind source must be valid UTF-8")
	}
	for _, r := range p {
		if !unicode.IsPrint(r) {
			return errors.New("a bind source holds printable characters only")
		}
		if r == ',' || r == ':' {
			// Defence in depth: both are separators in Docker's mount syntax.
			return errors.New("a bind source may not contain , or :")
		}
	}
	if path.Clean(p) != p {
		return errors.New("a bind source must be a clean path (no //, ., .. or trailing /)")
	}
	return nil
}

// MinMemoryMB is the smallest memory limit.
const MinMemoryMB = 32

// ValidateMemoryMB checks a memory limit (the host's total is checked where it is known).
func ValidateMemoryMB(v int) error {
	if v < MinMemoryMB {
		return fmt.Errorf("the memory limit must be at least %d MB", MinMemoryMB)
	}
	return nil
}

// ValidateCPUsMilli checks a CPU limit in thousandths of a CPU: at least 50, in
// steps of 50 (the host's CPU count is checked where it is known).
func ValidateCPUsMilli(v int) error {
	if v < 50 || v%50 != 0 {
		return errors.New("the CPU limit must be at least 0.05 and in steps of 0.05")
	}
	return nil
}
