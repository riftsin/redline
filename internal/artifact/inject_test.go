package artifact

import "testing"

func TestInjectSDK(t *testing.T) {
	const tag = `<script src="/sdk.js"></script>`
	cases := map[string]string{
		"<body><p>x</p></body></html>":                 `<body><p>x</p>` + tag + `</body></html>`,
		"<body><!-- </body> --><p>x</p></body>":        `<body><!-- </body> --><p>x</p>` + tag + `</body>`,
		"<p>no body end tag</p>\n":                     "<p>no body end tag</p>\n" + tag,
		"<p>no newline</p>":                            "<p>no newline</p>\n" + tag,
		"<body><script>'</body>'</script></body>":      `<body><script>'</body>'</script>` + tag + `</body>`,
		"\uFEFF<!doctype html><body>é</body>":          "\uFEFF<!doctype html><body>é" + tag + "</body>",
		"<body>\r\n<p>crlf kept</p>\r\n</body>\r\n":    "<body>\r\n<p>crlf kept</p>\r\n" + tag + "</body>\r\n",
		"<body><template></body></template>end</body>": `<body><template></body></template>end` + tag + `</body>`,
	}
	for in, want := range cases {
		if got := string(InjectSDK([]byte(in), tag)); got != want {
			t.Errorf("InjectSDK(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestWantsFolderReload(t *testing.T) {
	for src, want := range map[string]bool{
		`<html data-redline-live-reload-root><body>`:                    true,
		`<head><meta name="redline-live-reload" content="root"></head>`: true,
		`<meta name="Redline-Live-Reload" content=" ROOT ">`:            true,
		`<html><body data-redline-live-reload-root>`:                    false,
		`<!-- <html data-redline-live-reload-root> --><html>`:           false,
		`<meta name="redline-live-reload" content="file">`:              false,
	} {
		if got := WantsFolderReload([]byte(src)); got != want {
			t.Errorf("WantsFolderReload(%q) = %v, want %v", src, got, want)
		}
	}
}
