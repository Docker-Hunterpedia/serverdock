package agent

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Caps for cleaned text (ADR 0012).
const (
	// MaxReasonText caps an error or reason string.
	MaxReasonText = 512
	// MaxDetailText caps a stage detail.
	MaxDetailText = 256
	// MaxAgentVersion caps the agent version string in `hello`.
	MaxAgentVersion = 64
)

// CleanText makes a string that came from Docker, a registry, an image, a
// container or an agent safe to store and show: invalid UTF-8 is replaced with
// U+FFFD, every control character (C0 including newline and tab, DEL, C1), every
// format character (zero-width, soft hyphen, BOM, tag block, bidirectional
// controls) and the line and paragraph separators are removed, and the result is
// at most max bytes (cut on a rune boundary). It is the M3a part of G13.
func CleanText(s string, max int) string { return clean(s, max, false) }

// CleanLogText is CleanText for log bodies: it keeps newline and tab.
func CleanLogText(s string, max int) string { return clean(s, max, true) }

func clean(s string, max int, keepNLTab bool) string {
	if max <= 0 {
		return ""
	}
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	b.Grow(min(len(s), max))
	for _, r := range s {
		if r == '\n' || r == '\t' {
			if !keepNLTab {
				continue
			}
		} else if unicode.IsControl(r) || isInvisible(r) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isInvisible reports format characters (Cf: zero-width characters, the soft
// hyphen, the byte order mark, the tag block, bidi controls) and the line and
// paragraph separators (Zl, Zp), which can hide or reorder text.
func isInvisible(r rune) bool {
	if unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
		return true
	}
	switch {
	case r >= 0x202A && r <= 0x202E: // embeddings and overrides
		return true
	case r >= 0x2066 && r <= 0x2069: // isolates
		return true
	case r == 0x200E || r == 0x200F || r == 0x061C: // marks
		return true
	}
	return false
}
