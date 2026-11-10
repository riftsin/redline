// Package artifact serves a reviewed HTML file: its bytes on disk plus the
// one SDK script tag, and nothing else.
package artifact

import (
	"bytes"
	"strings"

	"github.com/riftsin/redline/internal/htmlscan"
)

// InjectSDK splices script immediately before the "</body>" end tag a browser
// would honor, or appends it on a new line when there is none. Every other
// byte of src is preserved.
func InjectSDK(src []byte, script string) []byte {
	out := make([]byte, 0, len(src)+len(script)+1)
	if at := htmlscan.BodyEnd(src); at >= 0 {
		out = append(out, src[:at]...)
		out = append(out, script...)
		return append(out, src[at:]...)
	}
	out = append(out, src...)
	if !bytes.HasSuffix(src, []byte("\n")) {
		out = append(out, '\n')
	}
	return append(out, script...)
}

// WantsFolderReload reports whether the artifact opts into live reload on
// changes to any file in its folder: data-redline-live-reload-root on its
// root element, or <meta name="redline-live-reload" content="root">.
func WantsFolderReload(src []byte) bool {
	for _, t := range htmlscan.Tokenize(src) {
		if t.Type != htmlscan.StartTag {
			continue
		}
		switch t.Name {
		case "html":
			if _, ok := t.Attr("data-redline-live-reload-root"); ok {
				return true
			}
		case "meta":
			n, ok := t.Attr("name")
			c, ok2 := t.Attr("content")
			if ok && ok2 && strings.EqualFold(n.Value(), "redline-live-reload") && strings.EqualFold(strings.TrimSpace(c.Value()), "root") {
				return true
			}
		}
	}
	return false
}
