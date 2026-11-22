package policy

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ValidateName checks a project or service name: ^[a-z][a-z0-9-]{0,29}$, and it
// may not end in "-" (container names join them with "-").
func ValidateName(s string) error {
	if s == "" || len(s) > 30 {
		return errors.New("a name must be 1 to 30 characters")
	}
	if s[0] < 'a' || s[0] > 'z' {
		return errors.New("a name must start with a lowercase letter")
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return errors.New("a name holds only lowercase letters, digits and -")
		}
	}
	if s[len(s)-1] == '-' {
		return errors.New("a name may not end in -")
	}
	return nil
}

// ValidateHealthPath checks the path of an HTTP health check: it starts with
// "/", is at most 256 bytes, and holds only printable ASCII without spaces (and
// without "#" and "\", which a URL parser would read differently).
func ValidateHealthPath(p string) error {
	if len(p) == 0 || len(p) > 256 || p[0] != '/' {
		return errors.New("the health path must start with / and be at most 256 bytes")
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c <= ' ' || c >= 0x7f || c == '#' || c == '\\' {
			return errors.New("the health path must be printable ASCII without spaces, # or backslash")
		}
	}
	return nil
}

func checkRange(what string, v, lo, hi int) error {
	if v < lo || v > hi {
		return fmt.Errorf("%s must be %d to %d", what, lo, hi)
	}
	return nil
}

// The numeric ranges of a service's settings (plan 0004, data model).
func ValidateContainerPort(v int) error { return checkRange("the container port", v, 1, 65535) }
func ValidateHealthTimeoutS(v int) error {
	return checkRange("the health timeout (seconds)", v, 5, 600)
}
func ValidateHealthPasses(v int) error { return checkRange("the health passes", v, 1, 10) }

// ValidateDrainS: never below Caddy's 10 s reload grace.
func ValidateDrainS(v int) error     { return checkRange("the drain time (seconds)", v, 10, 300) }
func ValidateStopGraceS(v int) error { return checkRange("the stop grace (seconds)", v, 0, 300) }

// ValidatePublicPort checks a service's public port: 1024 to 65535, never 80
// or 443, and never one of ServerDock's own listen ports (own). Uniqueness
// across services is the database's job (a UNIQUE column).
func ValidatePublicPort(port int, own ...int) error {
	if err := checkRange("the public port", port, 1024, 65535); err != nil {
		return err
	}
	if port == 80 || port == 443 {
		return errors.New("the public port may not be 80 or 443")
	}
	for _, o := range own {
		if port == o {
			return errors.New("the public port is one of ServerDock's own ports")
		}
	}
	return nil
}

// CheckServiceCount refuses the service that would exceed MaxServices; have is
// the count before the new one.
func CheckServiceCount(have int) error {
	if have >= MaxServices {
		return fmt.Errorf("an install has at most %d services", MaxServices)
	}
	return nil
}

// CheckFreeDisk refuses a pull when the free space on Docker's data root is
// below MinFreeBytes.
func CheckFreeDisk(free uint64) error {
	if free < MinFreeBytes {
		return fmt.Errorf("less than %d MiB free on Docker's data directory", MinFreeBytes>>20)
	}
	return nil
}

const reservedPrefix = "serverdock."

// IsReservedLabelKey reports whether a label key is in ServerDock's namespace,
// whatever its case: its first 11 characters fold to "serverdock." (Unicode
// simple folding, so "SERVERDOCK." and "ſerverdock." both count).
func IsReservedLabelKey(k string) bool {
	end := 0
	for n := 0; n < len(reservedPrefix); n++ { // the prefix is ASCII: 11 characters
		if end >= len(k) {
			return false
		}
		_, w := utf8.DecodeRuneInString(k[end:])
		end += w
	}
	return strings.EqualFold(k[:end], reservedPrefix)
}

// Bounds on user labels (M3b and M6 use them; M3a has no label input).
const (
	maxUserLabels   = 32
	maxLabelKeyLen  = 128
	maxLabelValueLn = 256
)

// ValidateUserLabels checks labels a user supplies for a container: none may be
// in the serverdock.* namespace (that namespace is ownership), keys are
// printable ASCII without spaces, values hold no control characters.
func ValidateUserLabels(labels map[string]string) error {
	if len(labels) > maxUserLabels {
		return fmt.Errorf("at most %d labels", maxUserLabels)
	}
	for k, v := range labels {
		if k == "" || len(k) > maxLabelKeyLen {
			return errors.New("a label key must be 1 to 128 bytes")
		}
		for i := 0; i < len(k); i++ {
			if c := k[i]; c <= ' ' || c >= 0x7f {
				return errors.New("a label key must be printable ASCII without spaces")
			}
		}
		if IsReservedLabelKey(k) {
			return errors.New("labels in the serverdock.* namespace are reserved (label spoofing rejected)")
		}
		if len(v) > maxLabelValueLn {
			return errors.New("a label value is at most 256 bytes")
		}
		for _, r := range v {
			if r < 0x20 || r == 0x7f {
				return errors.New("a label value may not hold control characters")
			}
		}
	}
	return nil
}
