//go:build e2e

// Package e2e drives a real headless Chrome over the DevTools protocol so
// tests can exercise redline the way a reviewer does: real clicks, real
// sandboxed iframes, real live reloads.
package e2e

import (
	"bytes"
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

	// initialTarget is the tab Chrome opened at launch, until a page takes it.
	initialTarget string
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
		"--window-size=1280,800",
		// Keep a fresh profile from doing first-run work that can stall the
		// DevTools connection: keychain access, background services, and
		// throttling of pages it considers hidden.
		"--use-mock-keychain",
		"--password-store=basic",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-sync",
		"--disable-default-apps",
		"--disable-extensions",
		"--disable-breakpad",
		"--disable-client-side-phishing-detection",
		"--disable-domain-reliability",
		"--disable-hang-monitor",
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"--disable-ipc-flooding-protection",
		"--metrics-recording-only",
		"--no-service-autorun",
		"--force-color-profile=srgb",
	}
	if os.Getenv("CI") != "" && runtime.GOOS == "linux" {
		// GitHub's Ubuntu runners restrict unprivileged user namespaces, which
		// Chrome's process sandbox needs. This does not affect iframe sandboxing.
		args = append(args, "--no-sandbox")
	}
	args = append(args, "about:blank")

	cmd := exec.Command(chrome, args...)
	chromeLog := &bytes.Buffer{}
	cmd.Stdout = io.Discard
	cmd.Stderr = &lockedWriter{w: chromeLog}
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
	t.Cleanup(func() {
		b.close()
		if t.Failed() {
			t.Logf("chrome output:\n%s", chromeLog.String())
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := b.call(ctx, "", "Target.getTargets", nil, &targets); err != nil {
		t.Fatal(err)
	}
	for _, ti := range targets.TargetInfos {
		if ti.Type == "page" {
			b.initialTarget = ti.TargetID
			break
		}
	}
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

// close asks Chrome to quit so it takes its helper processes with it, and
// kills it only if it does not exit in time.
func (b *Browser) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = b.call(ctx, "", "Browser.close", nil, nil)
	cancel()
	exited := make(chan struct{})
	go func() {
		_ = b.cmd.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		_ = b.cmd.Process.Kill()
		<-exited
	}
	_ = b.ws.Close()
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
		// A DevTools call that never answers has been hard to explain;
		// keep every goroutine's stack to show whether the read loop or
		// Chrome is stuck.
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		fmt.Fprintf(os.Stderr, "e2e: %s got no answer; goroutines:\n%s\n", method, buf[:n])
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

// lockedWriter serializes writes from Chrome's output pipe.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
