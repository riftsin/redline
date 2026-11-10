// Package artifact serves a reviewed HTML file: its bytes on disk plus the
// one SDK script tag, and nothing else.
package artifact

import (
	"bytes"

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
