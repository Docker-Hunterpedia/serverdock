//go:build spa

package web

import (
	"bytes"
	"compress/gzip"
	"io/fs"
	"strings"
	"testing"
)

// The -tags spa build embeds the real app, with its pre-compressed twins, inside the
// 250 KB gzip JavaScript budget.
func TestSPABuildEmbedsTheApp(t *testing.T) {
	if UI != "spa" {
		t.Fatalf("UI = %q", UI)
	}
	idx, err := fs.ReadFile(FS(), "index.html")
	if err != nil || !strings.Contains(string(idx), "/_app/immutable/") {
		t.Fatalf("index.html is not the SvelteKit app: %v", err)
	}
	total, files := 0, 0
	err = fs.WalkDir(FS(), "_app/immutable", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return err
		}
		plain, _ := fs.ReadFile(FS(), p)
		var b bytes.Buffer
		w, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
		w.Write(plain)
		w.Close()
		total += b.Len()
		files++
		if _, err := fs.Stat(FS(), p+".gz"); err != nil {
			t.Errorf("%s has no pre-compressed twin", p)
		}
		return nil
	})
	if err != nil || files == 0 {
		t.Fatalf("no JavaScript embedded: %v", err)
	}
	if total > 250*1000 {
		t.Errorf("%d files, %d bytes of gzip JavaScript: over the 250 KB budget", files, total)
	}
	t.Logf("%d JavaScript files, %d bytes gzip", files, total)
}
