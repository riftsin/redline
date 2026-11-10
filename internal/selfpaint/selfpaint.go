// Package selfpaint is a render-free check for the one authoring failure
// that makes a review silently unusable: an artifact that never paints its
// own page background, so its text renders over whatever surface hosts it
// and can be invisible. The check fails open - any stylesheet link,
// @import, Tailwind runtime script or root paint signal counts as painted -
// because a wrong warning on every open is worse than a miss. It stays a
// warning: it never blocks an open and never repairs anything.
//
// Adapted from lavish-axi (MIT, Copyright (c) 2026 Kun Chen).
package selfpaint

import (
	"regexp"
	"strings"

	"github.com/riftsin/redline/internal/htmlscan"
)

// Warning is shown when Painted reports false.
const Warning = "This artifact never paints its own page surface: no background on html/body/:root, no bg-* class or data-theme on html/body, and no stylesheet that could set one. redline injects no design system, so text that assumes a dark or light host surface can render invisible. Set an explicit background and readable text."

var (
	cssComment   = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssRule      = regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
	rootSelector = regexp.MustCompile(`(?i)(^|[\s,>~+])(html|body|:root|\*)([^\w-]|$)`)
	bgClass      = regexp.MustCompile(`(?i)(^|[^\w-])bg-`)
)

// Painted reports whether the document paints its own page surface, and
// the first signal that says so.
func Painted(src []byte) (bool, string) {
	for _, t := range htmlscan.Tokenize(src) {
		switch {
		case t.Type == htmlscan.StartTag && t.Name == "link":
			if rel, ok := t.Attr("rel"); ok && strings.Contains(strings.ToLower(rel.Value()), "stylesheet") {
				return true, "stylesheet-link"
			}
		case t.Type == htmlscan.StartTag && t.Name == "script":
			if s, ok := t.Attr("src"); ok && strings.Contains(strings.ToLower(s.Value()), "tailwind") {
				return true, "tailwind-runtime"
			}
		case t.Type == htmlscan.StartTag && t.Name == "meta":
			if n, ok := t.Attr("name"); ok && strings.EqualFold(n.Value(), "color-scheme") {
				return true, "color-scheme"
			}
		case t.Type == htmlscan.StartTag && (t.Name == "html" || t.Name == "body"):
			if _, ok := t.Attr("data-theme"); ok {
				return true, "data-theme"
			}
			if c, ok := t.Attr("class"); ok && bgClass.MatchString(c.Value()) {
				return true, "background-class"
			}
			if s, ok := t.Attr("style"); ok {
				style := strings.ToLower(s.Value())
				if strings.Contains(style, "background") {
					return true, "inline-background"
				}
				if strings.Contains(style, "color-scheme") {
					return true, "color-scheme"
				}
			}
			if _, ok := t.Attr("bgcolor"); ok && t.Name == "body" {
				return true, "inline-background"
			}
		}
	}
	// Style blocks: the raw text inside <style>.
	toks := htmlscan.Tokenize(src)
	for i, t := range toks {
		if t.Type != htmlscan.StartTag || t.Name != "style" || i+1 >= len(toks) || !toks[i+1].Raw {
			continue
		}
		css := cssComment.ReplaceAllString(string(src[toks[i+1].Start:toks[i+1].End]), "")
		lower := strings.ToLower(css)
		if strings.Contains(lower, "@import") {
			return true, "css-import"
		}
		if strings.Contains(lower, "color-scheme") {
			return true, "color-scheme"
		}
		for _, m := range cssRule.FindAllStringSubmatch(css, -1) {
			if strings.Contains(strings.ToLower(m[2]), "background") && rootSelector.MatchString(m[1]) {
				return true, "root-background-rule"
			}
		}
	}
	return false, ""
}
