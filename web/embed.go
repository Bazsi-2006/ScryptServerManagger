// Package web embeds the frontend so the application ships as one binary.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:static
var static embed.FS

// FS returns the frontend files rooted at the site root.
func FS() (fs.FS, error) { return fs.Sub(static, "static") }
