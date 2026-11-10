//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Page is one browser tab.
type Page struct {
	t         testing.TB
	b         *Browser
	targetID  string
	sessionID string

	mu sync.Mutex
	// frameSessions maps out-of-process iframe frame ids to their DevTools
	// session. Chrome isolates sandboxed iframes into their own process, so
	// an artifact frame is usually one of these.
	frameSessions map[string]string
	// errors collects uncaught JavaScript exceptions from every frame; a
	// failing test prints them.
	errors []string
}

// DefaultTimeout bounds every single browser step.
const DefaultTimeout = 15 * time.Second

func (p *Page) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), DefaultTimeout)
}

// NewPage opens a blank tab.
func (b *Browser) NewPage(t testing.TB) *Page {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()

	// Use the tab Chrome opened at launch when it is still free, so no
	// second renderer has to start.
	var created struct {
		TargetID string `json:"targetId"`
	}
	b.mu.Lock()
	initial := b.initialTarget
	b.initialTarget = ""
	b.mu.Unlock()
	if initial != "" {
		created.TargetID = initial
	} else if err := b.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &created); err != nil {
		t.Fatal(err)
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := b.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &attached); err != nil {
		t.Fatal(err)
	}
	p := &Page{t: t, b: b, targetID: created.TargetID, sessionID: attached.SessionID, frameSessions: map[string]string{}}

	b.listen(func(msg cdpMessage) {
		p.mu.Lock()
		ours := msg.SessionID == p.sessionID
		for _, s := range p.frameSessions {
			ours = ours || msg.SessionID == s
		}
		p.mu.Unlock()
		if !ours {
			return
		}
		switch msg.Method {
		case "Target.attachedToTarget":
			var ev struct {
				SessionID  string `json:"sessionId"`
				TargetInfo struct {
					TargetID string `json:"targetId"`
					Type     string `json:"type"`
				} `json:"targetInfo"`
			}
			if json.Unmarshal(msg.Params, &ev) == nil && ev.TargetInfo.Type == "iframe" {
				p.mu.Lock()
				// An OOPIF's target id is its frame id.
				p.frameSessions[ev.TargetInfo.TargetID] = ev.SessionID
				p.mu.Unlock()
				// Report the frame's exceptions too. This runs on the read
				// loop, so the call must not wait here.
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
					defer cancel()
					_ = b.call(ctx, ev.SessionID, "Runtime.enable", nil, nil)
				}()
			}
		case "Runtime.exceptionThrown":
			var ev struct {
				ExceptionDetails struct {
					Text      string `json:"text"`
					URL       string `json:"url"`
					Line      int    `json:"lineNumber"`
					Exception struct {
						Description string `json:"description"`
					} `json:"exception"`
				} `json:"exceptionDetails"`
			}
			if json.Unmarshal(msg.Params, &ev) == nil {
				d := ev.ExceptionDetails
				p.mu.Lock()
				p.errors = append(p.errors, fmt.Sprintf("%s:%d %s %s", d.URL, d.Line+1, d.Text, d.Exception.Description))
				p.mu.Unlock()
			}
		case "Target.detachedFromTarget":
			var ev struct {
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal(msg.Params, &ev) == nil {
				p.mu.Lock()
				for frame, session := range p.frameSessions {
					if session == ev.SessionID {
						delete(p.frameSessions, frame)
					}
				}
				p.mu.Unlock()
			}
		}
	})

	for _, method := range []string{"Page.enable", "Runtime.enable", "DOM.enable"} {
		if err := b.call(ctx, p.sessionID, method, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.call(ctx, p.sessionID, "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": false, "flatten": true,
	}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.mu.Lock()
		errs := append([]string(nil), p.errors...)
		p.mu.Unlock()
		if t.Failed() {
			for _, e := range errs {
				t.Logf("page exception: %s", e)
			}
			var debug string
			if p.eval(p.sessionID, 0, `JSON.stringify({error: (document.getElementById('composer-error')||{}).textContent, card: !(document.getElementById('annotation-card')||{hidden:true}).hidden})`, &debug) == nil {
				t.Logf("page state: %s", debug)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
		defer cancel()
		_ = b.call(ctx, "", "Target.closeTarget", map[string]any{"targetId": p.targetID}, nil)
	})
	return p
}

// Navigate loads url and waits for its load event, which includes subframes.
func (p *Page) Navigate(url string) {
	p.t.Helper()
	ctx, cancel := p.ctx()
	defer cancel()
	loaded := p.b.expectEvent(p.sessionID, "Page.loadEventFired", nil)
	var nav struct {
		ErrorText string `json:"errorText"`
	}
	if err := p.b.call(ctx, p.sessionID, "Page.navigate", map[string]any{"url": url}, &nav); err != nil {
		p.t.Fatal(err)
	}
	if nav.ErrorText != "" {
		p.t.Fatalf("navigate %s: %s", url, nav.ErrorText)
	}
	if _, err := p.b.await(ctx, loaded); err != nil {
		p.t.Fatal(err)
	}
}

// Eval evaluates a JavaScript expression in the top-level page and decodes
// its (awaited) value into out, which may be nil.
func (p *Page) Eval(expression string, out any) {
	p.t.Helper()
	if err := p.eval(p.sessionID, 0, expression, out); err != nil {
		p.t.Fatal(err)
	}
}

// EvalInFrame evaluates expression in an isolated world of the first frame
// whose URL contains urlPart. The isolated world sees the frame's DOM but
// not its scripts' globals.
func (p *Page) EvalInFrame(urlPart, expression string, out any) {
	p.t.Helper()
	session, contextID, err := p.frameContext(urlPart)
	if err != nil {
		p.t.Fatal(err)
	}
	if err := p.eval(session, contextID, expression, out); err != nil {
		p.t.Fatal(err)
	}
}

func (p *Page) eval(sessionID string, contextID int64, expression string, out any) error {
	ctx, cancel := p.ctx()
	defer cancel()
	params := map[string]any{"expression": expression, "returnByValue": true, "awaitPromise": true}
	if contextID != 0 {
		params["contextId"] = contextID
	}
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := p.b.call(ctx, sessionID, "Runtime.evaluate", params, &res); err != nil {
		return err
	}
	if res.ExceptionDetails != nil {
		return fmt.Errorf("eval %q: %s %s", expression, res.ExceptionDetails.Text, res.ExceptionDetails.Exception.Description)
	}
	if out != nil && len(res.Result.Value) > 0 {
		return json.Unmarshal(res.Result.Value, out)
	}
	return nil
}

type frameNode struct {
	Frame struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	} `json:"frame"`
	ChildFrames []frameNode `json:"childFrames"`
}

func findFrame(n frameNode, urlPart string) (string, bool) {
	if strings.Contains(n.Frame.URL, urlPart) {
		return n.Frame.ID, true
	}
	for _, c := range n.ChildFrames {
		if id, ok := findFrame(c, urlPart); ok {
			return id, true
		}
	}
	return "", false
}

// frameContext finds the frame and returns the session that owns it plus an
// isolated-world execution context inside it.
func (p *Page) frameContext(urlPart string) (sessionID string, contextID int64, err error) {
	ctx, cancel := p.ctx()
	defer cancel()

	frameID, sessionID, err := p.findFrameAnywhere(ctx, urlPart)
	if err != nil {
		return "", 0, err
	}
	var world struct {
		ExecutionContextID int64 `json:"executionContextId"`
	}
	err = p.b.call(ctx, sessionID, "Page.createIsolatedWorld", map[string]any{
		"frameId": frameID, "worldName": "redline-e2e",
	}, &world)
	return sessionID, world.ExecutionContextID, err
}

func (p *Page) findFrameAnywhere(ctx context.Context, urlPart string) (frameID, sessionID string, err error) {
	sessions := []string{p.sessionID}
	p.mu.Lock()
	for _, s := range p.frameSessions {
		sessions = append(sessions, s)
	}
	p.mu.Unlock()

	for _, s := range sessions {
		var tree struct {
			FrameTree frameNode `json:"frameTree"`
		}
		if err := p.b.call(ctx, s, "Page.getFrameTree", nil, &tree); err != nil {
			continue
		}
		if id, ok := findFrame(tree.FrameTree, urlPart); ok {
			// The top-level session also lists OOPIF frames; evaluate through
			// the frame's own session when it has one.
			p.mu.Lock()
			owner, isOOPIF := p.frameSessions[id]
			p.mu.Unlock()
			if isOOPIF {
				return id, owner, nil
			}
			return id, s, nil
		}
	}
	return "", "", fmt.Errorf("no frame with URL containing %q", urlPart)
}

// Rect is a viewport rectangle in CSS pixels.
type Rect struct {
	X, Y, Width, Height float64
}

// Center returns the rectangle's middle point.
func (r Rect) Center() (float64, float64) {
	return r.X + r.Width/2, r.Y + r.Height/2
}

// Rect returns selector's bounding box in the top-level viewport, once it
// is settled.
func (p *Page) Rect(selector string) Rect {
	p.t.Helper()
	p.Eval(settleScript(selector), nil)
	var r Rect
	p.Eval(rectExpression(selector), &r)
	return r
}

// settleScript brings selector's element into view and resolves once it has
// stopped moving for a few frames (smooth scrolling, a sliding sheet, a
// region the server just re-rendered). Input aimed at something Chrome has
// not painted yet is dropped or lands elsewhere, so every click waits for it,
// as a person's would.
func settleScript(selector string) string {
	sel, _ := json.Marshal(selector)
	return fmt.Sprintf(`(async () => {
		const el = document.querySelector(%s);
		if (!el) return;
		const r0 = el.getBoundingClientRect();
		if (r0.bottom < 0 || r0.top > innerHeight || r0.right < 0 || r0.left > innerWidth) el.scrollIntoView({block: "center"});
		const frame = () => new Promise((r) => requestAnimationFrame(r));
		let last = "", stable = 0;
		for (let i = 0; i < 120 && stable < 3; i++) {
			await frame();
			const r = el.getBoundingClientRect();
			const now = r.x + "," + r.y + "," + r.width + "," + r.height;
			stable = now === last ? stable + 1 : 0;
			last = now;
		}
	})()`, sel)
}

// RectInFrame returns selector's bounding box, inside the frame whose URL
// contains urlPart, translated into the top-level viewport, once it is
// settled.
func (p *Page) RectInFrame(urlPart, selector string) Rect {
	p.t.Helper()
	p.EvalInFrame(urlPart, settleScript(selector), nil)
	var inner Rect
	p.EvalInFrame(urlPart, rectExpression(selector), &inner)

	ctx, cancel := p.ctx()
	defer cancel()
	frameID, _, err := p.findFrameAnywhere(ctx, urlPart)
	if err != nil {
		p.t.Fatal(err)
	}
	var owner struct {
		BackendNodeID int64 `json:"backendNodeId"`
	}
	if err := p.b.call(ctx, p.sessionID, "DOM.getFrameOwner", map[string]any{"frameId": frameID}, &owner); err != nil {
		p.t.Fatal(err)
	}
	var box struct {
		Model struct {
			Content []float64 `json:"content"`
		} `json:"model"`
	}
	if err := p.b.call(ctx, p.sessionID, "DOM.getBoxModel", map[string]any{"backendNodeId": owner.BackendNodeID}, &box); err != nil {
		p.t.Fatal(err)
	}
	inner.X += box.Model.Content[0]
	inner.Y += box.Model.Content[1]
	return inner
}

func rectExpression(selector string) string {
	sel, _ := json.Marshal(selector)
	return fmt.Sprintf(`(() => {
		const el = document.querySelector(%s);
		if (!el) throw new Error("no element " + %s);
		const r = el.getBoundingClientRect();
		return {X: r.x, Y: r.y, Width: r.width, Height: r.height};
	})()`, sel, sel)
}

// ClickAt sends a real left click at viewport coordinates.
func (p *Page) ClickAt(x, y float64, modifiers int) {
	p.t.Helper()
	ctx, cancel := p.ctx()
	defer cancel()
	if err := p.b.call(ctx, p.sessionID, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": x, "y": y, "modifiers": modifiers}, nil); err != nil {
		p.t.Fatal(err)
	}
	// Input aimed at another frame is routed asynchronously; like a real
	// pointer, arrive before pressing.
	time.Sleep(50 * time.Millisecond)
	for _, ev := range []map[string]any{
		{"type": "mousePressed", "x": x, "y": y, "button": "left", "clickCount": 1, "modifiers": modifiers},
		{"type": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": 1, "modifiers": modifiers},
	} {
		if err := p.b.call(ctx, p.sessionID, "Input.dispatchMouseEvent", ev, nil); err != nil {
			p.t.Fatal(err)
		}
	}
}

// Click clicks the middle of selector in the top-level page.
func (p *Page) Click(selector string) {
	p.t.Helper()
	x, y := p.Rect(selector).Center()
	p.ClickAt(x, y, 0)
}

// ClickInFrame clicks the middle of selector inside the frame whose URL
// contains urlPart.
func (p *Page) ClickInFrame(urlPart, selector string) {
	p.t.Helper()
	x, y := p.RectInFrame(urlPart, selector).Center()
	p.ClickAt(x, y, 0)
}

// WaitFor polls a top-level JavaScript expression until it is truthy.
func (p *Page) WaitFor(expression string) {
	p.t.Helper()
	deadline := time.Now().Add(DefaultTimeout)
	for {
		var ok bool
		if err := p.eval(p.sessionID, 0, "!!("+expression+")", &ok); err != nil {
			p.t.Fatal(err)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("timed out waiting for %s", expression)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// InsertText types text into the focused element, as an input method would.
func (p *Page) InsertText(text string) {
	p.t.Helper()
	ctx, cancel := p.ctx()
	defer cancel()
	if err := p.b.call(ctx, p.sessionID, "Input.insertText", map[string]any{"text": text}, nil); err != nil {
		p.t.Fatal(err)
	}
}

// Modifier bits for key and mouse events.
const (
	ModAlt   = 1
	ModCtrl  = 2
	ModMeta  = 4
	ModShift = 8
)

var keyCodes = map[string]struct {
	code string
	vk   int
	text string
}{
	"Enter":  {"Enter", 13, "\r"},
	"Escape": {"Escape", 27, ""},
	"i":      {"KeyI", 73, "i"},
}

// Press sends a key down and up to the focused element.
func (p *Page) Press(key string, modifiers int) {
	p.t.Helper()
	k, ok := keyCodes[key]
	if !ok {
		p.t.Fatalf("Press: unknown key %q", key)
	}
	ctx, cancel := p.ctx()
	defer cancel()
	down := map[string]any{"type": "keyDown", "key": key, "code": k.code, "windowsVirtualKeyCode": k.vk, "modifiers": modifiers}
	if k.text != "" && modifiers&(ModCtrl|ModMeta|ModAlt) == 0 {
		down["type"] = "keyDown"
		down["text"] = k.text
	} else {
		down["type"] = "rawKeyDown"
	}
	for _, ev := range []map[string]any{down, {"type": "keyUp", "key": key, "code": k.code, "windowsVirtualKeyCode": k.vk, "modifiers": modifiers}} {
		if err := p.b.call(ctx, p.sessionID, "Input.dispatchKeyEvent", ev, nil); err != nil {
			p.t.Fatal(err)
		}
	}
}

// Drag presses the mouse at one point and releases it at another, which
// selects text the way a reviewer does.
func (p *Page) Drag(x1, y1, x2, y2 float64) {
	p.t.Helper()
	ctx, cancel := p.ctx()
	defer cancel()
	if err := p.b.call(ctx, p.sessionID, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": x1, "y": y1}, nil); err != nil {
		p.t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // arrive before pressing, as in ClickAt
	events := []map[string]any{
		{"type": "mousePressed", "x": x1, "y": y1, "button": "left", "buttons": 1, "clickCount": 1},
	}
	for i := 1; i <= 5; i++ {
		f := float64(i) / 5
		events = append(events, map[string]any{"type": "mouseMoved", "x": x1 + (x2-x1)*f, "y": y1 + (y2-y1)*f, "button": "left", "buttons": 1})
	}
	events = append(events, map[string]any{"type": "mouseReleased", "x": x2, "y": y2, "button": "left", "clickCount": 1})
	for _, ev := range events {
		if err := p.b.call(ctx, p.sessionID, "Input.dispatchMouseEvent", ev, nil); err != nil {
			p.t.Fatal(err)
		}
	}
}

// WaitInFrame polls an expression in the frame whose URL contains urlPart
// until it is truthy.
func (p *Page) WaitInFrame(urlPart, expression string) {
	p.t.Helper()
	deadline := time.Now().Add(DefaultTimeout)
	for {
		var ok bool
		session, contextID, err := p.frameContext(urlPart)
		if err == nil {
			err = p.eval(session, contextID, "!!("+expression+")", &ok)
		}
		if err == nil && ok {
			return
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("timed out waiting in frame %s for %s (last error: %v)", urlPart, expression, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Screenshot saves a PNG of the viewport.
func (p *Page) Screenshot(path string) {
	p.t.Helper()
	ctx, cancel := p.ctx()
	defer cancel()
	var shot struct {
		Data string `json:"data"`
	}
	if err := p.b.call(ctx, p.sessionID, "Page.captureScreenshot", map[string]any{"format": "png"}, &shot); err != nil {
		p.t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		p.t.Fatal(err)
	}
}

// SetViewport resizes the page's viewport (CSS pixels).
func (p *Page) SetViewport(width, height int, dark bool) {
	p.t.Helper()
	ctx, cancel := p.ctx()
	defer cancel()
	if err := p.b.call(ctx, p.sessionID, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width": width, "height": height, "deviceScaleFactor": 1, "mobile": width < 600,
	}, nil); err != nil {
		p.t.Fatal(err)
	}
	scheme := "light"
	if dark {
		scheme = "dark"
	}
	if err := p.b.call(ctx, p.sessionID, "Emulation.setEmulatedMedia", map[string]any{
		"features": []map[string]string{{"name": "prefers-color-scheme", "value": scheme}},
	}, nil); err != nil {
		p.t.Fatal(err)
	}
}

// SetInputFiles chooses files for a file input in the top-level page, as if
// the reviewer picked them.
func (p *Page) SetInputFiles(selector string, files ...string) {
	p.t.Helper()
	ctx, cancel := p.ctx()
	defer cancel()
	var doc struct {
		Root struct {
			NodeID int64 `json:"nodeId"`
		} `json:"root"`
	}
	if err := p.b.call(ctx, p.sessionID, "DOM.getDocument", map[string]any{"depth": 0}, &doc); err != nil {
		p.t.Fatal(err)
	}
	var found struct {
		NodeID int64 `json:"nodeId"`
	}
	if err := p.b.call(ctx, p.sessionID, "DOM.querySelector", map[string]any{"nodeId": doc.Root.NodeID, "selector": selector}, &found); err != nil || found.NodeID == 0 {
		p.t.Fatalf("no element %s: %v", selector, err)
	}
	if err := p.b.call(ctx, p.sessionID, "DOM.setFileInputFiles", map[string]any{"nodeId": found.NodeID, "files": files}, nil); err != nil {
		p.t.Fatal(err)
	}
}
