// Package web holds the frontend. Everything is embedded into the binary.
package web

import "embed"

// Files contains index.html (a Go html/template) and the static assets.
//
//go:embed index.html static
var Files embed.FS
