// Package htmlscan is a small HTML tokenizer that follows the browser's
// tokenization rules closely enough to find real markup: a "</body>" inside a
// comment, a script, a style or an attribute value is character data, not a
// tag. Every token carries its byte offsets so callers can splice the source
// without re-serializing it, which keeps served artifacts byte-identical to
// the file on disk apart from deliberate insertions.
package htmlscan

import (
	"bytes"
	"html"
	"strings"
)

// Type is a token's kind.
type Type int

const (
	// Text is character data, including the contents of raw-text elements
	// such as script and style (see Token.Raw).
	Text Type = iota
	StartTag
	EndTag
	// Comment covers real comments and bogus ones (<!x>, <?x>, </ x>).
	Comment
	Doctype
)

// Attr is one attribute of a start tag. Offsets index the source.
type Attr struct {
	Name string // lowercased
	// RawValue is the value exactly as written, without quotes; Value
	// decodes character references.
	RawValue string
	HasValue bool
	// ValueStart/ValueEnd bound RawValue in the source (inside the quotes).
	// Both are -1 when the attribute has no value.
	ValueStart, ValueEnd int
}

// Value returns the attribute value with character references decoded.
func (a Attr) Value() string {
	return html.UnescapeString(a.RawValue)
}

// Token is one lexical unit of the source, which spans [Start, End).
type Token struct {
	Type        Type
	Name        string // lowercased tag name for StartTag and EndTag
	Attrs       []Attr
	SelfClosing bool
	// Raw marks a Text token holding the contents of a raw-text element
	// (script, style, textarea, title, ...), where markup is not recognized.
	Raw        bool
	Start, End int
}

// Attr returns the named attribute and whether it is present.
func (t Token) Attr(name string) (Attr, bool) {
	for _, a := range t.Attrs {
		if a.Name == name {
			return a, true
		}
	}
	return Attr{}, false
}

// rawTextElements switch the tokenizer to a mode where only their own end
// tag is markup. noscript is included because browsers run with scripting
// enabled.
var rawTextElements = map[string]bool{
	"script": true, "style": true, "textarea": true, "title": true,
	"xmp": true, "iframe": true, "noembed": true, "noframes": true, "noscript": true,
}

// Tokenize splits src into tokens. Malformed input never fails: like a
// browser, it degrades to text or comments. A tag cut off by the end of
// input produces no token, as in a browser.
func Tokenize(src []byte) []Token {
	z := tokenizer{src: src}
	z.run()
	return z.tokens
}

type tokenizer struct {
	src    []byte
	pos    int
	text   int // start of pending text, or -1
	tokens []Token
}

func (z *tokenizer) emit(t Token) {
	z.tokens = append(z.tokens, t)
}

func (z *tokenizer) flushText(end int) {
	if z.text >= 0 && end > z.text {
		z.emit(Token{Type: Text, Start: z.text, End: end})
	}
	z.text = -1
}

func (z *tokenizer) run() {
	z.text = -1
	n := len(z.src)
	for z.pos < n {
		lt := bytes.IndexByte(z.src[z.pos:], '<')
		if lt < 0 {
			z.startText(z.pos)
			z.pos = n
			break
		}
		start := z.pos + lt
		z.startText(z.pos)
		z.pos = start

		if t, ok := z.markup(start); ok {
			z.flushText(start)
			z.emit(t)
			// A self-closing flag does not stop <script/> from opening raw text.
			if t.Type == StartTag {
				switch {
				case t.Name == "plaintext":
					if z.pos < n {
						z.emit(Token{Type: Text, Raw: true, Start: z.pos, End: n})
					}
					z.pos = n
				case rawTextElements[t.Name]:
					z.rawText(t.Name)
				}
			}
			continue
		}
		// A '<' that does not start markup is text.
		z.startText(start)
		z.pos = start + 1
	}
	z.flushText(n)
}

func (z *tokenizer) startText(at int) {
	if z.text < 0 {
		z.text = at
	}
}

// rawText consumes the contents of a raw-text element up to (not including)
// its end tag.
func (z *tokenizer) rawText(name string) {
	start := z.pos
	end := findEndTag(z.src, start, name)
	if end > start {
		z.emit(Token{Type: Text, Raw: true, Start: start, End: end})
	}
	z.pos = end
}

// findEndTag returns the offset of the first "</name" (any case) that is
// followed by whitespace, '/' or '>', or len(src).
func findEndTag(src []byte, from int, name string) int {
	for i := from; i < len(src); {
		j := bytes.Index(src[i:], []byte("</"))
		if j < 0 {
			return len(src)
		}
		at := i + j
		nameEnd := at + 2 + len(name)
		if nameEnd <= len(src) && strings.EqualFold(string(src[at+2:nameEnd]), name) {
			if nameEnd == len(src) || isTagNameEnd(src[nameEnd]) {
				return at
			}
		}
		i = at + 2
	}
	return len(src)
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r'
}

func isTagNameEnd(c byte) bool {
	return isSpace(c) || c == '/' || c == '>'
}

func isASCIIAlpha(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// markup tries to read a tag, comment or doctype at start (src[start] is
// '<'). It advances z.pos past it on success.
func (z *tokenizer) markup(start int) (Token, bool) {
	src, n := z.src, len(z.src)
	if start+1 >= n {
		return Token{}, false
	}
	c := src[start+1]
	switch {
	case c == '!':
		if bytes.HasPrefix(src[start:], []byte("<!--")) {
			return z.comment(start), true
		}
		if start+9 <= n && strings.EqualFold(string(src[start+2:start+9]), "doctype") {
			end := closeAngle(src, start)
			z.pos = end
			return Token{Type: Doctype, Start: start, End: end}, true
		}
		return z.bogusComment(start), true
	case c == '?':
		return z.bogusComment(start), true
	case c == '/':
		if start+2 >= n {
			return Token{}, false
		}
		if src[start+2] == '>' {
			// "</>" is dropped by browsers; treat it as an empty comment.
			z.pos = start + 3
			return Token{Type: Comment, Start: start, End: start + 3}, true
		}
		if !isASCIIAlpha(src[start+2]) {
			return z.bogusComment(start), true
		}
		return z.tag(start, start+2, EndTag)
	case isASCIIAlpha(c):
		return z.tag(start, start+1, StartTag)
	}
	return Token{}, false
}

// closeAngle returns the offset just past the next '>' after start, or
// len(src).
func closeAngle(src []byte, start int) int {
	if i := bytes.IndexByte(src[start+1:], '>'); i >= 0 {
		return start + 1 + i + 1
	}
	return len(src)
}

func (z *tokenizer) bogusComment(start int) Token {
	end := closeAngle(z.src, start)
	z.pos = end
	return Token{Type: Comment, Start: start, End: end}
}

// comment reads "<!--" ... "-->", including the abrupt "<!-->" and "<!--->"
// forms and the "--!>" closer browsers also accept.
func (z *tokenizer) comment(start int) Token {
	src := z.src
	body := start + 4
	for _, abrupt := range []string{">", "->"} {
		if bytes.HasPrefix(src[body:], []byte(abrupt)) {
			z.pos = body + len(abrupt)
			return Token{Type: Comment, Start: start, End: z.pos}
		}
	}
	end := len(src)
	for i := body; i < len(src); i++ {
		if src[i] != '-' {
			continue
		}
		if bytes.HasPrefix(src[i:], []byte("-->")) {
			end = i + 3
			break
		}
		if bytes.HasPrefix(src[i:], []byte("--!>")) {
			end = i + 4
			break
		}
	}
	z.pos = end
	return Token{Type: Comment, Start: start, End: end}
}

// tag reads a start or end tag whose name begins at nameStart.
func (z *tokenizer) tag(start, nameStart int, typ Type) (Token, bool) {
	src, n := z.src, len(z.src)
	i := nameStart
	for i < n && !isTagNameEnd(src[i]) {
		i++
	}
	t := Token{Type: typ, Name: strings.ToLower(string(src[nameStart:i])), Start: start}

	for {
		for i < n && (isSpace(src[i]) || src[i] == '/') {
			if src[i] == '/' && i+1 < n && src[i+1] == '>' {
				t.SelfClosing = true
				i++
				break
			}
			i++
		}
		if i >= n {
			return Token{}, false
		}
		if src[i] == '>' {
			t.End = i + 1
			z.pos = t.End
			if typ == EndTag {
				// Attributes and self-closing flags on end tags are ignored.
				t.Attrs, t.SelfClosing = nil, false
			}
			return t, true
		}
		t.SelfClosing = false

		// Attribute name; a leading '=' is part of the name.
		nameAt := i
		i++
		for i < n && !isSpace(src[i]) && src[i] != '/' && src[i] != '>' && src[i] != '=' {
			i++
		}
		a := Attr{Name: strings.ToLower(string(src[nameAt:i])), ValueStart: -1, ValueEnd: -1}

		j := i
		for j < n && isSpace(src[j]) {
			j++
		}
		if j < n && src[j] == '=' {
			j++
			for j < n && isSpace(src[j]) {
				j++
			}
			if j >= n {
				return Token{}, false
			}
			switch q := src[j]; q {
			case '"', '\'':
				k := bytes.IndexByte(src[j+1:], q)
				if k < 0 {
					return Token{}, false
				}
				a.ValueStart, a.ValueEnd = j+1, j+1+k
				i = j + 1 + k + 1
			case '>':
				// "a=>" has an empty value; the '>' closes the tag.
				a.ValueStart, a.ValueEnd = j, j
				i = j
			default:
				k := j
				for k < n && !isSpace(src[k]) && src[k] != '>' {
					k++
				}
				a.ValueStart, a.ValueEnd = j, k
				i = k
			}
			a.HasValue = true
			a.RawValue = string(src[a.ValueStart:a.ValueEnd])
		}
		// Browsers keep the first of duplicate attributes.
		if _, dup := t.Attr(a.Name); !dup {
			t.Attrs = append(t.Attrs, a)
		}
	}
}

// BodyEnd returns the offset of the "</body>" end tag a browser would honor,
// or -1 when the document has none. A "</body>" inside template contents is
// ignored, as it is by the HTML parser.
func BodyEnd(src []byte) int {
	templates := 0
	for _, t := range Tokenize(src) {
		switch {
		case t.Type == StartTag && t.Name == "template":
			templates++
		case t.Type == EndTag && t.Name == "template" && templates > 0:
			templates--
		case t.Type == EndTag && t.Name == "body" && templates == 0:
			return t.Start
		}
	}
	return -1
}
