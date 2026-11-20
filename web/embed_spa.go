//go:build spa

package web

import (
	"embed"
	"io/fs"
)

// all: because SvelteKit's output lives under _app, a name //go:embed would skip.
//
//go:embed all:build
var build embed.FS

// UI is "spa" in this build.
const UI = "spa"

// FS returns the embedded files rooted at the app's top directory.
func FS() fs.FS {
	sub, err := fs.Sub(build, "build")
	if err != nil {
		panic(err) // build is embedded at compile time
	}
	return sub
}
