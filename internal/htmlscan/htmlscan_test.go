package htmlscan

import (
	"strings"
	"testing"
)

func TestBodyEnd(t *testing.T) {
	cases := []struct {
		name, src string
		// want marks the expected offset with "|"; no marker means -1.
	}{
		{"plain", `<html><body><p>hi</p>|</body></html>`},
		{"uppercase and spaces", `<BODY>x|</BODY  >`},
		{"attributes on the end tag", `<body>x|</body class="y">`},
		{"no body end tag", `<p>hi</p>`},
		{"in a comment", `<body><!-- </body> -->x|</body>`},
		{"in an abrupt comment", `<body><!-->x|</body>`},
		{"in a script", `<body><script>document.write("</body>")</script>|</body>`},
		{"in a script with a nearly matching end", `<body><script>"</scripts></body>"</script>|</body>`},
		{"in a style", `<body><style>/* </body> */</style>|</body>`},
		{"in a textarea", `<body><textarea></body></textarea>|</body>`},
		{"in a title", `<head><title></body></title></head><body>|</body>`},
		{"in an attribute", `<body><a title="</body>">x</a>|</body>`},
		{"in a single-quoted attribute", `<body><a title='</body>'>x</a>|</body>`},
		{"in a template", `<body><template></body></template>|</body>`},
		{"in nested templates", `<body><template><template></template></body></template>|</body>`},
		{"self-closing script still raw", `<body><script/></body></script>|</body>`},
		{"in plaintext", `<body><plaintext></body>`},
		{"in a bogus comment", `<body><!x </body> y>|</body>`},
		{"in a processing instruction", `<body><?x </body>|</body>`},
		{"first of two", `<body>a|</body>b</body>`},
		{"before body start", `<head>|</body><p>x`},
		{"unterminated tag", `<body>x</body`},
	}
	for _, c := range cases {
		want := strings.Index(c.src, "|")
		src := strings.Replace(c.src, "|", "", 1)
		if got := BodyEnd([]byte(src)); got != want {
			t.Errorf("%s: BodyEnd(%q) = %d, want %d", c.name, src, got, want)
		}
	}
}

func TestTokenizeAttributes(t *testing.T) {
	src := `<img src="a.png" alt='x &amp; y' data-x=bare checked DATA-X="dup" =odd>`
	toks := Tokenize([]byte(src))
	if len(toks) != 1 || toks[0].Type != StartTag || toks[0].Name != "img" {
		t.Fatalf("tokens = %+v", toks)
	}
	tok := toks[0]

	want := []struct {
		name, raw string
		has       bool
	}{
		{"src", "a.png", true},
		{"alt", "x &amp; y", true},
		{"data-x", "bare", true},
		{"checked", "", false},
		{"=odd", "", false},
	}
	if len(tok.Attrs) != len(want) {
		t.Fatalf("attrs = %+v", tok.Attrs)
	}
	for i, w := range want {
		a := tok.Attrs[i]
		if a.Name != w.name || a.RawValue != w.raw || a.HasValue != w.has {
			t.Errorf("attr %d = %+v, want %+v", i, a, w)
		}
		if a.HasValue && src[a.ValueStart:a.ValueEnd] != a.RawValue {
			t.Errorf("attr %s offsets [%d,%d) do not bound its raw value", a.Name, a.ValueStart, a.ValueEnd)
		}
	}
	if alt, _ := tok.Attr("alt"); alt.Value() != "x & y" {
		t.Errorf("alt decoded = %q", alt.Value())
	}
}

func TestTokenizeCoversSource(t *testing.T) {
	src := `<!doctype html><html><head><style>p{}</style></head><body a=1>t<!--c--><br/>x < y</body></html>`
	toks := Tokenize([]byte(src))
	at := 0
	for _, tok := range toks {
		if tok.Start != at {
			t.Fatalf("gap or overlap before %+v (expected start %d)", tok, at)
		}
		at = tok.End
	}
	if at != len(src) {
		t.Fatalf("tokens end at %d, source is %d bytes", at, len(src))
	}

	var kinds []string
	for _, tok := range toks {
		kinds = append(kinds, map[Type]string{Text: "text", StartTag: "<", EndTag: "</", Comment: "comment", Doctype: "doctype"}[tok.Type]+tok.Name)
	}
	got := strings.Join(kinds, " ")
	want := "doctype <html <head <style text </style </head <body text comment <br text </body </html"
	if got != want {
		t.Fatalf("token kinds:\n got %s\nwant %s", got, want)
	}
}
