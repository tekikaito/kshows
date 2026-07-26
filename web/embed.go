// Package web embeds the static UI bundle into the binary so a single
// container image (or local binary) ships everything.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var static embed.FS

// Static returns the UI bundle rooted at its index.html.
func Static() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err) // embedded path is fixed at compile time
	}
	return sub
}
