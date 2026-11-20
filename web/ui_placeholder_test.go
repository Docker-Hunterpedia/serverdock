//go:build !spa

package web

import (
	"io/fs"
	"strings"
	"testing"
)

// A plain `go test` (no Node, no web build) gets the placeholder page, and says so.
func TestPlaceholderBuildEmbedsThePlaceholder(t *testing.T) {
	if UI != "placeholder" {
		t.Fatalf("UI = %q", UI)
	}
	b, err := fs.ReadFile(FS(), "index.html")
	if err != nil || !strings.Contains(string(b), "<title>ServerDock</title>") {
		t.Fatalf("%v %q", err, b)
	}
}
