package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/riftsin/redline/internal/netinfo"
	"github.com/riftsin/redline/internal/paths"
	"github.com/riftsin/redline/internal/review"
	"github.com/riftsin/redline/internal/store"
)

type env struct {
	t    *testing.T
	srv  *httptest.Server
	m    *review.Manager
	dir  string
	file string
	key  string
}

func newEnv(t *testing.T) *env {
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
	plan := netinfo.Resolve(func(string) string { return "" }, func() []byte { return nil })
	s := New(m, plan, 4389, Info{Version: "v0.0.0-test", StateID: "test"})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.Close(); m.Close(); st.Close() })

	artifactDir := filepath.Join(dir, "work")
	if err := os.MkdirAll(filepath.Join(artifactDir, "img"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(artifactDir, "plan.html")
	write(t, file, "<!doctype html><title>Plan</title><body><h1>Plan</h1><!-- </body> --></body>")
	write(t, filepath.Join(artifactDir, "img", "logo.svg"), "<svg/>")
	write(t, filepath.Join(dir, "secret.txt"), "secret")
	return &env{t: t, srv: srv, m: m, dir: dir, file: file, key: paths.SessionKey(file)}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// do sends a request as the CLI would (no Origin) unless headers say otherwise.
func (e *env) do(method, path string, body any, headers map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, r)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func (e *env) open() OpenResponse {
	e.t.Helper()
	resp, data := e.do("POST", "/api/sessions", map[string]any{"file": e.file}, nil)
	if resp.StatusCode != 200 {
		e.t.Fatalf("open: %d %s", resp.StatusCode, data)
	}
	var out OpenResponse
	if err := json.Unmarshal(data, &out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// browser headers for a same-origin page request.
func (e *env) browser() map[string]string {
	return map[string]string{"Origin": e.srv.URL}
}

func TestHealth(t *testing.T) {
	e := newEnv(t)
	resp, data := e.do("GET", "/health", nil, nil)
	var h HealthResponse
	if resp.StatusCode != 200 || json.Unmarshal(data, &h) != nil || h.App != "redline" || h.Version != "v0.0.0-test" {
		t.Fatalf("health: %d %s", resp.StatusCode, data)
	}
}

func TestOpenRejectsNonHTML(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.do("POST", "/api/sessions", map[string]any{"file": filepath.Join(e.dir, "secret.txt")}, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("non-HTML open: %d", resp.StatusCode)
	}
}

func TestArtifactRoute(t *testing.T) {
	e := newEnv(t)
	e.open()

	resp, data := e.do("GET", "/artifact/"+e.key+"/plan.html", nil, nil)
	want := "<!doctype html><title>Plan</title><body><h1>Plan</h1><!-- </body> -->" + SDKTag + "</body>"
	if resp.StatusCode != 200 || string(data) != want {
		t.Fatalf("artifact: %d %q", resp.StatusCode, data)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); csp != "sandbox "+Sandbox {
		t.Fatalf("artifact CSP = %q", csp)
	}

	resp, data = e.do("GET", "/artifact/"+e.key+"/img/logo.svg", nil, nil)
	if resp.StatusCode != 200 || string(data) != "<svg/>" || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("asset: %d %q", resp.StatusCode, data)
	}

	if err := os.Symlink(filepath.Join(e.dir, "secret.txt"), filepath.Join(filepath.Dir(e.file), "leak.txt")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"leak.txt", "../secret.txt", "%2e%2e/secret.txt", "img/%2e%2e/%2e%2e/secret.txt"} {
		if resp, data := e.do("GET", "/artifact/"+e.key+"/"+p, nil, nil); resp.StatusCode == 200 {
			t.Errorf("%s escaped the artifact directory: %q", p, data)
		}
	}
}

func TestMissingArtifactWakesAgent(t *testing.T) {
	e := newEnv(t)
	e.open()
	if err := os.Remove(e.file); err != nil {
		t.Fatal(err)
	}
	if resp, _ := e.do("GET", "/artifact/"+e.key+"/plan.html", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("missing artifact: %d", resp.StatusCode)
	}
	fb, _ := e.m.Store.TakeFeedback(context.Background(), e.key)
	if fb.Status != "feedback" || len(fb.ArtifactFailures) != 1 || fb.ArtifactFailures[0].Kind != "artifact-unavailable" {
		t.Fatalf("feedback = %+v", fb)
	}
}

func TestReviewPageHeaders(t *testing.T) {
	e := newEnv(t)
	e.open()
	resp, data := e.do("GET", "/session/"+e.key, nil, nil)
	if resp.StatusCode != 200 || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("review page: %d %v", resp.StatusCode, resp.Header)
	}
	if !strings.Contains(string(data), `sandbox="`+Sandbox+`"`) || !strings.Contains(string(data), "plan.html") {
		t.Fatalf("review page body:\n%s", data)
	}
}

func TestGuards(t *testing.T) {
	e := newEnv(t)
	e.open()

	req, _ := http.NewRequest("GET", e.srv.URL+"/health", nil)
	req.Host = "evil.example:4389"
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 403 {
		t.Errorf("rebinding Host: %v %v", resp.StatusCode, err)
	}

	foreign := map[string]string{"Origin": "http://evil.example"}
	if resp, _ := e.do("POST", "/api/"+e.key+"/send", map[string]any{"message": "x"}, foreign); resp.StatusCode != 403 {
		t.Errorf("foreign Origin send: %d", resp.StatusCode)
	}
	if resp, _ := e.do("POST", "/shutdown", nil, map[string]string{"Referer": "http://evil.example/page"}); resp.StatusCode != 403 {
		t.Errorf("foreign Referer shutdown: %d", resp.StatusCode)
	}
	if resp, data := e.do("POST", "/api/"+e.key+"/send", map[string]any{"message": "ok"}, e.browser()); resp.StatusCode != 200 {
		t.Errorf("same-origin send: %d %s", resp.StatusCode, data)
	}
}

// startPoll runs `redline poll` against the server and returns its final
// JSON once it completes.
func (e *env) startPoll(query string, body any) <-chan PollResponse {
	out := make(chan PollResponse, 1)
	go func() {
		method := "GET"
		var r io.Reader
		if body != nil {
			method = "POST"
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, e.srv.URL+"/api/poll?file="+e.file+query, r)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- PollResponse{Status: "client-error", Error: err.Error()}
			return
		}
		defer resp.Body.Close()
		var pr PollResponse
		data, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(bytes.TrimSpace(data), &pr); err != nil {
			pr = PollResponse{Status: "bad-json", Error: string(data)}
		}
		out <- pr
	}()
	return out
}

func (e *env) waitListening() {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for e.m.Presence(e.key).State != review.Listening {
		if time.Now().After(deadline) {
			e.t.Fatal("poll never started listening")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func awaitPoll(t *testing.T, c <-chan PollResponse) PollResponse {
	t.Helper()
	select {
	case r := <-c:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not return")
	}
	return PollResponse{}
}

func TestPollRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.open()

	done := e.startPoll("&owner=claude", nil)
	e.waitListening()

	// The reviewer queues a note on an element, then sends with a message.
	if resp, data := e.do("POST", "/api/"+e.key+"/queue", map[string]any{
		"prompt": "Bigger title", "selector": "body > h1", "tag": "h1", "text": "Plan",
	}, e.browser()); resp.StatusCode != 200 {
		t.Fatalf("queue: %d %s", resp.StatusCode, data)
	}
	if resp, data := e.do("POST", "/api/"+e.key+"/send", map[string]any{"message": "Thanks", "snapshot": "body\n  h1 \"Plan\""}, e.browser()); resp.StatusCode != 200 {
		t.Fatalf("send: %d %s", resp.StatusCode, data)
	}

	r := awaitPoll(t, done)
	if r.Status != "feedback" || len(r.Prompts) != 2 || r.Prompts[0].Selector != "body > h1" || r.Prompts[1].Prompt != "Thanks" || r.DOMSnapshot == "" {
		t.Fatalf("poll = %+v", r)
	}

	// The agent replies through the next poll, which then sees the user end.
	next := e.startPoll("", map[string]any{"agent_reply": "Made it bigger."})
	e.waitListening()
	if resp, _ := e.do("POST", "/api/"+e.key+"/end", nil, e.browser()); resp.StatusCode != 200 {
		t.Fatal("user end failed")
	}
	if r := awaitPoll(t, next); r.Status != "ended" || r.EndedBy != "user" {
		t.Fatalf("poll after user end = %+v", r)
	}
	chat, _ := e.m.Store.Transcript(context.Background(), e.key, 10)
	if last := chat[len(chat)-1]; last.Role != "agent" || last.Text != "Made it bigger." {
		t.Fatalf("transcript tail = %+v", last)
	}

	// A plain reopen is refused after the user ended.
	resp, data := e.do("POST", "/api/sessions", map[string]any{"file": e.file}, nil)
	var open OpenResponse
	_ = json.Unmarshal(data, &open)
	if resp.StatusCode != 200 || open.Status != "user-ended" {
		t.Fatalf("reopen: %d %s", resp.StatusCode, data)
	}
}

func TestPollListenerActive(t *testing.T) {
	e := newEnv(t)
	e.open()
	first := e.startPoll("&owner=first", nil)
	e.waitListening()

	if r := awaitPoll(t, e.startPoll("", nil)); r.Code != "LISTENER_ACTIVE" || r.Listener != "first" {
		t.Fatalf("second poll = %+v", r)
	}
	second := e.startPoll("&takeover=1&owner=second", map[string]any{})
	if r := awaitPoll(t, first); r.Code != "LISTENER_REPLACED" {
		t.Fatalf("displaced poll = %+v", r)
	}
	if _, data := e.do("POST", "/api/end", map[string]any{"file": e.file}, nil); !strings.Contains(string(data), `"agent"`) {
		t.Fatalf("agent end: %s", data)
	}
	if r := awaitPoll(t, second); r.Status != "ended" || r.EndedBy != "agent" {
		t.Fatalf("second poll after end = %+v", r)
	}
}

func TestPollMissingSession(t *testing.T) {
	e := newEnv(t)
	if r := awaitPoll(t, e.startPoll("", nil)); r.Code != "NOT_FOUND" {
		t.Fatalf("poll without a session = %+v", r)
	}
}

func TestEventsStream(t *testing.T) {
	e := newEnv(t)
	e.open()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/events/"+e.key, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := make(chan [2]string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var name string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				events <- [2]string{name, strings.TrimPrefix(line, "data: ")}
			}
		}
		close(events)
	}()
	next := func(want string) string {
		t.Helper()
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					t.Fatalf("stream closed waiting for %s", want)
				}
				if ev[0] == want || strings.Contains(ev[1], want) {
					return ev[1]
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("no %s event", want)
			}
		}
	}

	next(`"id":"ended-banner"`) // initial resync
	if n := e.m.Reviewers(e.key); n != 1 {
		t.Fatalf("reviewers = %d", n)
	}
	if resp, _ := e.do("POST", "/api/"+e.key+"/queue", map[string]any{"prompt": "Shorter <please>", "tag": "p"}, e.browser()); resp.StatusCode != 200 {
		t.Fatal("queue failed")
	}
	var region struct{ ID, HTML string }
	if err := json.Unmarshal([]byte(next(`Shorter`)), &region); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(region.HTML, `Shorter &lt;please&gt;`) {
		t.Fatalf("user text must be escaped in the region: %s", region)
	}

	write(t, e.file, "<body><h1>Plan v2</h1></body>")
	reload := next("reload")
	if !strings.Contains(reload, "rev=1") {
		t.Fatalf("reload = %s", reload)
	}

	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for e.m.Reviewers(e.key) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("closing the stream did not unregister the reviewer")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
