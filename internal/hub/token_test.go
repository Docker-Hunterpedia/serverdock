package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateToken(t *testing.T) {
	gen, _ := GenerateToken()
	for _, ok := range []string{gen, "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG", "0123456789abcdef0123456789abcdef0123456789abcdef"} {
		if err := ValidateToken(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", 43), strings.Repeat("0f", 32), strings.Repeat("A-_", 15), strings.Repeat("a", 42), strings.Repeat("a", 42) + " ", strings.Repeat("a", 42) + "+", strings.Repeat("a", 42) + "=", strings.Repeat("a", 42) + "\n"} {
		err := ValidateToken(bad)
		if err == nil {
			t.Errorf("%q accepted", bad)
		} else if len(bad) >= 20 && strings.Contains(err.Error(), bad) {
			t.Errorf("error echoes the token: %v", err)
		}
	}
	if len(gen) != 43 {
		t.Errorf("generated token has %d chars", len(gen))
	}
}

func TestLoadOrCreateToken(t *testing.T) {
	dir := t.TempDir()
	tok, path, err := LoadOrCreateToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "agent.token") || ValidateToken(tok) != nil {
		t.Fatalf("%q %q", path, tok)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v %v", st.Mode().Perm(), err)
	}
	// A second start reuses it.
	tok2, _, err := LoadOrCreateToken(dir)
	if err != nil || tok2 != tok {
		t.Fatalf("token changed on restart: %v", err)
	}
	// An invalid file is an error, not replaced.
	os.WriteFile(path, []byte("weak\n"), 0o600)
	if _, _, err := LoadOrCreateToken(dir); err == nil {
		t.Error("weak token file accepted")
	}
	if b, _ := os.ReadFile(path); string(b) != "weak\n" {
		t.Error("invalid token file was overwritten")
	}
	// A group/world readable file is refused.
	os.WriteFile(path, []byte(tok+"\n"), 0o644)
	os.Chmod(path, 0o644)
	if _, _, err := LoadOrCreateToken(dir); err == nil {
		t.Error("world-readable token file accepted")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "agent.token.tmp*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}
