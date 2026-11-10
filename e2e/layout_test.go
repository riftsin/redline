//go:build e2e

package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

const clippedPage = `<!doctype html><html><head><style>
body { margin: 0; padding: 24px; font: 16px/1.5 sans-serif; background: #fff; color: #111 }
.card { width: 160px; height: 40px; overflow: hidden; border: 1px solid #ccc }
.card h2 { margin: 0; white-space: nowrap; font-size: 28px }
</style></head><body>
<div class="card" id="card"><h2>A heading far too long for its card</h2></div>
<p>Everything else is fine.</p>
</body></html>`

const fixedPage = `<!doctype html><html><head><style>
body { margin: 0; padding: 24px; font: 16px/1.5 sans-serif; background: #fff; color: #111 }
.card { width: 160px; border: 1px solid #ccc }
.card h2 { margin: 0; font-size: 18px }
</style></head><body>
<div class="card" id="card"><h2>A heading far too long for its card</h2></div>
<p>Everything else is fine.</p>
</body></html>`

func TestLayoutInboxLifecycle(t *testing.T) {
	r := newReview(t, "")
	if err := os.WriteFile(r.file, []byte(clippedPage), 0o644); err != nil {
		t.Fatal(err)
	}

	// The audit finds the clipped heading; nothing wakes the agent.
	r.page.WaitFor(`!document.getElementById('layout-button').hidden && document.querySelector('.layout-count').textContent === '1'`)
	if p := r.m.Presence(r.key); p.State != "idle" {
		t.Fatalf("detection must not involve the agent: %+v", p)
	}
	r.page.Click("#layout-button")
	r.page.WaitFor(`getComputedStyle(document.getElementById('layout-drawer')).display === 'block'`)
	if title := r.text(".layout-item .layout-title"); !strings.Contains(title, "Text cut off by its container") {
		t.Fatalf("issue title = %q", title)
	}
	// The audit measures the artifact frame (1280px minus the conversation panel).
	if meta := r.text(".layout-item .layout-meta"); !strings.Contains(meta, "Tablet / compact · 896px") || !strings.Contains(meta, "#card") {
		t.Fatalf("issue meta = %q", meta)
	}

	// Queue the fix: it becomes an ordinary queued note.
	feedback := r.poll()
	r.page.Click(".layout-item .layout-check")
	r.page.WaitFor(`!document.querySelector('[data-layout-queue]').disabled`)
	r.page.Click("[data-layout-queue]")
	r.page.WaitFor(`document.querySelector('.queued-bubble .anchor-label') && document.querySelector('.queued-bubble .anchor-label').textContent === 'layout issues'`)
	r.page.WaitFor(`document.querySelector('.layout-item .layout-status').textContent === 'Queued for fix' && !document.querySelector('.layout-item .layout-check')`)
	r.page.Click("#send")
	fb := awaitFeedback(t, feedback)
	if len(fb.Prompts) != 1 || fb.Prompts[0].Kind != "layout" || !strings.Contains(fb.Prompts[0].Prompt, "Text cut off by its container") || !strings.Contains(string(fb.Prompts[0].Target), `"layout-warnings"`) {
		t.Fatalf("prompt = %+v", fb.Prompts)
	}

	// The agent fixes the file: a newer revision with a clean check resolves it.
	if err := os.WriteFile(r.file, []byte(fixedPage), 0o644); err != nil {
		t.Fatal(err)
	}
	r.page.WaitFor(`document.getElementById('layout-button').hidden`)
	ws, _ := r.m.Store.LayoutWarnings(context.Background(), r.key)
	if len(ws) != 1 || ws[0].Status != "resolved" {
		t.Fatalf("warnings = %+v", ws)
	}
}

func TestIntentionalClippingIsNotReported(t *testing.T) {
	r := newReview(t, "")
	page := `<!doctype html><html><head><style>
body { margin: 0; padding: 24px; font: 16px/1.5 sans-serif; background: #fff; color: #111 }
.ellipsis { width: 120px; overflow: hidden; white-space: nowrap; text-overflow: ellipsis }
.scroller { width: 160px; overflow-x: auto; white-space: nowrap }
.sr-only { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap }
.rounded { width: 120px; overflow: hidden; border-radius: 12px; white-space: nowrap }
</style></head><body>
<p class="ellipsis">A long line that ends in an ellipsis on purpose</p>
<div class="scroller">A long line inside a horizontal scroller the reader can scroll</div>
<span class="sr-only">Text for screen readers only</span>
<div class="rounded">A masked line inside a rounded card that clips</div>
</body></html>`
	if err := os.WriteFile(r.file, []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	r.page.WaitInFrame("/artifact/", "document.querySelector('.ellipsis') !== null")
	// Give the audit time to settle and report.
	time.Sleep(2500 * time.Millisecond)
	ws, _ := r.m.Store.LayoutWarnings(context.Background(), r.key)
	if len(ws) != 0 {
		t.Fatalf("intentional clipping was reported: %+v", ws)
	}
	r.page.WaitFor(`document.getElementById('layout-button').hidden`)
}
