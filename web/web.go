// Package web holds redline's browser files: the review page templates, the
// hand-written JavaScript and CSS, and the vendored design files artifacts
// may opt into. Everything is embedded into the binary and served exactly
// as written.
package web

import "embed"

// Static is served under /static/: app.js, app.css and the artifact SDK.
//
//go:embed static
var Static embed.FS

// Templates are the review page and its server-rendered regions.
//
//go:embed templates
var Templates embed.FS

// Design holds the vendored, unmodified Tailwind CSS browser runtime and
// DaisyUI stylesheets, served under /design/ to artifacts that opt in.
//
//go:embed design
var Design embed.FS

// DesignFiles lists the files served under /design/.
var DesignFiles = map[string]string{
	"tailwindcss-browser.js": "text/javascript; charset=utf-8",
	"daisyui.css":            "text/css; charset=utf-8",
	"daisyui-themes.css":     "text/css; charset=utf-8",
}
