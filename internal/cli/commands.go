package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/riftsin/redline/internal/export"
	"github.com/riftsin/redline/internal/guidance"
	"github.com/riftsin/redline/internal/integrations"
	"github.com/riftsin/redline/internal/selfpaint"
	"github.com/riftsin/redline/internal/server"
	"github.com/riftsin/redline/internal/store"
)

const replyTimeout = 10 * time.Second

// -- home ---------------------------------------------------------------------

func homeCommand(e *env) (string, error) {
	var sessions []guidance.Session
	// Sessions come from a running server; without one nothing is live.
	if result, _ := probe(); result == running {
		var list struct {
			Sessions []server.SessionSummary `json:"sessions"`
		}
		if status, err := callWithTimeout(5*time.Second, "GET", "/api/sessions", nil, &list); err == nil && status == http.StatusOK {
			for _, s := range list.Sessions {
				sessions = append(sessions, guidance.Session{File: s.File, Status: s.Status, URL: s.URL, Listener: s.Listener, PendingPrompts: s.PendingPrompts})
			}
		}
	}
	return guidance.Home(sessions, e.agent), nil
}

// -- open ---------------------------------------------------------------------

func openCommand(e *env, args []string) (string, error) {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	noOpen := fs.Bool("no-open", false, "")
	reopen := fs.Bool("reopen", false, "")
	file, err := fileArg(fs, args, "redline <html-file>")
	if err != nil {
		return "", err
	}
	unpainted := false
	if src, err := os.ReadFile(file); err == nil {
		painted, _ := selfpaint.Painted(src)
		unpainted = !painted
	}
	if err := e.ensureServer(); err != nil {
		return "", err
	}
	var resp server.OpenResponse
	status, err := callWithTimeout(10*time.Second, "POST", "/api/sessions", map[string]any{"file": file, "reopen": *reopen}, &resp)
	if err != nil || status != http.StatusOK {
		return "", serverError(status, err)
	}

	var b strings.Builder
	if resp.Status == "user-ended" {
		fmt.Fprintf(&b, "Not reopened: the user ended the review of %s.\n", file)
		fmt.Fprintf(&b, "URL: %s\n", resp.URL)
		writeWarning(&b, resp.NetworkWarning)
		b.WriteString("\nNext: " + guidance.UserEndedOpenNextStep(file) + "\n")
		return b.String(), nil
	}

	browser := "not opened (--no-open)"
	switch {
	case *noOpen:
	case e.getenv("REDLINE_NO_OPEN") == "1":
		browser = "not opened (REDLINE_NO_OPEN=1)"
	case e.openBrowser(resp.URL):
		browser = "opened"
	default:
		browser = "could not be opened; give the user the URL"
	}
	fmt.Fprintf(&b, "Review session ready for %s\n", file)
	fmt.Fprintf(&b, "URL: %s\n", resp.URL)
	fmt.Fprintf(&b, "Browser: %s\n", browser)
	writeWarning(&b, resp.NetworkWarning)
	if unpainted {
		b.WriteString("Warning: " + selfpaint.Warning + "\n")
	}
	b.WriteString("\nNext: " + guidance.OpenNextStep(file, e.agent, resp.NetworkWarning != "", unpainted) + "\n")
	return b.String(), nil
}

func writeWarning(b *strings.Builder, warning string) {
	if warning != "" {
		b.WriteString("Network warning: " + warning + "\n")
	}
}

// systemOpen opens url in the default browser.
func systemOpen(url string) bool {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	go func() { _ = cmd.Wait() }()
	return true
}

// -- poll ---------------------------------------------------------------------

func replyText(e *env, text, file string, required bool) (string, error) {
	switch {
	case text != "" && file != "":
		return "", usageError("pass either --agent-reply or --agent-reply-file, not both")
	case file == "-":
		b, err := io.ReadAll(e.stdin)
		if err != nil {
			return "", failure("cannot read the reply from stdin: " + err.Error())
		}
		text = string(b)
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", usageError("cannot read " + file + ": " + err.Error())
		}
		text = string(b)
	case required && text == "":
		return "", usageError("an agent reply is required",
			"pass exactly one of --agent-reply \"<message>\" or --agent-reply-file <path> (`-` reads stdin)")
	}
	if (text != "" || file != "") && strings.TrimSpace(text) == "" {
		return "", usageError("the agent reply text is empty", "pass a non-empty reply")
	}
	return text, nil
}

func pollCommand(e *env, args []string) (string, error) {
	fs := flag.NewFlagSet("poll", flag.ContinueOnError)
	owner := fs.String("owner", "", "")
	takeover := fs.Bool("takeover", false, "")
	agentReply := fs.String("agent-reply", "", "")
	agentReplyFile := fs.String("agent-reply-file", "", "")
	timeoutMS := fs.Int("timeout-ms", -1, "")
	file, err := fileArg(fs, args, "redline poll <html-file>")
	if err != nil {
		return "", err
	}
	reply, err := replyText(e, *agentReply, *agentReplyFile, false)
	if err != nil {
		return "", err
	}
	if err := e.ensureServer(); err != nil {
		return "", err
	}

	q := url.Values{"file": {file}}
	if *owner != "" {
		q.Set("owner", *owner)
	}
	if *takeover {
		q.Set("takeover", "1")
	}
	if *timeoutMS >= 0 {
		q.Set("timeout_ms", strconv.Itoa(*timeoutMS))
	}
	method := http.MethodGet
	var body io.Reader
	if reply != "" || *takeover {
		method = http.MethodPost
		payload := map[string]any{}
		if reply != "" {
			payload["agent_reply"] = reply
		}
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}

	// An interrupted poll loses nothing: say so, then exit like a signal.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		if sig, ok := <-signals; ok {
			fmt.Fprintf(e.stderr, "\nredline: poll interrupted. Re-run `redline poll %s` - queued feedback is never lost.\n", file)
			if sig == os.Interrupt {
				os.Exit(130)
			}
			os.Exit(143)
		}
	}()

	stopReporter := e.startWaitReporter(file)
	defer stopReporter()

	req, err := http.NewRequestWithContext(ctx, method, baseURL()+"/api/poll?"+q.Encode(), body)
	if err != nil {
		return "", failure(err.Error())
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", interrupted(file)
	}
	defer resp.Body.Close()
	// Only an indefinite poll that actually starts waiting chimes.
	if resp.Header.Get(server.PollStateHeader) == "listening" && *timeoutMS < 0 && integrations.HerdrChimeEnabled(e.getenv) {
		integrations.HerdrChime()
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", interrupted(file)
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return "", interrupted(file)
	}
	var pr server.PollResponse
	if err := json.Unmarshal(data, &pr); err != nil {
		return "", serverError(resp.StatusCode, fmt.Errorf("unexpected poll response: %s", data))
	}
	return pollOutput(e, file, resp.StatusCode, pr)
}

func interrupted(file string) error {
	return failure("the poll was interrupted before feedback arrived (the server restarted or stopped)",
		"re-run `redline poll "+file+"` - queued feedback is never lost and remains queued until delivery")
}

// startWaitReporter tells a human watching the terminal that the silence is
// expected. Repeated ticks go to stderr only on a terminal.
func (e *env) startWaitReporter(file string) func() {
	fmt.Fprintf(e.stderr, "redline: waiting for feedback on %s - this is normal; leave it running\n", file)
	f, ok := e.stderr.(*os.File)
	if !ok {
		return func() {}
	}
	if info, err := f.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return func() {}
	}
	done := make(chan struct{})
	start := time.Now()
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				fmt.Fprintf(e.stderr, "redline: still waiting (%dm)\n", int(time.Since(start).Minutes()))
			}
		}
	}()
	return func() { close(done) }
}

func pollOutput(e *env, file string, status int, pr server.PollResponse) (string, error) {
	switch {
	case pr.Code == "NOT_FOUND" || status == http.StatusNotFound:
		return "", noSession(file)
	case pr.Code == "LISTENER_ACTIVE":
		return "", failure(pr.Error,
			"another poll ("+pr.Listener+") already listens for "+file+"; do not start a second one",
			"pass --takeover only when you are sure the other listener is gone")
	case pr.Code == "LISTENER_REPLACED":
		return "", failure(pr.Error, "another poll took over this session; stop polling from this process")
	case pr.Code == "VALIDATION_ERROR":
		return "", usageError(pr.Error)
	case pr.Status == "error":
		return "", failure(pr.Error, "run `redline stop`, then retry")
	}

	var b strings.Builder
	switch pr.Status {
	case "feedback":
		writeFeedback(&b, file, pr)
		failures := make([]guidance.Failure, len(pr.ArtifactFailures))
		for i, f := range pr.ArtifactFailures {
			failures[i] = guidance.Failure{Kind: f.Kind, Detail: f.Detail}
		}
		images, layoutFixes := false, false
		for _, p := range pr.Prompts {
			images = images || len(p.Attachments) > 0
			layoutFixes = layoutFixes || p.Kind == "layout"
		}
		next := guidance.FeedbackNextStep(file, failures, pr.SessionEnded, pr.EndedBy, images, e.agent)
		if layoutFixes {
			next = guidance.LayoutNote + next
		}
		b.WriteString("\nNext: " + next + "\n")
		if pr.DOMSnapshot != "" {
			b.WriteString("\nPage outline when the user sent this:\n")
			for _, line := range strings.Split(pr.DOMSnapshot, "\n") {
				b.WriteString("  " + line + "\n")
			}
		}
	case "ended":
		who := "the session ended"
		if pr.EndedBy == "user" {
			who = "the user ended the session"
		}
		fmt.Fprintf(&b, "Review of %s is over: %s.\n\nNext: %s\n", file, who, guidance.EndedNextStep(file, pr.EndedBy))
	case "browser_disconnected":
		fmt.Fprintf(&b, "Every review window for %s disconnected.\n\nNext: %s\n", file, guidance.BrowserDisconnectedNextStep)
	case "waiting":
		fmt.Fprintf(&b, "No feedback for %s yet (timeout).\n\nNext: %s\n", file, guidance.WaitingNextStep(file))
	default:
		return "", serverError(status, fmt.Errorf("unexpected poll status %q", pr.Status))
	}
	return b.String(), nil
}

// writeFeedback renders delivered prompts as numbered notes.
func writeFeedback(b *strings.Builder, file string, pr server.PollResponse) {
	n := len(pr.Prompts)
	noun := "notes"
	if n == 1 {
		noun = "note"
	}
	fmt.Fprintf(b, "Feedback on %s (%d %s)", file, n, noun)
	if pr.SessionEnded {
		if pr.EndedBy == "user" {
			b.WriteString(" - the user then ended the session")
		} else {
			b.WriteString(" - the session then ended")
		}
	}
	b.WriteString("\n")

	for i, p := range pr.Prompts {
		fmt.Fprintf(b, "\n%d. %s\n", i+1, promptHeading(p))
		if p.Selector != "" && p.Kind != "message" {
			b.WriteString("   selector: " + p.Selector + "\n")
		}
		if strings.TrimSpace(p.Prompt) != "" {
			for _, line := range strings.Split(strings.TrimRight(p.Prompt, "\n"), "\n") {
				b.WriteString("   " + line + "\n")
			}
		}
		for _, a := range p.Attachments {
			kind := strings.TrimPrefix(a.Mime, "image/")
			fmt.Fprintf(b, "   image: %s (%s, %dx%d", a.Path, kind, a.Width, a.Height)
			if a.Name != "" {
				fmt.Fprintf(b, ", %q", a.Name)
			}
			b.WriteString(")\n")
		}
	}
	if len(pr.ArtifactFailures) > 0 {
		b.WriteString("\nFatal artifact failures (the review surface could not be used):\n")
		for _, f := range pr.ArtifactFailures {
			b.WriteString("  - " + f.Kind + ": " + f.Detail + "\n")
		}
	}
}

// promptHeading says what a note is attached to, in the reviewer's terms.
func promptHeading(p store.Prompt) string {
	var target struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		RowLabel    string `json:"rowLabel"`
		ColumnLabel string `json:"columnLabel"`
		NodeID      string `json:"nodeId"`
		Label       string `json:"label"`
		DiagramID   string `json:"diagramId"`
	}
	if len(p.Target) > 0 {
		_ = json.Unmarshal(p.Target, &target)
	}
	quote := func(s string) string {
		s = strings.Join(strings.Fields(s), " ")
		if r := []rune(s); len(r) > 120 {
			s = string(r[:119]) + "…"
		}
		return "\"" + s + "\""
	}
	switch {
	case p.Kind == "message":
		return "Message"
	case p.Kind == "layout":
		return "Layout issues the user asked you to fix (" + p.Text + ")"
	case target.Type == "mermaid-node":
		h := "On diagram node"
		if target.Label != "" {
			h += " " + quote(target.Label)
		}
		if target.NodeID != "" {
			h += " (node id " + target.NodeID
			if target.DiagramID != "" {
				h += " in " + target.DiagramID
			}
			h += ")"
		}
		return h
	case target.Type == "text-range":
		text := target.Text
		if text == "" {
			text = p.Text
		}
		return "On selected text " + quote(text)
	case target.Type == "table-cell" && (target.RowLabel != "" || target.ColumnLabel != ""):
		var parts []string
		for _, s := range []string{target.RowLabel, target.ColumnLabel} {
			if s != "" {
				parts = append(parts, s)
			}
		}
		return "On table cell " + strings.Join(parts, " → ")
	}
	prefix := "On"
	if p.Kind == "answer" {
		prefix = "Answer from"
	}
	h := prefix + " <" + p.Tag + ">"
	if p.Text != "" {
		h += " " + quote(p.Text)
	}
	return h
}

// -- reply / end / stop ---------------------------------------------------------

func replyCommand(e *env, args []string) (string, error) {
	fs := flag.NewFlagSet("reply", flag.ContinueOnError)
	agentReply := fs.String("agent-reply", "", "")
	agentReplyFile := fs.String("agent-reply-file", "", "")
	file, err := fileArg(fs, args, "redline reply <html-file> --agent-reply \"...\"")
	if err != nil {
		return "", err
	}
	text, err := replyText(e, *agentReply, *agentReplyFile, true)
	if err != nil {
		return "", err
	}
	if err := e.ensureServer(); err != nil {
		return "", err
	}
	var resp apiError
	status, err := callWithTimeout(replyTimeout, "POST", "/api/reply", map[string]string{"file": file, "text": text}, &resp)
	switch {
	case err != nil && status == 0:
		return "", failure(fmt.Sprintf("redline did not confirm the reply within %d seconds", int(replyTimeout.Seconds())),
			"re-run the same `redline reply` command; the review page may still show Working")
	case status == http.StatusOK:
		return "Reply accepted for " + file + ".\n\nNext: " + guidance.ReplyNextStep(file) + "\n", nil
	case status == http.StatusConflict:
		return "", failure("the review session for "+file+" has ended; the reply was not shown", guidance.EndedNextStep(file, resp.EndedBy))
	case status == http.StatusNotFound:
		return "", noSession(file)
	case status == http.StatusBadRequest:
		return "", usageError(resp.Error)
	}
	return "", serverError(status, err)
}

func endCommand(e *env, args []string) (string, error) {
	file, err := fileArg(flag.NewFlagSet("end", flag.ContinueOnError), args, "redline end <html-file>")
	if err != nil {
		return "", err
	}
	if err := e.ensureServer(); err != nil {
		return "", err
	}
	var resp apiError
	status, err := callWithTimeout(10*time.Second, "POST", "/api/end", map[string]string{"file": file}, &resp)
	switch {
	case status == http.StatusOK:
		if resp.EndedBy == "user" {
			return "Ended the review of " + file + ". The user had already ended it.\n", nil
		}
		return "Ended the review of " + file + ". A plain `redline " + file + "` reopens it later.\n", nil
	case status == http.StatusNotFound:
		return "", noSession(file)
	}
	return "", serverError(status, err)
}

func stopCommand(e *env, args []string) (string, error) {
	positional, err := parse(flag.NewFlagSet("stop", flag.ContinueOnError), args)
	if err != nil {
		return "", err
	}
	if len(positional) > 0 {
		return "", usageError("unexpected arguments: " + strings.Join(positional, " "))
	}
	switch result, h := probe(); result {
	case notRunning:
		return "The redline server is not running.\n", nil
	case foreign:
		return "", foreignError(h)
	}
	_, _ = callWithTimeout(3*time.Second, "POST", "/shutdown", map[string]string{"reason": "stop"}, nil)
	if err := awaitPortFree(); err != nil {
		return "", err
	}
	return "Stopped the redline server.\n", nil
}

// -- export ---------------------------------------------------------------------

func exportCommand(e *env, args []string) (string, error) {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	outFlag := fs.String("out", "", "")
	file, err := fileArg(fs, args, "redline export <html-file> [--out <path>]")
	if err != nil {
		return "", err
	}
	out := *outFlag
	if out == "" {
		out = filepath.Join(filepath.Dir(file), export.FileName(file))
	}
	out, err = filepath.Abs(out)
	if err != nil {
		return "", usageError(err.Error())
	}
	if out == file {
		return "", usageError("--out must not overwrite the artifact itself")
	}
	data, warnings, err := export.Bundle(file, export.LimitsFromEnv(e.getenv))
	if err != nil {
		return "", failure("cannot export " + file + ": " + err.Error())
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return "", usageError("cannot write " + out + ": " + err.Error())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Exported %s\nto %s (%d KB)\n", file, out, (len(data)+1023)/1024)
	if len(warnings) > 0 {
		b.WriteString("\nNot inlined:\n")
		for _, w := range warnings {
			b.WriteString("  - " + w.Kind + ": " + w.Detail + "\n")
		}
	}
	if painted, _ := selfpaint.Painted(data); !painted {
		b.WriteString("\nWarning: " + selfpaint.Warning + "\n")
	}
	b.WriteString("\nNext: The portable copy is at " + out + ". Local assets are inlined; remote (https) references still need the network.\n")
	return b.String(), nil
}

// -- guidance -------------------------------------------------------------------

func designCommand(e *env, args []string) (string, error) {
	if positional, err := parse(flag.NewFlagSet("design", flag.ContinueOnError), args); err != nil {
		return "", err
	} else if len(positional) > 0 {
		return "", usageError("unexpected arguments: " + strings.Join(positional, " "))
	}
	return guidance.Design(), nil
}

func playbookCommand(e *env, args []string) (string, error) {
	positional, err := parse(flag.NewFlagSet("playbook", flag.ContinueOnError), args)
	if err != nil {
		return "", err
	}
	switch len(positional) {
	case 0:
		return "PLAYBOOKS\n" + guidance.PlaybookList("  ") + "\nRun `redline playbook <playbook_id>` for focused artifact guidance. " + guidance.RouterHelp() + "\n", nil
	case 1:
		p, ok := guidance.FindPlaybook(positional[0])
		if !ok {
			return "", usageError("unknown playbook: "+positional[0], "known IDs: "+strings.Join(guidance.PlaybookIDs(), ", "))
		}
		return p.Text(), nil
	}
	return "", usageError("pass at most one playbook id")
}
