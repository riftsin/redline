// Package guidance is everything redline tells agents: the home view, help,
// per-command help, `design`, `playbook` and the next-step lines that end
// command output.
//
// The CLI is discovered by running it, so this output is the interface.
// Each rule lives here exactly once and every surface points at it; the
// installable skill stays a stub that points at these commands.
//
// Adapted from lavish-axi (MIT, Copyright (c) 2026 Kun Chen).
package guidance

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed playbooks.json
var playbooksJSON []byte

//go:embed daisyui.json
var daisyuiJSON []byte

var (
	playbookData = mustDecode(playbooksJSON)
	daisyuiData  = mustDecode(daisyuiJSON)
)

func mustDecode(data []byte) node {
	n, err := decodeOrdered(data)
	if err != nil {
		panic(fmt.Sprintf("guidance: %v", err))
	}
	return n
}

// Description is what redline is for, in one paragraph.
const Description = "redline turns rich HTML artifacts into a review loop between you and the user. " +
	"Whenever you are about to give the user a complex response that will be easier to understand " +
	"as a rich or interactive page, consider using redline. First generate an interactive HTML artifact " +
	"for the user's request, then run `redline <html-file>` so the user can review it visually, annotate " +
	"elements or selected text, queue notes, and send feedback back through `redline poll`."

var pollWakePathRules = []string{
	"Keep the poll in the foreground by default and let it return the feedback directly to the agent.",
	"A background poll is allowed only through a harness-native tracked background-job facility whose completion result is guaranteed to resume or notify the same agent.",
	"Never use `nohup`, shell `&`, `disown`, redirected fire-and-forget processes, or a detached terminal without an explicit verified callback merely to keep polling alive.",
	"If the harness has no completion-aware background facility, use the foreground poll or first wire a verified wake callback into the surrounding supervisor.",
	"Do not tell the user the artifact is being monitored until that wake path is live.",
	"If the poll gets killed or times out before feedback arrives, re-run it - feedback remains queued until delivery. Poll delivery consumes the response, so read it completely.",
	"If it returns browser_disconnected, the review window stayed disconnected past its reconnect grace period but the session remains resumable; ask the user whether to reopen or end it, and do neither uninvited.",
}

const codexGuidance = "Codex detected: completed background tasks may not resume Codex automatically, so keep the poll attached to the active turn."

// SendAndEndRule explains the user's Send & End button.
const SendAndEndRule = "`Send & End` ends the session. Its final feedback is still delivered once. After that response, polling stops, and the agent must not reopen the session uninvited."

// AgentReplyRule says how to write a reply.
const AgentReplyRule = "Keep the reply concise. Only when a longer reply is genuinely necessary, put a structured body in a file and pass it with `--agent-reply-file <path>` (`-` reads stdin) so newlines survive quoting."

const editRule = "Edit only what the feedback asks for: change the affected elements in place and leave the rest of the file untouched - do not regenerate, reorder, or reformat sections nobody asked about."

// DesignPriorityRule is how an agent picks an artifact's look.
const DesignPriorityRule = "Decide the design direction in this strict priority order, and only move to the next step when the current one truly yields nothing: (1) if the user asked for a specific look or named design system, use that; (2) otherwise you must first inspect the project the artifact is about - the subject or product whose content or UI it represents, which may differ from your current working directory - and match that project's design system: Tailwind or theme config, shared CSS variables or design tokens, component library, brand assets, or existing styled pages. If the artifact previews, proposes, or mocks a specific app's UI, render it in that app's own design system so it faithfully shows the product, even when you are running in a different repo; (3) only when both steps come up empty, use the Tailwind CSS browser runtime v4 + DaisyUI v5 snippet from `redline design`, which is dark by default and follows the viewer's light/dark preference, and prefer that snippet over hand-writing styles unless explicitly instructed otherwise by the user."

// Agent is the harness that invoked the CLI; only Codex needs extra poll
// guidance.
type Agent int

const (
	Generic Agent = iota
	Codex
)

// InvokingAgent detects the harness from its environment.
func InvokingAgent(getenv func(string) string) Agent {
	if getenv("CODEX_SANDBOX") != "" || getenv("CODEX_THREAD_ID") != "" {
		return Codex
	}
	return Generic
}

// PollExecution is how to run the long poll safely.
func PollExecution(agent Agent) string {
	s := strings.Join(pollWakePathRules, " ")
	if agent == Codex {
		s += " " + codexGuidance
	}
	return s
}

// DesignSystemHint is the short form of the design rules.
func DesignSystemHint() string {
	return "redline does not inject any design system - artifacts stay portable so they render identically " +
		"when opened directly without redline running. Before writing any HTML: " + DesignPriorityRule +
		" Run `redline design` for a content-to-playbook router, the copy-pasteable Tailwind + DaisyUI snippet, " +
		"and the DaisyUI component reference. When you deliver the artifact, state which of the three design " +
		"sources you used and why."
}

// -- Playbooks ------------------------------------------------------------------

// Playbook is one focused artifact guide.
type Playbook struct {
	ID      string
	UseWhen string
	data    node
}

// Playbooks lists every playbook in authored order.
func Playbooks() []Playbook {
	var out []Playbook
	for _, p := range playbookData.get("playbooks").list {
		out = append(out, Playbook{ID: p.get("id").str, UseWhen: p.get("use_when").str, data: p})
	}
	return out
}

// PlaybookIDs lists the playbook ids.
func PlaybookIDs() []string {
	var ids []string
	for _, p := range Playbooks() {
		ids = append(ids, p.ID)
	}
	return ids
}

// RouterHelp says to open every matching playbook.
func RouterHelp() string { return playbookData.get("router_help").str }

// RouterInstruction is the router's lead line.
func RouterInstruction() string { return playbookData.get("router_instruction").str }

// FindPlaybook returns the playbook with id.
func FindPlaybook(id string) (Playbook, bool) {
	for _, p := range Playbooks() {
		if p.ID == id {
			return p, true
		}
	}
	return Playbook{}, false
}

// Text renders the whole playbook.
func (p Playbook) Text() string {
	var b strings.Builder
	b.WriteString("Playbook: " + p.ID + "\n")
	for i, key := range p.data.keys {
		if key == "id" {
			continue
		}
		v := p.data.vals[i]
		if v.isStr {
			b.WriteString(label(key) + ": " + v.str + "\n")
			continue
		}
		b.WriteString("\n" + label(key) + ":\n")
		writeText(&b, v, "  ")
	}
	return b.String()
}

// PlaybookList renders the id and trigger of every playbook.
func PlaybookList(indent string) string {
	var b strings.Builder
	width := 0
	for _, p := range Playbooks() {
		width = max(width, len(p.ID))
	}
	for _, p := range Playbooks() {
		fmt.Fprintf(&b, "%s%-*s  %s\n", indent, width, p.ID, p.UseWhen)
	}
	return b.String()
}

// -- Design ----------------------------------------------------------------------

const layoutSafetySnippet = `<style>
  *, *::before, *::after { box-sizing: border-box; }
  :where(.grid, .flex, .layout-grid, .layout-flex) > *,
  :where([style*="display: grid"], [style*="display:grid"], [style*="display: flex"], [style*="display:flex"]) > * {
    min-width: 0;
  }
  :where(p, h1, h2, h3, h4, h5, h6, li, dd, blockquote, figcaption, td, th, .badge, .label) {
    overflow-wrap: anywhere;
  }
  :where(img, svg, video, canvas, iframe) {
    max-width: 100%;
    height: auto;
  }
</style>`

// Dark (coffee) by default; the light pair (autumn) only when the viewer's
// system prefers light. Runs before the stylesheets so the first paint has
// the right theme.
const themeSnippet = `<script>
  (() => {
    const light = matchMedia("(prefers-color-scheme: light)");
    const apply = () => (document.documentElement.dataset.theme = light.matches ? "autumn" : "coffee");
    apply();
    light.addEventListener("change", apply);
  })();
</script>`

const cdnDesignSnippet = themeSnippet + `
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/daisyui@5.5.19/daisyui.css">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/daisyui@5.5.19/themes.css">
<script src="https://cdn.jsdelivr.net/npm/@tailwindcss/browser@4.2.4/dist/index.global.js"></script>`

var themeUsage = []string{
	"Default to `<html data-theme=\"coffee\">` and keep the snippet's theme script: the page is dark by default and switches to `autumn` when the viewer's system prefers light. Only when the user asks for a specific theme or a fixed light or dark page, or the content clearly calls for another theme from the list below, drop the script and set that theme on `<html>`.",
	"Set a nested section theme with `<section data-theme=\"night\">`.",
	"Prefer semantic colors such as `bg-base-100`, `bg-base-200`, `text-base-content`, `bg-primary`, `text-primary-content`, `alert-warning`, and `btn-primary` so both the dark and the light theme stay readable.",
	"Avoid hardcoded Tailwind color names for text and surfaces unless the user asked for exact colors.",
	"Use Tailwind responsive prefixes such as `sm:`, `md:`, `lg:`, and `xl:` for layout changes.",
	"Never `@apply` DaisyUI classes (such as `text-base-content/40`, `bg-base-200`, or `btn`) inside `<style type=\"text/tailwindcss\">` - the Tailwind browser runtime does not know them, and one unknown utility aborts the entire compile, leaving the page with no Tailwind styles at all. Put DaisyUI classes directly on elements, or write plain CSS with theme variables such as `var(--color-base-200)`.",
}

// Design renders `redline design`.
func Design() string {
	var b strings.Builder
	b.WriteString("PLAYBOOK ROUTER\n")
	b.WriteString(RouterInstruction() + "\n\n")
	b.WriteString(PlaybookList("  "))

	b.WriteString("\nDESIGN FALLBACK\n")
	b.WriteString("Use this fallback only if (1) the user gave no design direction and (2) you already inspected the project the artifact is about and found no design system or style conventions to match. If you have not checked the subject project yet, check first. redline does not inject any design system; artifacts stay portable HTML. Paint an explicit page background and readable text. redline adds only its SDK script to your file and never wraps a fragment, so start it with `<!doctype html>`: without one the page renders in quirks mode, where `<table>` text stops inheriting the color a wrapper theme sets.\n\n")
	b.WriteString(DesignPriorityRule + "\n\n")
	b.WriteString("Paste this into your `<head>` and start the page with `<html data-theme=\"coffee\">` (Tailwind 4.2.4, DaisyUI 5.5.19):\n\n")
	writeBlock(&b, cdnDesignSnippet, "  ")
	b.WriteString("\nUse this command for common syntax. Read the latest DaisyUI docs (https://daisyui.com/components/) when using advanced or unfamiliar components. If the user asks for a different design system (Bootstrap, custom CSS, plain HTML, etc.), use that instead - redline does not require DaisyUI.\n")

	b.WriteString("\nTHEME USAGE\n")
	for _, rule := range themeUsage {
		writeBullet(&b, rule, "")
	}

	b.WriteString("\nLAYOUT SAFETY (optional)\n")
	b.WriteString("Copy-paste CSS for artifacts with dense nested grid/flex layouts, badges, wide monospace or pixel fonts, or local media. Paste it into the artifact yourself when useful; redline never injects it.\n\n")
	writeBlock(&b, layoutSafetySnippet, "  ")

	b.WriteString("\nDAISYUI REFERENCE\n")
	writeText(&b, daisyuiData, "")
	return b.String()
}

// -- Home and help ---------------------------------------------------------------

// Session is an open review session, for the home view.
type Session struct {
	File, Status, URL, Listener string
	PendingPrompts              int
}

var visualGuidance = []string{
	"Use visual hierarchy to make the most important decisions, risks, tradeoffs, and next actions obvious at a glance.",
	"Show, don't tell: explain concepts, flows, relationships, and comparisons with labeled illustrations - hand-authored inline SVG, per the diagram playbook - and show existing UI or state with screenshots of the real pages (run the app read-only if needed); reserve prose for what cannot be shown, such as rationale, trade-offs, and open questions.",
	"Structure the prose that remains with sections, cards, tables, annotated snippets, and side-by-side comparisons instead of long paragraphs.",
	"Choose typography, spacing, color, and layout deliberately so the artifact has a clear point of view.",
	"Prevent horizontal overflow at every nesting level: nested grid/flex children also need minmax(0, 1fr) tracks and min-width: 0, especially when badges, labels, or status text use wide pixel or monospace fonts; wrap, truncate, or contain long unbreakable text deliberately.",
}

// Home renders `redline` with no arguments.
func Home(sessions []Session, agent Agent) string {
	var b strings.Builder
	b.WriteString("redline - review HTML artifacts with your user\n\n")
	b.WriteString(Description + "\n")

	b.WriteString("\nOPEN SESSIONS\n")
	if len(sessions) == 0 {
		b.WriteString("  none\n")
	}
	for _, s := range sessions {
		listener := s.Listener
		if listener == "" {
			listener = "none"
		}
		fmt.Fprintf(&b, "  %s\n    status: %s, pending prompts: %d, listener: %s\n    %s\n", s.File, s.Status, s.PendingPrompts, listener, s.URL)
	}

	b.WriteString("\nVISUAL GUIDANCE\n")
	for _, g := range visualGuidance {
		writeBullet(&b, g, "")
	}

	b.WriteString("\nPLAYBOOKS\n")
	b.WriteString(PlaybookList("  "))

	b.WriteString("\nHOW TO USE\n")
	for _, h := range []string{
		"Run `redline <html-file>` to open or resume a review session. If the user explicitly ended the session from the browser, this refuses to reopen it and explains why instead of reopening uninvited - pass `--reopen` only when the user asks for further review or something important needs their visual attention.",
		"Unless the user specifies another location, create HTML artifacts in the current working directory under `.redline/`.",
		"redline serves the HTML file through a local server. If your HTML needs other local files such as images, CSS, fonts, and scripts, copy them into the same directory as the HTML file and reference them with relative paths from that directory. Never prepend `/` to those asset paths - root paths won't resolve.",
		"Run `redline poll <html-file>` to wait for user feedback. It long-polls and stays silent until the user sends feedback, ends the session, or leaves every review window disconnected past the reconnect grace period, so leave it running - never kill it. The only response that arrives without a user action is a fatal artifact failure, which means the review surface itself could not be used. " + PollExecution(agent) + " " + SendAndEndRule,
		"Run `redline reply <html-file> --agent-reply \"<message>\"` to show a concise reply and exit once redline accepts it, when you are handing a result back and are not about to long-poll. The review page stops showing Working only after this command exits 0. Use `redline poll <html-file> --agent-reply \"<message>\"` when that reply should be followed by another wait for feedback. A longer reply uses `--agent-reply-file <path>` (`-` reads stdin) on either command.",
		"Run `redline end <html-file>` to end a session as the agent - ending it this way still allows a plain reopen later. When the user ends it from the browser instead, a later `redline <html-file>` refuses to reopen it without `--reopen`.",
		"Run `redline stop` to shut down the background server.",
		"Run `redline playbook <playbook_id>` for focused artifact guidance. " + RouterHelp(),
		DesignSystemHint(),
		"Use redline when the user asks for a visual artifact, HTML explainer, interactive prototype, review surface, product or technical plan, comparison, report, or browser-based feedback loop.",
	} {
		writeBullet(&b, h, "")
	}
	return b.String()
}

// Help renders `redline --help`.
func Help(agent Agent) string {
	return `redline - review HTML artifacts with your user

Usage:
  redline
  redline <html-file> [--no-open] [--reopen]
  redline poll <html-file> [--owner <label>] [--takeover] [--agent-reply "..."] [--agent-reply-file <path>]
  redline reply <html-file> (--agent-reply "..." | --agent-reply-file <path>)
  redline end <html-file>
  redline stop
  redline playbook [playbook_id]
  redline design
  redline --version

` + DesignSystemHint() + `

Note: poll long-polls until the user sends feedback, ends the session, or leaves every review window disconnected past the reconnect grace period, staying silent while it waits - never kill it. Do not pass --timeout-ms during normal agent use; it is for tests and debugging only. ` + PollExecution(agent) + " " + SendAndEndRule + " Use `redline reply` when handing a result back without waiting for more feedback: it exits 0 only after the server accepts the reply, so the review page stops showing Working. `poll --agent-reply` posts a reply and then keeps waiting.\n"
}

// CommandHelp renders `redline <command> --help`, or "" for an unknown
// command.
func CommandHelp(command string, agent Agent) string {
	switch command {
	case "open":
		return `Usage: redline <html-file> [--no-open] [--reopen]

Open or resume a redline review session for an HTML artifact. Use --no-open (or REDLINE_NO_OPEN=1) when you need to ensure the server and session exist without opening another browser window. If the user explicitly ended the session from the browser, this refuses to reopen it and returns guidance instead - pass --reopen to force it open when the user asks for further review or something important needs their visual attention. Sessions ended by the agent (` + "`redline end`" + `) reopen normally without the flag.
`
	case "poll":
		return `Usage: redline poll <html-file> [--owner <label>] [--takeover] [--agent-reply "..."] [--agent-reply-file <path>]

Long-poll for the user's feedback. It stays silent while it waits - that is normal, never kill it. Pass --owner <label> to make the active listener visible in session listings; --takeover displaces an existing listener, which then fails with LISTENER_REPLACED. A second poll without --takeover fails with LISTENER_ACTIVE instead of silently waiting. The only response that arrives without user action is a fatal artifact failure that made the review surface itself unusable. Do not pass --timeout-ms during normal agent use; it is for tests and debugging only. ` + PollExecution(agent) + ` Use --agent-reply after applying prior feedback to display a concise response in the review page before waiting again. When you are handing a result back and are not about to long-poll, run ` + "`redline reply <html-file> --agent-reply \"...\"`" + ` instead: it exits 0 only after the server accepts the reply. ` + AgentReplyRule + ` Do not combine --agent-reply with --agent-reply-file.

Examples:
  redline poll report.html --agent-reply "Renamed the payment step."
  redline poll report.html --agent-reply-file reply.md
  redline poll report.html --agent-reply-file -

` + SendAndEndRule + "\n"
	case "reply":
		return `Usage: redline reply <html-file> (--agent-reply "..." | --agent-reply-file <path>)

Post an agent reply to the open review session and exit once the server confirms it was accepted, which is when the review page stops showing Working. If that answer does not arrive within 10 seconds, exit non-zero with a timeout error. Use this when you are handing a result back and are not about to wait for more feedback.

Use ` + "`redline poll <html-file> --agent-reply \"...\"`" + ` instead when the reply should be followed by a long-poll.

Pass exactly one of --agent-reply or --agent-reply-file. ` + AgentReplyRule + ` An empty reply is refused.

Examples:
  redline reply report.html --agent-reply "Renamed the payment step."
  redline reply report.html --agent-reply-file reply.md
  redline reply report.html --agent-reply-file -
`
	case "end":
		return `Usage: redline end <html-file>

End a review session as the agent. A session ended this way still reopens normally on the next ` + "`redline <html-file>`" + `, unlike a user ending it from the browser, which requires --reopen.
`
	case "stop":
		return `Usage: redline stop

Shut down the background redline server. Open review pages reconnect on their own when it starts again.
`
	case "playbook":
		return `Usage: redline playbook [playbook_id]

List focused artifact guidance playbooks, or show one playbook by ID. Known IDs: ` + strings.Join(PlaybookIDs(), ", ") + `.

` + RouterHelp() + `

Examples:
  redline playbook
  redline playbook diagram
  redline playbook input
`
	case "design":
		return `Usage: redline design

Show the copy-pasteable Tailwind CSS browser runtime v4 + DaisyUI v5 snippet, a content-to-playbook router, an optional layout safety CSS snippet, and a DaisyUI component reference. ` + RouterHelp() + ` The snippet is the design fallback, not the default: inspect the subject project before falling back. ` + DesignPriorityRule + "\n"
	}
	return ""
}

// -- Next steps ------------------------------------------------------------------

// OpenNextStep follows a successful `redline <html-file>`.
func OpenNextStep(file string, agent Agent, networkWarning bool) string {
	network := ""
	if networkWarning {
		network = "redline could not serve every address it was asked to (see the network warning above): when you next message the user, tell them, because review links at that address will not load until it clears. "
	}
	return network + "Do not respond to the user just yet. Now you must run `redline poll " + file + "`. This command long-polls until the user sends feedback, ends the session, or leaves every review window disconnected past the reconnect grace period, and it stays silent the whole time - that is normal, never kill it. Do not pass --timeout-ms during normal agent use. " + PollExecution(agent) + " After applying feedback, run `redline poll " + file + " --agent-reply \"<message for the user>\"` without --timeout-ms to show a concise response in the review page and keep waiting for more feedback. If instead you are handing back a result without starting another long-poll, run `redline reply " + file + " --agent-reply \"<message for the user>\"` to get an acceptance receipt. " + AgentReplyRule + " If the user ends the session, stop polling and do not reopen it by re-running `redline " + file + "` unless the user asks for further review or something genuinely important needs their visual attention - deliver routine updates directly in this conversation instead. When reopening is warranted, run `redline " + file + " --reopen`."
}

// UserEndedOpenNextStep follows an open refused because the user ended the
// session.
func UserEndedOpenNextStep(file string) string {
	return "The user explicitly ended this review session from the browser, so it was not reopened. Deliver your update directly in this conversation instead. Only run `redline " + file + " --reopen` if the user asks for further review or something genuinely important needs their visual attention."
}

// Failure is a fatal artifact failure, for next-step texts.
type Failure struct{ Kind, Detail string }

const imageNote = "Some notes carry images the user attached: each `image:` line is an absolute local file path. Open those image files to see what the user is referring to. "

// FeedbackNextStep follows delivered feedback.
func FeedbackNextStep(file string, failures []Failure, sessionEnded bool, endedBy string, images bool, agent Agent) string {
	note := failureNote(file, failures, sessionEnded, endedBy)
	if images {
		note += imageNote
	}
	switch {
	case sessionEnded && endedBy == "user":
		reopen := ""
		if len(failures) == 0 {
			reopen = " Only run `redline " + file + " --reopen` if the user explicitly asks for further review or something genuinely important needs their visual attention."
		}
		return note + "This was the last feedback before the user ended the session. Stop polling " + file + " and do not reopen it - deliver any remaining updates directly in this conversation instead." + reopen
	case sessionEnded:
		return note + "This was the last feedback before the session ended. Stop polling " + file + ". Deliver any remaining updates directly in this conversation, or run `redline " + file + "` to open a fresh session if the user needs further visual review."
	}
	prefix := note
	if len(failures) == 0 {
		prefix = "Apply the requested changes to " + file + ". " + editRule + " " + note
	}
	return prefix + "Do not respond to the user just yet. If you are continuing to wait for feedback, run `redline poll " + file + " --agent-reply \"<message for the user>\"` without --timeout-ms to reply and keep polling. If instead you are handing back a result without starting another long-poll, run `redline reply " + file + " --agent-reply \"<message for the user>\"` to post it and receive an acceptance receipt. " + AgentReplyRule + " The poll waits silently until the user sends more feedback, ends the session, or leaves every review window disconnected past the reconnect grace period - never kill it. " + PollExecution(agent)
}

func failureNote(file string, failures []Failure, sessionEnded bool, endedBy string) string {
	if len(failures) == 0 {
		return ""
	}
	var details []string
	for i, f := range failures {
		if i == 5 {
			break
		}
		details = append(details, f.Kind+": "+f.Detail)
	}
	plural := "s"
	if len(failures) == 1 {
		plural = ""
	}
	var fix string
	switch {
	case sessionEnded && endedBy == "user":
		fix = "Repair " + file + ", then open it directly and confirm it renders without reopening this ended session. "
	case sessionEnded:
		fix = "Repair " + file + ", then run `redline " + file + "` to open a fresh session. "
	default:
		fix = "Repair " + file + " so it renders with all of its local assets, then re-check in the browser. redline live-reloads the artifact automatically after you save, so you do not need to re-run `redline " + file + "` for this. "
	}
	return fmt.Sprintf("%d fatal artifact failure%s detected - the review surface could not be used (%s). ", len(failures), plural, strings.Join(details, "; ")) + fix
}

// EndedNextStep follows a poll that found the session ended.
func EndedNextStep(file, endedBy string) string {
	if endedBy == "user" {
		return "The user ended this review session. Stop polling " + file + " - do not run `redline " + file + "` to reopen it. Deliver any remaining updates directly in this conversation instead. Only reopen with `redline " + file + " --reopen` if the user explicitly asks for further review or something genuinely important needs their visual attention."
	}
	return "This review session for " + file + " has ended. Stop polling. Deliver any remaining updates directly in this conversation, or run `redline " + file + "` to open a fresh session if the user needs further visual review."
}

// BrowserDisconnectedNextStep follows a poll released by the disconnect
// grace.
const BrowserDisconnectedNextStep = "The review window was closed or disconnected. The session remains open and resumable; ask the user whether they want to reopen it or end the session. Do not reopen or end it without their direction."

// WaitingNextStep follows a poll that hit its --timeout-ms.
func WaitingNextStep(file string) string {
	return "No user feedback arrived before the optional timeout. Run `redline poll " + file + "` without --timeout-ms to wait indefinitely - feedback remains queued until delivery, so re-running the poll is safe while waiting. Poll delivery consumes the response, so read it completely."
}

// ReplyNextStep follows an accepted `redline reply`.
func ReplyNextStep(file string) string {
	return "redline accepted the reply for " + file + " and is no longer showing Working. Run `redline poll " + file + "` when you are ready to wait for more feedback."
}
