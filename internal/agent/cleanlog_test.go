package agent

import (
	"strings"
	"testing"
)

// Log mode keeps newline and tab and nothing else that can drive a terminal or hide
// text: ESC (so CSI and OSC sequences lose their introducer), NUL, the C1 controls,
// carriage return, every bidirectional control and the other invisible formats.
func TestCleanLogTextRemovesEveryControlAndBidiCharacter(t *testing.T) {
	removed := []rune{0x00, 0x07, 0x08, 0x0b, 0x0c, 0x0d, 0x1b, 0x7f, 0x80, 0x85, 0x9b, 0x9d, // C0, DEL, C1 (CSI 0x9b, OSC 0x9d)
		0x061c, 0x200b, 0x200c, 0x200d, 0x200e, 0x200f, 0x2028, 0x2029, // marks, zero-width, separators
		0x202a, 0x202b, 0x202c, 0x202d, 0x202e, // embeddings and overrides
		0x2066, 0x2067, 0x2068, 0x2069, 0xfeff, 0xe0041} // isolates, BOM, tag
	for _, r := range removed {
		in := "a" + string(r) + "b"
		if got := CleanLogText(in, 100); got != "ab" {
			t.Errorf("U+%04X: %q", r, got)
		}
	}
	if got := CleanLogText("x\ny\tz\n", 100); got != "x\ny\tz\n" {
		t.Errorf("newline and tab: %q", got)
	}
	// An escape sequence is left as inert text, with no ESC to start it.
	got := CleanLogText("\x1b[2J\x1b[31mred\x1b]0;title\x07", 100)
	if strings.ContainsAny(got, "\x1b\x07") || got != "[2J[31mred]0;title" {
		t.Errorf("%q", got)
	}
	// A stray byte of the C1 range (invalid UTF-8) becomes U+FFFD, never a control.
	if got := CleanLogText("a\x9bb", 100); got != "a�b" {
		t.Errorf("%q", got)
	}
	// The cap holds on a rune boundary.
	if got := CleanLogText(strings.Repeat("é", 100), 7); got != strings.Repeat("é", 3) {
		t.Errorf("%q", got)
	}
}
