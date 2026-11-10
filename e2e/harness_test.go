//go:build e2e

package e2e

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestHarnessClicksInsideSandboxedFrame proves the harness can do what every
// redline e2e test needs: load a page whose content sits in a sandboxed,
// opaque-origin iframe, click a real element inside it, and observe the
// postMessage the frame sends to its parent.
func TestHarnessClicksInsideSandboxedFrame(t *testing.T) {
	var (
		mu       sync.Mutex
		received []string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html>
<style>body{margin:0} iframe{position:absolute;left:40px;top:60px;width:600px;height:400px;border:3px solid #888}</style>
<iframe src="/frame" sandbox="allow-scripts"></iframe>
<script>
addEventListener("message", (e) => {
  if (e.source !== document.querySelector("iframe").contentWindow) return;
  fetch("/record", {method: "POST", body: e.data});
});
</script>`)
	})
	mux.HandleFunc("GET /frame", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts")
		io.WriteString(w, `<!doctype html>
<style>body{margin:0;padding:120px 0 0 200px}</style>
<button id="target">Target</button>
<p id="decoy">Decoy</p>
<script>
document.addEventListener("click", (e) => parent.postMessage("clicked:" + e.target.id, "*"));
</script>`)
	})
	mux.HandleFunc("POST /record", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, string(body))
		mu.Unlock()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	page := Launch(t).NewPage(t)
	page.Navigate(srv.URL + "/")

	var origin string
	page.EvalInFrame("/frame", "location.origin + '|' + window.origin", &origin)
	if origin != srv.URL+"|null" {
		t.Fatalf("frame origin = %q, want the server URL with an opaque window.origin", origin)
	}

	page.ClickInFrame("/frame", "#target")

	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) > 0
	})
	mu.Lock()
	defer mu.Unlock()
	if received[0] != "clicked:target" {
		t.Fatalf("frame reported %q, want clicked:target", received[0])
	}
}
