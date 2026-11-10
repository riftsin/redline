//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/riftsin/redline/internal/store"
)

const tableExtra = `<table id="bench"><thead><tr><th>Store</th><th>Latency</th></tr></thead>
<tbody><tr><td>Redis</td><td id="redis-latency">12ms</td></tr><tr><td>Postgres</td><td>30ms</td></tr></tbody></table>`

func TestTableCellNamesRowAndColumn(t *testing.T) {
	r := newReview(t, "")
	writeArtifact(t, r.file, "", tableExtra)
	r.page.WaitInFrame("/artifact/", "document.getElementById('redis-latency') !== null")
	r.waitFrameReady()
	feedback := r.poll()

	r.page.ClickInFrame("/artifact/", "#redis-latency")
	r.page.WaitFor(cardOpen)
	if title := r.text("#card-title"); title != "Annotate cell: Redis → Latency" {
		t.Fatalf("card title = %q", title)
	}
	r.page.InsertText("Is this p50 or p99?")
	r.page.Press("Enter", ModCtrl)
	fb := awaitFeedback(t, feedback)
	var target struct{ Type, RowLabel, ColumnLabel string }
	if err := json.Unmarshal(fb.Prompts[0].Target, &target); err != nil {
		t.Fatal(err)
	}
	if target.Type != "table-cell" || target.RowLabel != "Redis" || target.ColumnLabel != "Latency" {
		t.Fatalf("target = %+v", target)
	}
	r.page.WaitFor(`document.querySelector('.bubble.user .anchor-label') && document.querySelector('.bubble.user .anchor-label').textContent === 'cell'`)
}

func TestEditQueuedNoteInPlace(t *testing.T) {
	r := newReview(t, "")
	r.page.ClickInFrame("/artifact/", "#title")
	r.page.WaitFor(cardOpen)
	r.page.InsertText("Bigger")
	r.page.Press("Enter", 0)
	r.page.WaitFor(`document.querySelector('.queued-bubble [data-edit]')`)

	r.page.Click(".queued-bubble [data-edit]")
	r.page.WaitFor(`document.activeElement && document.activeElement.classList.contains('edit-note')`)
	r.page.Eval(`document.activeElement.select()`, nil)
	r.page.InsertText("Bigger and bold")
	r.page.Press("Enter", 0)
	r.page.WaitFor(`document.querySelector('.queued-bubble [data-prompt]') && document.querySelector('.queued-bubble [data-prompt]').textContent === 'Bigger and bold'`)
	q, _ := r.m.Store.Queue(context.Background(), r.key)
	if len(q) != 1 || q[0].Prompt != "Bigger and bold" || q[0].Selector != "h1#title" {
		t.Fatalf("queue = %+v", q)
	}

	// Escape discards an edit.
	r.page.Click(".queued-bubble [data-edit]")
	r.page.WaitFor(`document.activeElement && document.activeElement.classList.contains('edit-note')`)
	r.page.InsertText(" - never mind")
	r.page.Press("Escape", 0)
	r.page.WaitFor(`!document.querySelector('.edit-note') && document.querySelector('.queued-bubble [data-prompt]').textContent === 'Bigger and bold'`)
}

func TestDeliveryReceiptsAndChanges(t *testing.T) {
	r := newReview(t, "")
	ctx := context.Background()

	// Sent with nobody listening: the receipt says why it is not Seen.
	r.page.Click("#composer-input")
	r.page.InsertText("Shorten the intro")
	r.page.Click("#send")
	r.page.WaitFor(`document.querySelector('.receipt-pending') && document.querySelector('.receipt-pending').textContent === 'No agent is listening'`)

	feedback := r.poll()
	awaitFeedback(t, feedback)
	const steps = `Array.from(document.querySelectorAll('.bubble.user .receipt .step.done')).map((s) => s.textContent).join('')`
	r.page.WaitFor(steps + ` === '👀'`)

	// The agent edits the file: Working, and the page shows what changed.
	html := strings.Replace(strings.Replace(artifactHTML, "%s", "", 1),
		"The rollout starts with internal users and widens weekly.", "Rollout widens weekly.", 1)
	if err := os.WriteFile(r.file, []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	r.page.WaitFor(steps + ` === '👀🛠️'`)
	r.page.WaitFor(`!document.getElementById('changes-chip').hidden && document.getElementById('changes-label').textContent === '1 change'`)
	waitChange(t, r, 1, []string{"p#intro"}, false)

	// The reply: Done, rendered Markdown, and a link to what changed.
	if err := r.m.Reply(ctx, r.key, "Shortened:\n- **intro** is one line now"); err != nil {
		t.Fatal(err)
	}
	r.page.WaitFor(steps + ` === '👀🛠️✅'`)
	r.page.WaitFor(`document.querySelector('.bubble.agent .markdown li strong') && document.querySelector('.bubble.agent .markdown li strong').textContent === 'intro'`)
	r.page.WaitFor(`document.querySelector('.bubble.agent .changes-link') && document.querySelector('.bubble.agent .changes-link').textContent === '1 change'`)

	// Reformatting alone changes nothing.
	reformatted := strings.ReplaceAll(strings.ReplaceAll(html, "\n", "\n    "), `<h1 id="title">`, `<h1   id="title" >`)
	if err := os.WriteFile(r.file, []byte(reformatted), 0o644); err != nil {
		t.Fatal(err)
	}
	waitChange(t, r, 2, nil, false)
	r.page.WaitFor(`document.getElementById('changes-chip').hidden`)
}

// waitChange waits until the server recorded a revision's change.
func waitChange(t *testing.T, r *reviewEnv, revision int, selectors []string, rewrite bool) {
	t.Helper()
	deadline := time.Now().Add(DefaultTimeout)
	for {
		changes, err := r.m.Store.Changes(context.Background(), r.key)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range changes {
			if c.Revision == revision {
				if strings.Join(c.Selectors, ",") != strings.Join(selectors, ",") || c.Rewrite != rewrite {
					t.Fatalf("revision %d changed %+v, want %v (rewrite %v)", revision, c, selectors, rewrite)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no change recorded for revision %d", revision)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestWholePageRewrite(t *testing.T) {
	r := newReview(t, "")
	var b strings.Builder
	b.WriteString("<!doctype html><title>New</title><body>")
	for i := 0; i < 12; i++ {
		b.WriteString("<section><h2>Totally new section</h2><p>Different words entirely.</p></section>")
	}
	b.WriteString("</body>")
	if err := os.WriteFile(r.file, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	waitChange(t, r, 1, nil, true)
	r.page.WaitFor(`document.getElementById('changes-label').textContent === 'Page rewritten'`)
}

func TestAttachImagesToMessage(t *testing.T) {
	r := newReview(t, "")
	feedback := r.poll()

	img := image.NewRGBA(image.Rect(0, 0, 32, 24))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	shot := filepath.Join(t.TempDir(), "screenshot.png")
	if err := os.WriteFile(shot, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	notImage := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(notImage, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	r.page.SetInputFiles("input[data-attach=composer]", shot, notImage)
	r.page.WaitFor(`document.querySelectorAll('[data-chips=composer] .chip').length === 2`)
	r.page.WaitFor(`document.querySelector('[data-chips=composer] .chip-error') && !Array.from(document.querySelectorAll('[data-chips=composer] .chip-status')).some((s) => s.textContent === 'Uploading…')`)
	r.page.Click("[data-chips=composer] .chip-error .chip-remove")
	r.page.WaitFor(`document.querySelectorAll('[data-chips=composer] .chip').length === 1`)

	r.page.Click("#composer-input")
	r.page.InsertText("See the screenshot")
	r.page.Click("#send")
	fb := awaitFeedback(t, feedback)
	if len(fb.Prompts) != 1 || len(fb.Prompts[0].Attachments) != 1 {
		t.Fatalf("prompts = %+v", fb.Prompts)
	}
	a := fb.Prompts[0].Attachments[0]
	if a.Name != "screenshot.png" || a.Width != 32 || a.Height != 24 || a.Mime != "image/png" {
		t.Fatalf("attachment = %+v", a)
	}
	if data, err := os.ReadFile(a.Path); err != nil || !bytes.Equal(data, buf.Bytes()) {
		t.Fatalf("the agent cannot read the image at %s: %v", a.Path, err)
	}
	r.page.WaitFor(`document.querySelector('.bubble.user .thumb img') && document.querySelector('.bubble.user .thumb img').naturalWidth === 32`)
	r.page.WaitFor(`document.querySelectorAll('[data-chips=composer] .chip').length === 0`)
}

func TestCollapsibleSidebar(t *testing.T) {
	r := newReview(t, "")
	r.page.Click("#sidebar-toggle")
	r.page.WaitFor(`getComputedStyle(document.getElementById('panel')).display === 'none'`)

	// Annotating opens it again so the note card is visible.
	r.page.ClickInFrame("/artifact/", "#title")
	r.page.WaitFor(cardOpen + ` && getComputedStyle(document.getElementById('panel')).display !== 'none'`)
	r.page.InsertText("Bigger")
	r.page.Press("Enter", 0)

	r.page.Click("#sidebar-toggle")
	r.page.WaitFor(`!document.getElementById('sidebar-badge').hidden && document.getElementById('sidebar-badge').textContent === '1'`)
	if err := r.m.Store.RemoveQueued(context.Background(), r.key, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Reply(context.Background(), r.key, "Noted."); err != nil {
		t.Fatal(err)
	}
	r.page.WaitFor(`document.getElementById('sidebar-badge').classList.contains('is-dot')`)
}

func TestTabTitleAndMeta(t *testing.T) {
	r := newReview(t, "")
	r.page.WaitFor(`document.title === 'Plan · redline'`)
	r.page.EvalInFrame("/artifact/", `document.title`, nil)
	// The artifact's own script updates the tab (it runs in the page's world).
	// The SDK is injected at the end of the body, so the artifact calls it
	// once the page has loaded.
	writeArtifact(t, r.file, "", `<script>addEventListener("load", () => window.redline?.setDocumentMeta({title: "3 tests failing"}));</script>`)
	r.page.WaitFor(`document.title === '3 tests failing · redline'`)
}

var _ = store.ReceiptLive

func TestLocalDesignSnippetStylesTheArtifact(t *testing.T) {
	r := newReview(t, "")
	html := `<!doctype html><html data-theme="coffee"><head>
<link rel="stylesheet" href="/design/daisyui.css">
<link rel="stylesheet" href="/design/daisyui-themes.css">
<script src="/design/tailwindcss-browser.js"></script>
</head><body class="bg-base-100 p-6"><button id="go" class="btn btn-primary">Go</button></body></html>`
	if err := os.WriteFile(r.file, []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	// DaisyUI styles the button and the Tailwind runtime generates p-6.
	r.page.WaitInFrame("/artifact/", `document.getElementById('go') && getComputedStyle(document.getElementById('go')).borderRadius !== '0px' && getComputedStyle(document.body).paddingTop === '24px'`)
}
