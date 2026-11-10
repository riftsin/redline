package selfpaint

import "testing"

func TestPainted(t *testing.T) {
	painted := map[string]string{
		`<link rel="stylesheet" href="a.css">`:                                   "stylesheet-link",
		`<script src="/design/tailwindcss-browser.js"></script>`:                 "tailwind-runtime",
		`<meta name="color-scheme" content="light dark">`:                        "color-scheme",
		`<html data-theme="coffee"><body>x`:                                      "data-theme",
		`<body class="min-h-screen bg-base-100">`:                                "background-class",
		`<body style="background:#fff">`:                                         "inline-background",
		`<style>/* body{} */ :root { --x: 1 } body { background: #111 }</style>`: "root-background-rule",
		`<style>html, .x { background-color: white }</style>`:                    "root-background-rule",
		`<style>@import url(theme.css);</style>`:                                 "css-import",
		`<style>:root{color-scheme:light dark}</style>`:                          "color-scheme",
	}
	for src, want := range painted {
		if ok, signal := Painted([]byte(src)); !ok || signal != want {
			t.Errorf("Painted(%q) = %v %q, want %q", src, ok, signal, want)
		}
	}
	unpainted := []string{
		`<!doctype html><body><h1>Plain</h1></body>`,
		`<style>.card { background: #222 } body { color: white }</style>`,
		`<!-- <link rel="stylesheet" href="x.css"> --><body>`,
		`<style>/* body { background: red } */</style>`,
		`<p class="bg-red-500">not the root</p>`,
	}
	for _, src := range unpainted {
		if ok, signal := Painted([]byte(src)); ok {
			t.Errorf("Painted(%q) = true (%s), want false", src, signal)
		}
	}
}
