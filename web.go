// Package collab embeds the browser client.
package collab

import (
	"embed"
	"io/fs"
)

//go:embed web
var webFiles embed.FS

// WebFS returns the static client files.
func WebFS() fs.FS {
	sub, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	return sub
}
