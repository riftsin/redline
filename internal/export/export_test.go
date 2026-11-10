package export

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tree struct {
	t    *testing.T
	root string // artifact directory
	out  string // a directory outside it
}

func newTree(t *testing.T) tree {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tr := tree{t: t, root: filepath.Join(base, "artifact"), out: filepath.Join(base, "outside")}
	for _, d := range []string{tr.root, tr.out} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return tr
}

func (tr tree) write(rel, content string) string {
	tr.t.Helper()
	p := filepath.Join(tr.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		tr.t.Fatal(err)
	}
	return p
}

func data(mime, content string) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString([]byte(content))
}

func (tr tree) bundle(html string, limits Limits) (string, []Warning) {
	tr.t.Helper()
	file := tr.write("plan.html", html)
	out, warnings, err := Bundle(file, limits)
	if err != nil {
		tr.t.Fatal(err)
	}
	return string(out), warnings
}

func kinds(ws []Warning) string {
	var k []string
	for _, w := range ws {
		k = append(k, w.Kind)
	}
	return strings.Join(k, ",")
}

func TestInlinesLocalAssets(t *testing.T) {
	tr := newTree(t)
	tr.write("css/site.css", `@import "base.css" screen; .hero { background: url(../img/bg.png) } @font-face { src: url('fonts/a.woff2') }`)
	tr.write("css/base.css", `body { margin: 0 }`)
	tr.write("css/fonts/a.woff2", "WOFF2")
	tr.write("img/bg.png", "PNGDATA")
	tr.write("img/a.svg", "<svg/>")
	tr.write("app.js", `document.body.append("</script> stays text")`)

	out, warnings := tr.bundle(`<!doctype html>
<link rel="stylesheet" href="css/site.css" media="print">
<link rel="icon" href="img/a.svg">
<style>.x { background: url("img/bg.png") }</style>
<body>
<img src="img/bg.png" srcset="img/bg.png 1x, img/a.svg 2x" alt="x">
<div style="background-image: url(img/bg.png)">styled</div>
<svg><use href="img/a.svg#icon"/></svg>
<script src="app.js" integrity="sha384-x" crossorigin="anonymous"></script>
</body>`, DefaultLimits)

	if len(warnings) != 0 {
		t.Fatalf("warnings: %+v", warnings)
	}
	for _, want := range []string{
		`<style media="print">@media screen {` + "\n" + `body { margin: 0 }` + "\n" + `} .hero { background: url("` + data("image/png", "PNGDATA") + `") } @font-face { src: url("` + data("font/woff2", "WOFF2") + `") }</style>`,
		`<link rel="icon" href="` + data("image/svg+xml", "<svg/>") + `">`,
		`<style>.x { background: url("` + data("image/png", "PNGDATA") + `") }</style>`,
		`<img src="` + data("image/png", "PNGDATA") + `" srcset="` + data("image/png", "PNGDATA") + ` 1x, ` + data("image/svg+xml", "<svg/>") + ` 2x" alt="x">`,
		`style="background-image: url(&#34;` + data("image/png", "PNGDATA") + `&#34;)"`,
		`href="` + data("image/svg+xml", "<svg/>") + `#icon"`,
		`<script>document.body.append("<\/script> stays text")</script>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("export lacks:\n%s\n--- export ---\n%s", want, out)
		}
	}
	if strings.Contains(out, "integrity") || strings.Contains(out, "crossorigin") {
		t.Error("an inlined script must drop integrity and crossorigin")
	}
}

func TestLeavesRemoteAndKeepsBytes(t *testing.T) {
	tr := newTree(t)
	src := `<!DOCTYPE html>
<html lang="en"><head>
  <link   rel='stylesheet'   href="https://cdn.example.com/x.css">
  <script src="//cdn.example.com/x.js"></script>
</head>
<body class="a  b"><a href="#top" data-x='1'>top</a><img src="data:image/png;base64,AAAA"><a href="mailto:a@b.c">m</a></body></html>`
	out, warnings := tr.bundle(src, DefaultLimits)
	if out != src || len(warnings) != 0 {
		t.Fatalf("nothing local, so nothing may change:\n%s\n%+v", out, warnings)
	}
}

func TestDesignFilesAreInlined(t *testing.T) {
	tr := newTree(t)
	out, warnings := tr.bundle(`<link rel="stylesheet" href="/design/daisyui-themes.css"><script src="/design/tailwindcss-browser.js"></script>`, DefaultLimits)
	if len(warnings) != 0 || strings.Contains(out, `href="/design/`) || strings.Contains(out, `src="/design/`) || !strings.HasPrefix(out, "<style>") || !strings.Contains(out, "<script>") {
		t.Fatalf("design files not inlined (%+v):\n%.300s", warnings, out)
	}
}

func TestConfinementAndRedaction(t *testing.T) {
	tr := newTree(t)
	secret := filepath.Join(tr.out, "secret.png")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(tr.root, "link.png")); err != nil {
		t.Fatal(err)
	}
	inside := tr.write("in.png", "INSIDE")

	out, warnings := tr.bundle(`<img src="../outside/secret.png"><img src="link.png"><img src="file://`+secret+`"><img src="file://`+inside+`"><img src="/abs.png">`, DefaultLimits)
	if strings.Contains(out, base64.StdEncoding.EncodeToString([]byte("SECRET"))) {
		t.Fatal("a file outside the artifact's directory was inlined")
	}
	if strings.Contains(out, secret) {
		t.Fatal("a local path outside the artifact leaked into the export")
	}
	for _, want := range []string{`src="about:blank"`, data("image/png", "INSIDE"), `src="/abs.png"`} {
		if !strings.Contains(out, want) {
			t.Errorf("export lacks %q:\n%s", want, out)
		}
	}
	got := kinds(warnings)
	for _, want := range []string{"unresolved-asset", "redacted-file-url"} {
		if !strings.Contains(got, want) {
			t.Errorf("warnings %s lack %s", got, want)
		}
	}
}

func TestCapsAndOtherWarnings(t *testing.T) {
	tr := newTree(t)
	tr.write("big.png", strings.Repeat("x", 200))
	tr.write("a.png", strings.Repeat("a", 60))
	tr.write("b.png", strings.Repeat("b", 60))
	out, warnings := tr.bundle(`<meta http-equiv="Content-Security-Policy" content="default-src 'self'">
<img src="big.png"><img src="a.png"><img src="b.png"><img src="missing.png">
<script src="/static/sdk.js" data-redline-sdk></script>`, Limits{MaxAssetBytes: 100, MaxBundleBytes: 100})
	got := kinds(warnings)
	for _, want := range []string{"author-csp", "asset-too-large", "bundle-too-large", "unresolved-asset"} {
		if !strings.Contains(got, want) {
			t.Errorf("warnings %s lack %s", got, want)
		}
	}
	if !strings.Contains(out, `src="big.png"`) || !strings.Contains(out, data("image/png", strings.Repeat("a", 60))) || !strings.Contains(out, `src="b.png"`) {
		t.Errorf("caps applied wrongly:\n%s", out)
	}
	if strings.Contains(out, "data-redline-sdk") {
		t.Error("the redline SDK must never be exported")
	}
}

func TestFileName(t *testing.T) {
	if got := FileName("/w/plan.HTML"); got != "plan.export.html" {
		t.Fatalf("FileName = %q", got)
	}
}
