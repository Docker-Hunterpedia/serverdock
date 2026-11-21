package api

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Docker-Hunterpedia/serverdock/web"
)

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func spaFS(t *testing.T) fstest.MapFS {
	js := strings.Repeat("console.log('app');\n", 50)
	css := strings.Repeat("body{margin:0}\n", 50)
	return fstest.MapFS{
		"index.html":                           {Data: []byte(`<!doctype html><html><body><script>start()</script></body></html>`)},
		"_app/immutable/entry/app.abc.js":      {Data: []byte(js)},
		"_app/immutable/entry/app.abc.js.gz":   {Data: gz(t, js)},
		"_app/immutable/assets/app.abc.css":    {Data: []byte(css)},
		"_app/immutable/assets/app.abc.css.gz": {Data: gz(t, css)},
		"_app/immutable/chunks/nogz.js":        {Data: []byte("x=1")}, // no pre-compressed twin
		"_app/version.json":                    {Data: []byte(`{"version":"1"}`)},
	}
}

func serve(t *testing.T, h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Host = "127.0.0.1:8443"
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestSPAServesPreCompressedAssets(t *testing.T) {
	fsys := spaFS(t)
	h := New(Config{SPA: fsys}).Handler()
	js := string(fsys["_app/immutable/entry/app.abc.js"].Data)

	// A client that accepts gzip gets the .gz as it is, with the right type and Vary.
	rec := serve(t, h, "/_app/immutable/entry/app.abc.js", map[string]string{"Accept-Encoding": "gzip, deflate, br"})
	if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("content type %q", ct)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(zr); string(got) != js {
		t.Error("the gzip body does not decompress to the asset")
	}
	// Without gzip: the plain file, still Vary (a cache must not mix them up).
	for name, ae := range map[string]string{"none": "", "identity": "identity", "q=0": "gzip;q=0", "br only": "br"} {
		rec := serve(t, h, "/_app/immutable/entry/app.abc.js", map[string]string{"Accept-Encoding": ae})
		if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != js || rec.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("%s: %d enc=%q vary=%q", name, rec.Code, rec.Header().Get("Content-Encoding"), rec.Header().Get("Vary"))
		}
	}
	// CSS likewise; a file without a .gz is served plain even to gzip clients, without Vary.
	if rec := serve(t, h, "/_app/immutable/assets/app.abc.css", map[string]string{"Accept-Encoding": "gzip"}); rec.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(rec.Header().Get("Content-Type"), "css") {
		t.Errorf("css: %v", rec.Header())
	}
	if rec := serve(t, h, "/_app/immutable/chunks/nogz.js", map[string]string{"Accept-Encoding": "gzip"}); rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != "x=1" {
		t.Errorf("a file without a twin: %v %q", rec.Header(), rec.Body)
	}
	// The .gz files themselves are not routes of their own that bypass negotiation...
	// they are plain files (a request for one gets the bytes), never marked as encoded.
	if rec := serve(t, h, "/_app/immutable/entry/app.abc.js.gz", nil); rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("a .gz asked for by name is labelled as encoded: %v", rec.Header())
	}
}

func TestSPACacheHeaders(t *testing.T) {
	h := New(Config{SPA: spaFS(t)}).Handler()
	for path, want := range map[string]string{
		"/_app/immutable/entry/app.abc.js":   "public, max-age=31536000, immutable",
		"/_app/immutable/assets/app.abc.css": "public, max-age=31536000, immutable",
		"/_app/immutable/chunks/nogz.js":     "public, max-age=31536000, immutable",
		"/":                                  "no-cache",
		"/settings/security":                 "no-cache", // the fallback
		"/_app/version.json":                 "no-cache",
	} {
		rec := serve(t, h, path, map[string]string{"Accept-Encoding": "gzip"})
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != want {
			t.Errorf("%s: %d Cache-Control %q, want %q", path, rec.Code, rec.Header().Get("Cache-Control"), want)
		}
	}
}

var inlineHandler = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)

// The CSP comes from the embedded index.html's inline scripts, has no
// 'unsafe-inline' and no 'unsafe-eval', and index.html has no inline event handler
// or inline style attribute that the policy would block.
func TestEmbeddedIndexMeetsTheCSP(t *testing.T) {
	raw, err := fs.ReadFile(web.FS(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	index := string(raw)
	if inlineHandler.MatchString(index) {
		t.Errorf("index.html has an inline event handler: %s", inlineHandler.FindString(index))
	}
	if strings.Contains(strings.ToLower(index), " style=") {
		t.Error("index.html has an inline style attribute, which style-src 'self' blocks")
	}
	h := New(Config{SPA: web.FS()}).Handler()
	csp := serve(t, h, "/", nil).Header().Get("Content-Security-Policy")
	for _, bad := range []string{"unsafe-inline", "unsafe-eval", "*"} {
		if strings.Contains(csp, bad) {
			t.Errorf("CSP contains %q: %s", bad, csp)
		}
	}
	// One hash per inline script of the embedded file, computed from it.
	if want := len(inlineScriptHashes(index)); strings.Count(csp, "'sha256-") != want {
		t.Errorf("CSP has %d script hashes, index.html has %d inline scripts: %s", strings.Count(csp, "'sha256-"), want, csp)
	}
}

// If the real build is on disk (make web), check it too (CI runs this with it present).
func TestBuiltSPAIsServedAndMeetsTheCSP(t *testing.T) {
	dir := filepath.Join("..", "..", "web", "build")
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		t.Skip("web/build is not built (make web)")
	}
	fsys := os.DirFS(dir)
	h := New(Config{SPA: fsys}).Handler()
	raw, _ := os.ReadFile(filepath.Join(dir, "index.html"))
	if inlineHandler.MatchString(string(raw)) || strings.Contains(strings.ToLower(string(raw)), " style=") {
		t.Error("the built index.html has an inline handler or style attribute")
	}
	rec := serve(t, h, "/", nil)
	csp := rec.Header().Get("Content-Security-Policy")
	if rec.Code != 200 || strings.Contains(csp, "unsafe-") || strings.Count(csp, "'sha256-") != len(inlineScriptHashes(string(raw))) || len(inlineScriptHashes(string(raw))) == 0 {
		t.Fatalf("%d %s", rec.Code, csp)
	}
	// Every .js and .css has a .gz that decompresses to it, and is served as such.
	n := 0
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !(strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".css")) {
			return nil
		}
		n++
		plain, _ := os.ReadFile(p)
		gzb, err := os.ReadFile(p + ".gz")
		if err != nil {
			t.Errorf("%s has no .gz", p)
			return nil
		}
		zr, _ := gzip.NewReader(bytes.NewReader(gzb))
		if got, _ := io.ReadAll(zr); !bytes.Equal(got, plain) {
			t.Errorf("%s.gz does not match", p)
		}
		rel, _ := filepath.Rel(dir, p)
		r := serve(t, h, "/"+filepath.ToSlash(rel), map[string]string{"Accept-Encoding": "gzip"})
		if r.Header().Get("Content-Encoding") != "gzip" || r.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Errorf("%s: %v", rel, r.Header())
		}
		return nil
	})
	if n == 0 {
		t.Error("no js or css in the build")
	}
	// A deep link falls back to index.html with the CSP; an API-looking path does not.
	if r := serve(t, h, "/settings/security", nil); r.Code != 200 || r.Header().Get("Content-Security-Policy") != csp {
		t.Errorf("the fallback: %d", r.Code)
	}
}
