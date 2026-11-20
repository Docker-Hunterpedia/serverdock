package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

// spaHandler serves files from spa; paths without a file extension that do not
// exist fall back to index.html (client-side routing). Missing paths that look
// like files return 404, and so does anything under /api/, /debug/ or /agent/:
// those are never the SPA.
//
// Assets: a .js or .css file with a .gz beside it (gzip -9, made by the web
// build) is served pre-compressed to a client that accepts gzip, with
// Content-Encoding and Vary: Accept-Encoding; nothing is compressed at runtime.
// /_app/immutable/ files (content-hashed names) are cached for a year; index.html
// and everything else is no-cache. index.html and the fallback carry the CSP.
func spaHandler(spa fs.FS) http.Handler {
	files := http.FileServerFS(spa)
	csp := cspFor(spa)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, prefix := range []string{"/api/", "/debug/", "/agent/"} {
			if strings.HasPrefix(r.URL.Path, prefix) {
				WriteError(w, http.StatusNotFound, "not_found", "no such route")
				return
			}
		}
		p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if st, err := fs.Stat(spa, p); err == nil && !st.IsDir() {
			setCacheControl(w, p)
			if p == "index.html" || strings.HasSuffix(p, ".html") {
				w.Header().Set("Content-Security-Policy", csp())
			}
			if ext := path.Ext(p); ext == ".js" || ext == ".css" {
				if gz, err := fs.ReadFile(spa, p+".gz"); err == nil {
					w.Header().Add("Vary", "Accept-Encoding")
					if acceptsGzip(r.Header.Get("Accept-Encoding")) {
						w.Header().Set("Content-Type", mime.TypeByExtension(ext))
						w.Header().Set("Content-Encoding", "gzip")
						http.ServeContent(w, r, p, time.Time{}, bytes.NewReader(gz))
						return
					}
				}
			}
			files.ServeHTTP(w, r)
			return
		}
		if path.Ext(p) != "" {
			http.NotFound(w, r)
			return
		}
		index, err := fs.ReadFile(spa, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Security-Policy", csp())
		_, _ = w.Write(index)
	})
}

func setCacheControl(w http.ResponseWriter, p string) {
	if strings.HasPrefix(p, "_app/immutable/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
}

// acceptsGzip reports whether an Accept-Encoding header allows gzip (a "q=0" does not).
func acceptsGzip(h string) bool {
	for _, part := range strings.Split(h, ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(coding), "gzip") && strings.TrimSpace(coding) != "*" {
			continue
		}
		q := strings.ReplaceAll(strings.ToLower(params), " ", "")
		return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
	}
	return false
}

// cspFor returns the CSP of the SPA's HTML. The hashes of the inline <script>
// blocks of index.html are computed once, on first use, so they always match the
// embedded build.
func cspFor(spa fs.FS) func() string {
	return sync.OnceValue(func() string {
		var hashes []string
		if index, err := fs.ReadFile(spa, "index.html"); err == nil {
			hashes = inlineScriptHashes(string(index))
		}
		return CSP(hashes)
	})
}

// CSP builds the policy for the given inline-script hashes ('sha256-...').
func CSP(scriptHashes []string) string {
	script := "'self'"
	for _, h := range scriptHashes {
		script += " '" + h + "'"
	}
	return "default-src 'none'; script-src " + script + "; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
}

// inlineScriptHashes finds the <script> elements without a src attribute and
// returns 'sha256-<base64>' of their text, as CSP expects.
func inlineScriptHashes(html string) []string {
	var out []string
	lower := strings.ToLower(html)
	for i := 0; ; {
		start := strings.Index(lower[i:], "<script")
		if start < 0 {
			return out
		}
		start += i
		tagEnd := strings.IndexByte(lower[start:], '>')
		if tagEnd < 0 {
			return out
		}
		tagEnd += start
		end := strings.Index(lower[tagEnd:], "</script")
		if end < 0 {
			return out
		}
		end += tagEnd
		tag := lower[start:tagEnd]
		if !strings.Contains(tag, " src=") && !strings.Contains(tag, "\tsrc=") && !strings.Contains(tag, "\nsrc=") {
			sum := sha256.Sum256([]byte(html[tagEnd+1 : end]))
			out = append(out, "sha256-"+base64.StdEncoding.EncodeToString(sum[:]))
		}
		i = end + 1
	}
}
