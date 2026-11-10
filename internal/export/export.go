// Package export writes a portable copy of an artifact: one HTML file with
// its local assets (stylesheets, scripts, images, fonts, media) inlined, so
// it opens with no server and no sibling files. Remote references (http,
// https, protocol-relative) stay links. Export makes no network requests:
// it reads local files only, confined by real path to the artifact's
// directory, and replaces any other file:// URL with about:blank so local
// paths never leak. The redline SDK is never included.
package export

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"io/fs"
	"mime"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/riftsin/redline/internal/htmlscan"
	"github.com/riftsin/redline/internal/paths"
	"github.com/riftsin/redline/web"
)

// Limits caps what is inlined.
type Limits struct {
	MaxAssetBytes  int64 // REDLINE_EXPORT_MAX_ASSET_BYTES
	MaxBundleBytes int64 // REDLINE_EXPORT_MAX_BUNDLE_BYTES
}

// DefaultLimits are used when the environment says nothing.
var DefaultLimits = Limits{MaxAssetBytes: 10 << 20, MaxBundleBytes: 25 << 20}

// LimitsFromEnv reads the caps from the environment; invalid values keep
// the defaults.
func LimitsFromEnv(getenv func(string) string) Limits {
	l := DefaultLimits
	for name, dst := range map[string]*int64{
		"REDLINE_EXPORT_MAX_ASSET_BYTES":  &l.MaxAssetBytes,
		"REDLINE_EXPORT_MAX_BUNDLE_BYTES": &l.MaxBundleBytes,
	} {
		if n, err := strconv.ParseInt(strings.TrimSpace(getenv(name)), 10, 64); err == nil && n > 0 {
			*dst = n
		}
	}
	return l
}

// Warning names something that could not be made portable.
type Warning struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// FileName is the default export name: plan.html -> plan.export.html.
func FileName(file string) string {
	base := filepath.Base(file)
	return strings.TrimSuffix(base, filepath.Ext(base)) + ".export.html"
}

// Bundle builds the portable copy of the canonical HTML file.
func Bundle(file string, limits Limits) ([]byte, []Warning, error) {
	src, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, err
	}
	b := &bundler{root: filepath.Dir(file), limits: limits, seen: map[string]bool{}}
	out := b.document(src)
	return out, b.warnings, nil
}

type bundler struct {
	root     string
	limits   Limits
	total    int64
	warnings []Warning
	seen     map[string]bool // warnings already reported
}

func (b *bundler) warn(kind, detail string) {
	k := kind + "\x00" + detail
	if b.seen[k] {
		return
	}
	b.seen[k] = true
	b.warnings = append(b.warnings, Warning{Kind: kind, Detail: detail})
}

// -- References -----------------------------------------------------------------

type refKind int

const (
	refKeep   refKind = iota // remote, fragment, data: and other non-file URLs
	refLocal                 // a file under the artifact's directory
	refDesign                // one of redline's /design/ files
	refRedact                // a file:// URL outside the artifact's directory
)

var schemeRef = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

// classify decides what to do with a reference found in a file whose
// directory is base (relative to the artifact root, "." for the root).
func (b *bundler) classify(ref, base string) (refKind, string) {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "", strings.HasPrefix(ref, "#"), strings.HasPrefix(ref, "//"):
		return refKeep, ""
	case strings.HasPrefix(strings.ToLower(ref), "file:"):
		u, err := url.Parse(ref)
		if err != nil {
			return refRedact, ""
		}
		real, err := filepath.EvalSymlinks(filepath.FromSlash(u.Path))
		if err != nil || !paths.Inside(real, b.root) {
			return refRedact, ""
		}
		rel, _ := filepath.Rel(b.root, real)
		return refLocal, filepath.ToSlash(rel)
	case schemeRef.MatchString(ref):
		return refKeep, ""
	case strings.HasPrefix(ref, "/design/"):
		name := strings.TrimPrefix(cutQuery(ref), "/design/")
		if _, ok := web.DesignFiles[name]; ok {
			return refDesign, name
		}
		return refKeep, ""
	case strings.HasPrefix(ref, "/"):
		b.warn("unresolved-asset", ref+" is a root path, which does not resolve for an artifact; use a path relative to the HTML file")
		return refKeep, ""
	}
	p, err := url.PathUnescape(cutQuery(ref))
	if err != nil {
		p = cutQuery(ref)
	}
	return refLocal, path.Clean(path.Join(base, p))
}

func cutQuery(ref string) string {
	if i := strings.IndexAny(ref, "?#"); i >= 0 {
		return ref[:i]
	}
	return ref
}

// read loads a local or design file within the caps.
func (b *bundler) read(kind refKind, rel string) ([]byte, bool) {
	var data []byte
	var err error
	switch kind {
	case refDesign:
		data, err = fs.ReadFile(web.Design, "design/"+rel)
	case refLocal:
		var real string
		real, err = paths.ResolveAsset(b.root, rel)
		if err == nil {
			var info os.FileInfo
			if info, err = os.Stat(real); err == nil {
				switch {
				case !info.Mode().IsRegular():
					err = fs.ErrInvalid
				case info.Size() > b.limits.MaxAssetBytes:
					b.warn("asset-too-large", fmt.Sprintf("%s is larger than %d MB and was left as a reference", rel, b.limits.MaxAssetBytes>>20))
					return nil, false
				default:
					data, err = os.ReadFile(real)
				}
			}
		}
	default:
		return nil, false
	}
	if err != nil {
		b.warn("unresolved-asset", rel+" could not be read and was left as a reference")
		return nil, false
	}
	if b.total+int64(len(data)) > b.limits.MaxBundleBytes {
		b.warn("bundle-too-large", fmt.Sprintf("%s was left as a reference: the export would exceed %d MB", rel, b.limits.MaxBundleBytes>>20))
		return nil, false
	}
	b.total += int64(len(data))
	return data, true
}

var extraTypes = map[string]string{
	".css": "text/css", ".js": "text/javascript", ".mjs": "text/javascript", ".svg": "image/svg+xml",
	".woff": "font/woff", ".woff2": "font/woff2", ".ttf": "font/ttf", ".otf": "font/otf",
	".ico": "image/x-icon", ".webp": "image/webp", ".avif": "image/avif", ".png": "image/png",
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".json": "application/json",
	".mp4": "video/mp4", ".webm": "video/webm", ".mp3": "audio/mpeg", ".wav": "audio/wav", ".ogg": "audio/ogg",
}

func typeOf(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := extraTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return strings.SplitN(t, ";", 2)[0]
	}
	return "application/octet-stream"
}

// dataURI turns a reference into a data: URI, or returns replacement
// ("" means keep the reference as it is).
func (b *bundler) dataURI(ref, base string) string {
	kind, rel := b.classify(ref, base)
	switch kind {
	case refRedact:
		b.warn("redacted-file-url", "a file:// URL outside the artifact's directory was replaced with about:blank")
		return "about:blank"
	case refLocal, refDesign:
		data, ok := b.read(kind, rel)
		if !ok {
			return ""
		}
		mimeType := typeOf(rel)
		if mimeType == "text/css" {
			data = []byte(b.css(string(data), path.Dir(rel), 0))
		}
		frag := ""
		if i := strings.IndexByte(ref, '#'); i >= 0 && mimeType == "image/svg+xml" {
			frag = ref[i:]
		}
		return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data) + frag
	}
	return ""
}

// -- CSS ------------------------------------------------------------------------

var (
	cssURL    = regexp.MustCompile(`url\(\s*(?:"([^"]*)"|'([^']*)'|([^)'"\s]*))\s*\)`)
	cssImport = regexp.MustCompile(`@import\s+(?:url\(\s*)?(?:"([^"]*)"|'([^']*)'|([^)'"\s;]+))\s*\)?\s*([^;]*);`)
)

// css inlines a stylesheet's local imports and url() references, resolved
// against base (the stylesheet's directory, relative to the artifact root).
func (b *bundler) css(src, base string, depth int) string {
	src = cssImport.ReplaceAllStringFunc(src, func(m string) string {
		p := cssImport.FindStringSubmatch(m)
		ref := p[1] + p[2] + p[3]
		kind, rel := b.classify(ref, base)
		if kind != refLocal && kind != refDesign {
			if kind == refRedact {
				b.warn("redacted-file-url", "a file:// URL outside the artifact's directory was removed from an @import")
				return ""
			}
			return m
		}
		if depth > 8 {
			b.warn("unresolved-asset", rel+": @import nested too deeply")
			return m
		}
		data, ok := b.read(kind, rel)
		if !ok {
			return m
		}
		inner := b.css(string(data), path.Dir(rel), depth+1)
		if media := strings.TrimSpace(p[4]); media != "" {
			return "@media " + media + " {\n" + inner + "\n}"
		}
		return inner
	})
	return cssURL.ReplaceAllStringFunc(src, func(m string) string {
		p := cssURL.FindStringSubmatch(m)
		ref := p[1] + p[2] + p[3]
		if uri := b.dataURI(ref, base); uri != "" {
			return `url("` + uri + `")`
		}
		return m
	})
}

// -- HTML -----------------------------------------------------------------------

// URL-bearing attributes, by element.
var urlAttrs = map[string][]string{
	"img":    {"src"},
	"source": {"src"},
	"video":  {"src", "poster"},
	"audio":  {"src"},
	"track":  {"src"},
	"input":  {"src"},
	"embed":  {"src"},
	"object": {"data"},
	"image":  {"href", "xlink:href"},
	"use":    {"href", "xlink:href"},
}

var iconRels = map[string]bool{"icon": true, "shortcut": true, "apple-touch-icon": true, "mask-icon": true}

func (b *bundler) document(src []byte) []byte {
	toks := htmlscan.Tokenize(src)
	var out bytes.Buffer
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		raw := src[t.Start:t.End]
		if t.Type != htmlscan.StartTag {
			if t.Raw && i > 0 && toks[i-1].Type == htmlscan.StartTag && toks[i-1].Name == "style" {
				out.WriteString(escapeEnd(b.css(string(raw), ".", 0), "style"))
				continue
			}
			out.Write(raw)
			continue
		}
		switch t.Name {
		case "script":
			if _, sdk := t.Attr("data-redline-sdk"); sdk {
				i = skipElement(toks, i, "script")
				continue
			}
			if s, ok := t.Attr("src"); ok {
				kind, rel := b.classify(s.Value(), ".")
				if kind == refRedact {
					b.warn("redacted-file-url", "a script's file:// URL outside the artifact's directory was removed")
					out.Write(rewriteAttrs(src, t, map[string]string{"src": "about:blank"}, nil))
					continue
				}
				if data, ok := b.readIf(kind, rel); ok {
					if typ, ok := t.Attr("type"); ok && strings.EqualFold(typ.Value(), "module") && bytes.Contains(data, []byte("import")) {
						b.warn("module-imports", rel+" is a module script; its own imports were not inlined")
					}
					out.Write(rewriteAttrs(src, t, nil, []string{"src", "integrity", "crossorigin"}))
					out.WriteString(escapeEnd(string(data), "script"))
					// Drop the (normally empty) body the src script had.
					if i+1 < len(toks) && toks[i+1].Raw {
						i++
					}
					continue
				}
			}
			out.Write(raw)
		case "link":
			rel := ""
			if r, ok := t.Attr("rel"); ok {
				rel = strings.ToLower(r.Value())
			}
			href, hasHref := t.Attr("href")
			if !hasHref {
				out.Write(raw)
				continue
			}
			if strings.Contains(rel, "stylesheet") {
				kind, file := b.classify(href.Value(), ".")
				if kind == refRedact {
					b.warn("redacted-file-url", "a stylesheet's file:// URL outside the artifact's directory was removed")
					continue
				}
				if data, ok := b.readIf(kind, file); ok {
					out.WriteString("<style")
					if m, ok := t.Attr("media"); ok {
						out.WriteString(` media="` + html.EscapeString(m.Value()) + `"`)
					}
					out.WriteString(">")
					out.WriteString(escapeEnd(b.css(string(data), path.Dir(file), 0), "style"))
					out.WriteString("</style>")
					continue
				}
				out.Write(raw)
				continue
			}
			isIcon := false
			for _, word := range strings.Fields(rel) {
				isIcon = isIcon || iconRels[word]
			}
			if isIcon || strings.Contains(rel, "preload") {
				if uri := b.dataURI(href.Value(), "."); uri != "" {
					out.Write(rewriteAttrs(src, t, map[string]string{"href": uri}, nil))
					continue
				}
			}
			out.Write(raw)
		case "meta":
			if h, ok := t.Attr("http-equiv"); ok && strings.EqualFold(h.Value(), "content-security-policy") {
				b.warn("author-csp", "the artifact sets a Content-Security-Policy, which may block the inlined styles or scripts")
			}
			out.Write(raw)
		default:
			out.Write(b.rewriteElement(src, t))
		}
	}
	return out.Bytes()
}

// readIf reads a local or design reference.
func (b *bundler) readIf(kind refKind, rel string) ([]byte, bool) {
	if kind != refLocal && kind != refDesign {
		return nil, false
	}
	return b.read(kind, rel)
}

// rewriteElement inlines an element's URL attributes, srcset candidates and
// style attribute.
func (b *bundler) rewriteElement(src []byte, t htmlscan.Token) []byte {
	set := map[string]string{}
	for _, name := range urlAttrs[t.Name] {
		if a, ok := t.Attr(name); ok {
			if uri := b.dataURI(a.Value(), "."); uri != "" {
				set[name] = uri
			}
		}
	}
	if a, ok := t.Attr("srcset"); ok && (t.Name == "img" || t.Name == "source") {
		if v, changed := b.srcset(a.Value()); changed {
			set["srcset"] = v
		}
	}
	if a, ok := t.Attr("style"); ok {
		if v := b.css(a.Value(), ".", 0); v != a.Value() {
			set["style"] = v
		}
	}
	if len(set) == 0 {
		return src[t.Start:t.End]
	}
	return rewriteAttrs(src, t, set, nil)
}

func (b *bundler) srcset(value string) (string, bool) {
	parts := strings.Split(value, ",")
	changed := false
	for i, part := range parts {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		if uri := b.dataURI(fields[0], "."); uri != "" {
			fields[0] = uri
			changed = true
		}
		parts[i] = strings.Join(fields, " ")
	}
	return strings.Join(parts, ", "), changed
}

// rewriteAttrs re-emits a start tag with some attribute values replaced and
// some attributes dropped. Untouched attributes are copied byte for byte.
func rewriteAttrs(src []byte, t htmlscan.Token, set map[string]string, drop []string) []byte {
	var out bytes.Buffer
	pos := t.Start
	for _, a := range t.Attrs {
		dropped := false
		for _, d := range drop {
			dropped = dropped || a.Name == d
		}
		value, replace := set[a.Name]
		if !dropped && !replace {
			continue
		}
		start, end := attrSpan(src, t, a)
		if dropped && !replace {
			// Take the whitespace before a removed attribute with it.
			for start > pos && isSpace(src[start-1]) {
				start--
			}
		}
		out.Write(src[pos:start])
		if replace {
			out.WriteString(a.Name + `="` + html.EscapeString(value) + `"`)
		}
		pos = end
	}
	out.Write(src[pos:t.End])
	return out.Bytes()
}

// attrSpan finds an attribute's full extent (name through closing quote).
func attrSpan(src []byte, t htmlscan.Token, a htmlscan.Attr) (int, int) {
	end := a.ValueEnd
	if a.HasValue && end < t.End && (src[end] == '"' || src[end] == '\'') {
		end++
	}
	// Walk back from the value (or search forward for a valueless name).
	from := t.Start
	if a.HasValue {
		from = a.ValueStart
	}
	lower := strings.ToLower(string(src[t.Start:from]))
	start := strings.LastIndex(lower, a.Name)
	if start < 0 {
		return from, from
	}
	start += t.Start
	if !a.HasValue {
		end = start + len(a.Name)
	}
	return start, end
}

// escapeEnd keeps inlined text from closing its raw-text element early.
func escapeEnd(text, tag string) string {
	re := regexp.MustCompile(`(?i)</(` + tag + `)`)
	return re.ReplaceAllString(text, `<\/$1`)
}

func skipElement(toks []htmlscan.Token, i int, name string) int {
	for j := i + 1; j < len(toks); j++ {
		if toks[j].Type == htmlscan.EndTag && toks[j].Name == name {
			return j
		}
	}
	return i
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}
