// Package web holds redline's browser files: the review page templates and
// the hand-written JavaScript and CSS, embedded into the binary and served
// exactly as written.
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
