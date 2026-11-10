//go:build e2e

// Package e2e drives a real headless Chrome over the DevTools protocol so
// tests can exercise redline the way a reviewer does: real clicks, real
// sandboxed iframes, real live reloads.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Browser is one headless Chrome process and its DevTools connection.
type Browser struct {
	cmd *exec.Cmd
	ws  *wsConn

	nextID    atomic.Int64
	mu        sync.Mutex
	pending   map[int64]chan cdpMessage
	waiters   []*eventWaiter
	listeners []func(cdpMessage)
	closed    chan struct{}
	readErr   error
}

type cdpMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

func (e *cdpError) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("cdp %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("cdp %d: %s", e.Code, e.Message)
}

type eventWaiter struct {
	sessionID string
	method    string
	match     func(json.RawMessage) bool
	ch        chan json.RawMessage
}

// ChromePath finds a Chrome or Chromium binary. REDLINE_CHROME overrides the
// search.
func ChromePath() (string, error) {
	if p := os.Getenv("REDLINE_CHROME"); p != "" {
		return p, nil
	}
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	case "windows":
		candidates = []string{
			filepath.Join(os.Getenv("ProgramFiles"), `Google\Chrome\Application\chrome.exe`),
			filepath.Join(os.Getenv("ProgramFiles(x86)"), `Google\Chrome\Application\chrome.exe`),
		}
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(name); err == nil {
			candidates = append(candidates, p)
		}
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("no Chrome found; set REDLINE_CHROME")
}

// Launch starts headless Chrome for the duration of the test.
func Launch(t testing.TB) *Browser {
	t.Helper()
	chrome, err := ChromePath()
	if err != nil {
		t.Fatal(err)
	}
	profile := t.TempDir()
	args := []string{
		"--headless=new",
		"--remote-debugging-port=0",
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-sync",
		"--window-size=1280,800",
	}
	if os.Getenv("CI") != "" && runtime.GOOS == "linux" {
		// GitHub's Ubuntu runners restrict unprivileged user namespaces, which
		// Chrome's process sandbox needs. This does not affect iframe sandboxing.
		args = append(args, "--no-sandbox")
	}
	args = append(args, "about:blank")

	cmd := exec.Command(chrome, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start chrome: %v", err)
	}

	wsURL, err := waitForDevToolsURL(profile, 20*time.Second)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	ws, err := dialWS(wsURL)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("connect devtools: %v", err)
	}

	b := &Browser{
		cmd:     cmd,
		ws:      ws,
		pending: map[int64]chan cdpMessage{},
		closed:  make(chan struct{}),
	}
	go b.readLoop()
	t.Cleanup(b.close)
	return b
}

func waitForDevToolsURL(profile string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	file := filepath.Join(profile, "DevToolsActivePort")
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(file)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) == 2 {
				return "ws://127.0.0.1:" + strings.TrimSpace(lines[0]) + strings.TrimSpace(lines[1]), nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", errors.New("chrome did not report a DevTools port")
}

func (b *Browser) close() {
	_ = b.ws.Close()
	_ = b.cmd.Process.Kill()
	_ = b.cmd.Wait()
}

func (b *Browser) readLoop() {
	defer close(b.closed)
	for {
		data, err := b.ws.ReadMessage()
		if err != nil {
			b.mu.Lock()
			b.readErr = err
			b.mu.Unlock()
			return
		}
		var msg cdpMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		b.mu.Lock()
		if msg.ID != 0 {
			if ch, ok := b.pending[msg.ID]; ok {
				delete(b.pending, msg.ID)
				ch <- msg
			}
		} else if msg.Method != "" {
			for _, listen := range b.listeners {
				listen(msg)
			}
			kept := b.waiters[:0]
			for _, w := range b.waiters {
				if w.method == msg.Method && w.sessionID == msg.SessionID && (w.match == nil || w.match(msg.Params)) {
					w.ch <- msg.Params
					continue
				}
				kept = append(kept, w)
			}
			b.waiters = kept
		}
		b.mu.Unlock()
	}
}

// call sends one DevTools command and waits for its result.
func (b *Browser) call(ctx context.Context, sessionID, method string, params any, result any) error {
	id := b.nextID.Add(1)
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	ch := make(chan cdpMessage, 1)
	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()

	if err := b.ws.WriteText(data); err != nil {
		return err
	}
	select {
	case reply := <-ch:
		if reply.Error != nil {
			return fmt.Errorf("%s: %w", method, reply.Error)
		}
		if result != nil {
			return json.Unmarshal(reply.Result, result)
		}
		return nil
	case <-b.closed:
		return fmt.Errorf("%s: browser connection closed: %v", method, b.readErr)
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

// listen calls fn for every event, on the read loop, with b.mu held. fn must
// not call back into the browser.
func (b *Browser) listen(fn func(cdpMessage)) {
	b.mu.Lock()
	b.listeners = append(b.listeners, fn)
	b.mu.Unlock()
}

// expectEvent registers interest in an event before the action that causes
// it, so the event cannot slip past between the action and the wait.
func (b *Browser) expectEvent(sessionID, method string, match func(json.RawMessage) bool) *eventWaiter {
	w := &eventWaiter{sessionID: sessionID, method: method, match: match, ch: make(chan json.RawMessage, 1)}
	b.mu.Lock()
	b.waiters = append(b.waiters, w)
	b.mu.Unlock()
	return w
}

func (b *Browser) await(ctx context.Context, w *eventWaiter) (json.RawMessage, error) {
	select {
	case params := <-w.ch:
		return params, nil
	case <-b.closed:
		return nil, fmt.Errorf("waiting for %s: browser connection closed", w.method)
	case <-ctx.Done():
		b.mu.Lock()
		for i, other := range b.waiters {
			if other == w {
				b.waiters = append(b.waiters[:i], b.waiters[i+1:]...)
				break
			}
		}
		b.mu.Unlock()
		return nil, fmt.Errorf("waiting for %s: %w", w.method, ctx.Err())
	}
}
