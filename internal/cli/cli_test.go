package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/riftsin/redline/internal/server"
	"github.com/riftsin/redline/internal/store"
)

func TestRunVersion(t *testing.T) {
	for _, flag := range []string{"--version", "-v", "-V"} {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{flag}, &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit %d, stderr %q", flag, code, stderr.String())
		}
		if got := strings.TrimSpace(stdout.String()); got != Version() {
			t.Fatalf("%s: printed %q, want %q", flag, got, Version())
		}
	}
}

func TestShouldReplace(t *testing.T) {
	cases := []struct {
		serverV, serverB, cliV, cliB string
		want                         bool
	}{
		{"v0.1.0", "v0.1.0", "v0.1.0", "v0.1.0", false},
		{"v0.1.0", "v0.1.0", "v0.2.0", "v0.2.0", true},  // newer CLI replaces
		{"v0.2.0", "v0.2.0", "v0.1.0", "v0.1.0", false}, // older CLI adopts
		{"v1.0.0-rc.1", "x", "v1.0.0", "y", true},       // release outranks its prerelease
		{"(devel)", "devel-a", "(devel)", "devel-b", true},
		{"(devel)", "devel-a", "(devel)", "devel-a", false},
		{"v0.1.0", "v0.1.0", "(devel)", "devel-a", true},
		{"v0.1.0+dirty", "v0.1.0+dirty-build-a", "v0.1.0+dirty", "v0.1.0+dirty-build-b", true}, // a local rebuild replaces
		{"v0.2.0", "v0.2.0", "v0.1.0+dirty", "v0.1.0+dirty-build-b", true},                     // a local build always takes over
	}
	for _, c := range cases {
		if got := shouldReplace(c.serverV, c.serverB, c.cliV, c.cliB); got != c.want {
			t.Errorf("shouldReplace(%+v) = %v", c, got)
		}
	}
}

func TestErrorsAndUsage(t *testing.T) {
	dir := t.TempDir()
	notHTML := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notHTML, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"frobnicate"}, 2, "redline: unknown command: frobnicate\nhint: run `redline --help`"},
		{[]string{"--no-open"}, 2, "flags must come after the command"},
		{[]string{"poll"}, 2, "an HTML file path is required"},
		{[]string{"poll", notHTML}, 2, "not an HTML file"},
		{[]string{"poll", filepath.Join(dir, "missing.html")}, 2, "does not exist"},
		{[]string{"playbook", "nope"}, 2, "unknown playbook: nope"},
		{[]string{"poll", "--bogus", "x.html"}, 2, "unknown or malformed flag"},
	} {
		var stdout, stderr bytes.Buffer
		code := Run(c.args, &stdout, &stderr)
		if code != c.code || !strings.Contains(stdout.String(), c.want) {
			t.Errorf("%v: exit %d, output %q; want exit %d containing %q", c.args, code, stdout.String(), c.code, c.want)
		}
	}
}

func TestHelpAndGuidanceCommands(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--help"}, "redline poll <html-file>"},
		{[]string{"poll", "--help"}, "Usage: redline poll"},
		{[]string{"playbook"}, "diagram"},
		{[]string{"playbook", "table"}, "Playbook: table"},
		{[]string{"design"}, "DAISYUI REFERENCE"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(c.args, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), c.want) {
			t.Errorf("%v: exit %d, output %.200q", c.args, code, stdout.String())
		}
	}
}

func TestPromptHeading(t *testing.T) {
	for _, c := range []struct {
		p    store.Prompt
		want string
	}{
		{store.Prompt{Kind: "message", Prompt: "hi"}, "Message"},
		{store.Prompt{Kind: "annotation", Tag: "h2", Text: "Rollout   plan"}, `On <h2> "Rollout plan"`},
		{store.Prompt{Kind: "annotation", Tag: "text", Text: "x", Target: json.RawMessage(`{"type":"text-range","text":"quoted words"}`)}, `On selected text "quoted words"`},
		{store.Prompt{Kind: "annotation", Tag: "td", Target: json.RawMessage(`{"type":"table-cell","rowLabel":"Redis","columnLabel":"Latency"}`)}, "On table cell Redis → Latency"},
		{store.Prompt{Kind: "answer", Tag: "input", Text: ""}, "Answer from <input>"},
	} {
		if got := promptHeading(c.p); got != c.want {
			t.Errorf("promptHeading(%+v) = %q, want %q", c.p, got, c.want)
		}
	}
}

// -- The real binary against a real background server --------------------------

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type binary struct {
	t    *testing.T
	path string
	env  []string
	port int
}

func buildBinary(t *testing.T) *binary {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the redline binary")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "redline")
	build := exec.Command("go", "build", "-o", path, "github.com/riftsin/redline")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	port := freePort(t)
	b := &binary{t: t, path: path, port: port, env: append(os.Environ(),
		"REDLINE_STATE_DIR="+filepath.Join(dir, "state"),
		"REDLINE_PORT="+strconv.Itoa(port),
		"REDLINE_HOST=127.0.0.1", // no Tailscale probing in tests
		"REDLINE_NO_OPEN=1",
	)}
	t.Cleanup(func() { b.run("stop") })
	return b
}

func (b *binary) cmd(args ...string) *exec.Cmd {
	c := exec.Command(b.path, args...)
	c.Env = b.env
	return c
}

func (b *binary) run(args ...string) (string, int) {
	b.t.Helper()
	var out bytes.Buffer
	c := b.cmd(args...)
	c.Stdout = &out
	c.Stderr = &out
	err := c.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		b.t.Fatal(err)
	}
	return out.String(), code
}

func (b *binary) post(path string, body any) {
	b.t.Helper()
	data, _ := json.Marshal(body)
	base := "http://127.0.0.1:" + strconv.Itoa(b.port)
	req, _ := http.NewRequest("POST", base+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", base) // as the review page sends it
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		b.t.Fatalf("POST %s: %d", path, resp.StatusCode)
	}
}

func TestAgentFlowWithRealServer(t *testing.T) {
	b := buildBinary(t)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	file := filepath.Join(dir, "plan.html")
	if err := os.WriteFile(file, []byte("<!doctype html><body><h1>Plan</h1></body>"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := b.run(file)
	if code != 0 || !strings.Contains(out, "Review session ready for "+file) || !strings.Contains(out, "Browser: not opened (REDLINE_NO_OPEN=1)") ||
		!strings.Contains(out, "Warning: This artifact never paints its own page surface") || !strings.Contains(out, "Next: First fix the unpainted page surface") ||
		!strings.Contains(out, "Do not respond to the user just yet") {
		t.Fatalf("open: exit %d\n%s", code, out)
	}
	key := strings.TrimSpace(out[strings.Index(out, "/session/")+len("/session/"):])
	key = strings.Fields(key)[0]

	// The poll waits until the reviewer sends.
	var pollOut bytes.Buffer
	poll := b.cmd("poll", file, "--owner", "test-agent")
	poll.Stdout = &pollOut
	var pollErr bytes.Buffer
	poll.Stderr = &pollErr
	if err := poll.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		home, _ := b.run()
		return strings.Contains(home, "listener: test-agent")
	})

	b.post("/api/"+key+"/queue", map[string]any{"prompt": "Make the title\nbigger", "tag": "h1", "selector": "body > h1", "text": "Plan"})
	b.post("/api/"+key+"/send", map[string]any{"message": "Thanks!", "snapshot": "body\n  h1 \"Plan\""})
	if err := poll.Wait(); err != nil {
		t.Fatalf("poll: %v\n%s\n%s", err, pollOut.String(), pollErr.String())
	}
	got := pollOut.String()
	for _, want := range []string{
		"Feedback on " + file + " (2 notes)",
		"1. On <h1> \"Plan\"\n   selector: body > h1\n   Make the title\n   bigger\n",
		"2. Message\n   Thanks!\n",
		"\nNext: Apply the requested changes to " + file,
		"Page outline when the user sent this:\n  body\n    h1 \"Plan\"\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("poll output lacks %q:\n%s", want, got)
		}
	}
	if !strings.Contains(pollErr.String(), "waiting for feedback") {
		t.Errorf("poll stderr = %q", pollErr.String())
	}

	if out, code := b.run("reply", file, "--agent-reply", "Bigger now."); code != 0 || !strings.Contains(out, "Reply accepted") {
		t.Fatalf("reply: exit %d\n%s", code, out)
	}
	if out, code := b.run("reply", file); code != 2 || !strings.Contains(out, "an agent reply is required") {
		t.Fatalf("reply without text: exit %d\n%s", code, out)
	}

	// Timeout escape hatch.
	if out, code := b.run("poll", file, "--timeout-ms", "100"); code != 0 || !strings.Contains(out, "No feedback for "+file+" yet") {
		t.Fatalf("poll timeout: exit %d\n%s", code, out)
	}

	// The user ends the session: the agent's poll learns it, and a plain reopen is refused.
	b.post("/api/"+key+"/end", nil)
	if out, code := b.run("poll", file); code != 0 || !strings.Contains(out, "the user ended the session") {
		t.Fatalf("poll after user end: exit %d\n%s", code, out)
	}
	if out, code := b.run(file); code != 0 || !strings.Contains(out, "Not reopened: the user ended") || !strings.Contains(out, "--reopen") {
		t.Fatalf("reopen refused: exit %d\n%s", code, out)
	}
	if out, code := b.run(file, "--reopen"); code != 0 || !strings.Contains(out, "Review session ready") {
		t.Fatalf("--reopen: exit %d\n%s", code, out)
	}
	if out, code := b.run("end", file); code != 0 || !strings.Contains(out, "Ended the review") {
		t.Fatalf("end: exit %d\n%s", code, out)
	}

	// Export needs no server; an asset next to the artifact is inlined.
	if err := os.WriteFile(filepath.Join(dir, "style.css"), []byte("body{background:#fff}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`<!doctype html><link rel="stylesheet" href="style.css"><body><h1>Plan</h1></body>`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = b.run("export", file)
	exported := filepath.Join(dir, "plan.export.html")
	data, _ := os.ReadFile(exported)
	if code != 0 || !strings.Contains(out, "Exported "+file) || !strings.Contains(string(data), "<style>body{background:#fff}</style>") || strings.Contains(out, "Warning:") {
		t.Fatalf("export: exit %d\n%s\n%s", code, out, data)
	}

	if out, code := b.run("stop"); code != 0 || !strings.Contains(out, "Stopped the redline server") {
		t.Fatalf("stop: exit %d\n%s", code, out)
	}
	if out, _ := b.run("stop"); !strings.Contains(out, "not running") {
		t.Fatalf("second stop: %s", out)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestFeedbackShowsImagesAndNextStep(t *testing.T) {
	var b strings.Builder
	pr := server.PollResponse{Status: "feedback", Prompts: []store.Prompt{{
		Kind: "message", Prompt: "",
		Attachments: []store.PromptAttachment{{Path: "/state/attachments/ab.png", Mime: "image/png", Width: 32, Height: 24, Name: "shot.png"}},
	}}}
	writeFeedback(&b, "/w/plan.html", pr)
	if want := "1. Message\n   image: /state/attachments/ab.png (png, 32x24, \"shot.png\")\n"; !strings.Contains(b.String(), want) {
		t.Fatalf("output lacks %q:\n%s", want, b.String())
	}
	out, err := pollOutput(&env{}, "/w/plan.html", 200, pr)
	if err != nil || !strings.Contains(out, "Open those image files") {
		t.Fatalf("next step lacks the image note: %v\n%s", err, out)
	}
}
