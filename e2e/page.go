//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
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

	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := b.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &created); err != nil {
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

// Rect returns selector's bounding box in the top-level viewport.
func (p *Page) Rect(selector string) Rect {
	p.t.Helper()
	var r Rect
	p.Eval(rectExpression(selector), &r)
	return r
}

// RectInFrame returns selector's bounding box, inside the frame whose URL
// contains urlPart, translated into the top-level viewport.
func (p *Page) RectInFrame(urlPart, selector string) Rect {
	p.t.Helper()
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
	for _, ev := range []map[string]any{
		{"type": "mouseMoved", "x": x, "y": y, "modifiers": modifiers},
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
