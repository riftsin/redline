//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/riftsin/redline/internal/attachments"
	"github.com/riftsin/redline/internal/netinfo"
	"github.com/riftsin/redline/internal/paths"
	"github.com/riftsin/redline/internal/review"
	"github.com/riftsin/redline/internal/server"
	"github.com/riftsin/redline/internal/store"
)

const artifactHTML = `<!doctype html>
<html lang="en"%s>
<head><meta charset="utf-8"><title>Plan</title>
<style>body{margin:0;padding:24px;font:16px/1.5 sans-serif;background:#fff;color:#111} .tall{height:2400px}</style>
</head>
<body>
<h1 id="title">Launch plan</h1>
<p id="intro">The rollout starts with internal users and widens weekly.</p>
<div role="button" id="widget" tabindex="0" onclick="this.textContent='pressed'">Press me</div>
<a id="docs" href="https://example.invalid/docs">Docs</a>
<div class="tall"></div>
<p id="bottom">The end.</p>
</body>
</html>`

type reviewEnv struct {
	t    *testing.T
	m    *review.Manager
	srv  *httptest.Server
	file string
	key  string
	page *Page
}

func newReview(t *testing.T, htmlAttrs string) *reviewEnv {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "redline.db"))
	if err != nil {
		t.Fatal(err)
	}
	m := review.NewManager(st, time.Minute)
	att, err := attachments.Open(filepath.Join(dir, "attachments"), st, attachments.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	m.Attachments = att
	plan := netinfo.Resolve(func(string) string { return "" }, func() []byte { return nil })
	s := server.New(m, att, plan, 0, server.Info{Version: "e2e"})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.Close(); m.Close(); st.Close() })

	file := filepath.Join(dir, "plan.html")
	writeArtifact(t, file, htmlAttrs, "")
	key := paths.SessionKey(file)
	if _, _, err := m.Open(context.Background(), key, file, false); err != nil {
		t.Fatal(err)
	}

	page := Launch(t).NewPage(t)
	page.SetViewport(1280, 800, false)
	page.Navigate(srv.URL + "/session/" + key)
	r := &reviewEnv{t: t, m: m, srv: srv, file: file, key: key, page: page}
	r.waitFrameReady()
	return r
}

func writeArtifact(t *testing.T, file, htmlAttrs, extra string) {
	t.Helper()
	html := strings.Replace(artifactHTML, "%s", htmlAttrs, 1)
	html = strings.Replace(html, "<p id=\"bottom\">", extra+"<p id=\"bottom\">", 1)
	if err := os.WriteFile(file, []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitFrameReady waits until the review page and the artifact's SDK have
// completed their handshake.
func (r *reviewEnv) waitFrameReady() {
	r.t.Helper()
	r.page.WaitFor("document.getElementById('review').dataset.frameReady === 'true'")
}

// poll runs the agent's poll in the background and returns once it waits
// or has already delivered feedback.
func (r *reviewEnv) poll() <-chan store.Feedback {
	out := make(chan store.Feedback, 1)
	waiting := make(chan struct{})
	go func() {
		_, _ = r.m.Poll(context.Background(), review.PollRequest{Key: r.key, File: r.file, Waiting: func() { close(waiting) }}, func(fb store.Feedback) error {
			out <- fb
			return nil
		})
	}()
	select {
	case <-waiting:
	case fb := <-out:
		out <- fb
	case <-time.After(DefaultTimeout):
		r.t.Fatal("poll neither waited nor delivered")
	}
	return out
}

func awaitFeedback(t *testing.T, c <-chan store.Feedback) store.Feedback {
	t.Helper()
	select {
	case fb := <-c:
		return fb
	case <-time.After(DefaultTimeout):
		t.Fatal("no feedback reached the poll")
	}
	return store.Feedback{}
}

const (
	cardOpen   = "!document.getElementById('annotation-card').hidden"
	cardClosed = "document.getElementById('annotation-card').hidden"
)

func (r *reviewEnv) text(selector string) string {
	r.t.Helper()
	var s string
	r.page.Eval(`document.querySelector(`+jsString(selector)+`).textContent`, &s)
	return s
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestAnnotateElementQueueAndSend(t *testing.T) {
	r := newReview(t, "")
	feedback := r.poll()
	r.page.WaitFor(`document.getElementById('presence').textContent.includes('listening')`)

	r.page.ClickInFrame("/artifact/", "#title")
	r.page.WaitFor(cardOpen)
	if title := r.text("#card-title"); title != "Annotate <h1>" {
		t.Fatalf("card title = %q", title)
	}
	if excerpt := r.text("#card-excerpt"); excerpt != "“Launch plan”" {
		t.Fatalf("card excerpt = %q", excerpt)
	}
	r.page.InsertText("Make the title bigger")
	r.page.Press("Enter", 0)
	r.page.WaitFor(`document.querySelector('.queued-bubble') && document.querySelector('.queued-bubble').textContent.includes('Make the title bigger')`)
	r.page.WaitFor("document.getElementById('annotation-card').hidden")

	r.page.Click("#composer-input")
	r.page.InsertText("Thanks!")
	r.page.Click("#send")

	fb := awaitFeedback(t, feedback)
	if len(fb.Prompts) != 2 {
		t.Fatalf("prompts = %+v", fb.Prompts)
	}
	note := fb.Prompts[0]
	if note.Prompt != "Make the title bigger" || note.Tag != "h1" || note.Selector != "h1#title" || note.Text != "Launch plan" {
		t.Fatalf("note = %+v", note)
	}
	if fb.Prompts[1].Kind != "message" || fb.Prompts[1].Prompt != "Thanks!" {
		t.Fatalf("message = %+v", fb.Prompts[1])
	}
	if !strings.Contains(fb.DOMSnapshot, `h1#title "Launch plan"`) {
		t.Fatalf("snapshot = %q", fb.DOMSnapshot)
	}

	// The sent note moves to the conversation and the composer clears.
	r.page.WaitFor(`!document.querySelector('.queued-bubble') && document.querySelector('.bubble.user .bubble-text').textContent === 'Make the title bigger'`)
	r.page.WaitFor(`document.getElementById('composer-input').value === ''`)
	r.page.WaitFor(`document.getElementById('presence').textContent.includes('working')`)

	// The agent's reply appears and concludes the work.
	if err := r.m.Reply(context.Background(), r.key, "Done: the title is bigger."); err != nil {
		t.Fatal(err)
	}
	r.page.WaitFor(`document.querySelector('.bubble.agent') && document.querySelector('.bubble.agent').textContent.includes('Done: the title is bigger.')`)
	r.page.WaitFor(`document.getElementById('presence').textContent.includes('not listening')`)
}

func TestAnnotateSelectedText(t *testing.T) {
	r := newReview(t, "")
	feedback := r.poll()

	rect := r.page.RectInFrame("/artifact/", "#intro")
	r.page.Drag(rect.X+2, rect.Y+rect.Height/2, rect.X+120, rect.Y+rect.Height/2)
	r.page.WaitFor(cardOpen)
	if title := r.text("#card-title"); title != "Annotate text" {
		t.Fatalf("card title = %q", title)
	}
	r.page.InsertText("Clarify")
	r.page.Press("Enter", ModCtrl) // Ctrl+Enter queues and sends
	fb := awaitFeedback(t, feedback)
	if len(fb.Prompts) != 1 {
		t.Fatalf("prompts = %+v", fb.Prompts)
	}
	var target struct {
		Type, Text, Selector string
	}
	if err := json.Unmarshal(fb.Prompts[0].Target, &target); err != nil {
		t.Fatal(err)
	}
	if target.Type != "text-range" || !strings.HasPrefix(target.Text, "The rollout") || len(target.Text) < 12 || target.Selector != "p#intro" {
		t.Fatalf("target = %+v", target)
	}
}

func TestInteractiveWidgetsAndLinks(t *testing.T) {
	r := newReview(t, "")

	// An ARIA button keeps working and opens no note.
	r.page.ClickInFrame("/artifact/", "#widget")
	r.page.WaitInFrame("/artifact/", "document.getElementById('widget').textContent === 'pressed'")
	r.page.WaitFor(cardClosed)

	// A modifier-click follows a link instead of annotating it.
	mod := ModCtrl
	if runtime.GOOS == "darwin" {
		mod = ModMeta
	}
	x, y := r.page.RectInFrame("/artifact/", "#docs").Center()
	r.page.ClickAt(x, y, mod)
	time.Sleep(300 * time.Millisecond)
	r.page.WaitFor(cardClosed)

	// A plain click on the link annotates it.
	r.page.ClickAt(x, y, 0)
	r.page.WaitFor(cardOpen)
	if title := r.text("#card-title"); title != "Annotate <a>" {
		t.Fatalf("card title = %q", title)
	}
}

func TestExploreModeDefaultAndToggle(t *testing.T) {
	r := newReview(t, ` data-redline-annotate="off"`)
	r.page.WaitFor(`document.querySelector('.mode[data-mode=explore]').classList.contains('active')`)
	r.page.ClickInFrame("/artifact/", "#title")
	time.Sleep(300 * time.Millisecond)
	r.page.WaitFor(cardClosed)

	// The toolbar overrides the page default.
	r.page.Click(".mode[data-mode=annotate]")
	r.page.ClickInFrame("/artifact/", "#title")
	r.page.WaitFor(cardOpen)

	// The reviewer's choice survives an artifact reload.
	writeArtifact(t, r.file, ` data-redline-annotate="off"`, "<p>v2</p>")
	r.page.WaitInFrame("/artifact/", "document.body.textContent.includes('v2')")
	r.waitFrameReady()
	r.page.WaitFor(`document.querySelector('.mode[data-mode=annotate]').classList.contains('active')`)
}

func TestLiveReloadKeepsScroll(t *testing.T) {
	r := newReview(t, "")
	r.page.EvalInFrame("/artifact/", "window.scrollTo(0, 900)", nil)
	time.Sleep(400 * time.Millisecond) // the SDK reports scroll after 150ms of quiet

	writeArtifact(t, r.file, "", "<p id=\"added\">Added by the agent</p>")
	r.page.WaitInFrame("/artifact/", "document.getElementById('added') !== null")
	r.page.WaitInFrame("/artifact/", "Math.abs(window.scrollY - 900) < 2")
}

func TestUserEndsSession(t *testing.T) {
	r := newReview(t, "")
	feedback := r.poll()
	r.page.Click("#composer-input")
	r.page.InsertText("Looks good, ship it")
	r.page.Click("#send-end")
	fb := awaitFeedback(t, feedback)
	if !fb.SessionEnded || fb.EndedBy != "user" || fb.Prompts[0].Prompt != "Looks good, ship it" {
		t.Fatalf("final feedback = %+v", fb)
	}
	r.page.WaitFor(`document.getElementById('review').classList.contains('is-ended')`)
	r.page.WaitFor(`!document.getElementById('ended-banner').hidden`)
	r.page.WaitFor(`getComputedStyle(document.getElementById('composer')).display === 'none'`)
}

func TestPhoneBottomSheet(t *testing.T) {
	r := newReview(t, "")
	r.page.SetViewport(390, 844, false)
	const open = "document.documentElement.classList.contains('sheet-open')"

	// The sheet slides for 0.28s; tap it once it has settled, as a person would.
	settle := func() { time.Sleep(400 * time.Millisecond) }
	settle() // the resize to phone width slides the panel down to its dock
	r.page.Click("#sheet-dock")
	r.page.WaitFor(open)
	settle()
	r.page.Click("#sheet-dock")
	r.page.WaitFor("!" + open)
	settle()

	// Pointing at an element raises the sheet so the note card is visible.
	r.page.ClickInFrame("/artifact/", "#intro")
	r.page.WaitFor(open + " && " + cardOpen)

	// The dimmed artifact closes it.
	settle()
	r.page.ClickAt(195, 60, 0)
	r.page.WaitFor("!" + open)

	// A reply that arrives while the sheet is down marks the dock.
	if err := r.m.Reply(context.Background(), r.key, "Updated."); err != nil {
		t.Fatal(err)
	}
	r.page.WaitFor("document.documentElement.classList.contains('has-unread')")
	settle()
	r.page.Click("#sheet-dock")
	r.page.WaitFor(open + " && !document.documentElement.classList.contains('has-unread')")
}
