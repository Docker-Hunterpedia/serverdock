//go:build !spa

// Package web embeds the single-page app. The default build embeds the committed
// placeholder page (web/dist/index.html), so `go build` and `go test` work with no
// Node installed. `go build -tags spa` embeds the real app from web/build instead
// (embed_spa.go); `make web` produces it. `serverdock version` reports which one a
// binary has, and the release and bench jobs assert "ui=spa".
package web

import (
	"embed"
	"io/fs"
)

//go:embed dist
var dist embed.FS

// UI is "placeholder" in this build.
const UI = "placeholder"

// FS returns the embedded files rooted at the app's top directory.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // dist is embedded at compile time
	}
	return sub
}
