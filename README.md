# redline

A local review loop between you and your coding agent, over one HTML file.

Your agent writes a rich HTML artifact (a plan, a comparison, a diagram, a report) and opens it with
`redline <file.html>`. You review it in the browser: point at elements, select text, answer the
artifact's own controls, and chat. Your feedback reaches the agent, precisely anchored, through
`redline poll`. The agent edits the file, the page live-reloads, and the loop continues until either
side ends the session.

redline is a Go rewrite of [lavish-axi](https://github.com/kunchenguid/lavish-axi). It runs entirely
on your machine (with optional review from your other devices over Tailscale) and makes no outbound
network requests.

## Install

```sh
go install github.com/riftsin/redline@latest
```

Any agent that can run shell commands can use redline: ask it to use `redline`. Running `redline`
with no arguments prints what an agent needs to get started.

## How it works

```
agent writes artifact.html
  └─ redline artifact.html        starts the background server, opens the review page
       └─ you annotate, answer, chat, then Send to Agent
            └─ redline poll artifact.html   returns your feedback to the agent
                 └─ agent edits the file → the page live-reloads → redline poll --agent-reply "…"
```

- **The artifact stays the author's.** It is served in a sandboxed iframe byte-for-byte, plus one
  injected `<script>` (the SDK). Opened directly, the file renders the same.
- **The SDK is a sensor.** It reports the element you clicked, the text you selected, answers the
  artifact queues through `window.redline.queuePrompt()`, scroll position and local assets that failed
  to load. Everything else (your queued notes, the conversation, agent presence) is server state,
  identical in every open tab and on your phone.
- **Nothing interrupts you.** Only your explicit Send wakes the agent; the one exception is an
  artifact that cannot be reviewed at all (missing file or local asset).

## The review page

- **Annotate** (default): click any element or select text in the artifact, write a note, press
  Enter to queue it (⌘↵ / Ctrl+Enter queues and sends). **Explore**: use the page normally. ⌘I /
  Ctrl+I toggles. An artifact can start in Explore with `<html data-redline-annotate="off">`.
- Native controls (inputs, buttons, labels, …) and widgets with an interactive ARIA role
  (`role="button"`, `tab`, `switch`, …) keep working in Annotate. Mark other custom clickable elements
  with `data-redline-action`; use `data-redline-question` (or `queueKey`) so re-answering a question
  replaces the earlier queued answer. ⌘-click (Ctrl-click off macOS) follows a link instead of
  annotating it.
- Clicking a table cell also names its row and column (`Redis → Latency`), so feedback stays readable
  when a table is sorted or filtered; ambiguous merged cells are left unnamed.
- **Images**: attach PNG, JPEG or WebP screenshots to a note or a message by pasting, dropping or with
  the paperclip. The agent receives each image's local file path.
- Queued notes can be edited in place (Enter saves, Escape discards) or removed before you send.
- **Send to Agent** delivers every queued note plus your message, with a compact outline of the
  rendered page. **Send & End** also ends the session; the agent must not reopen it uninvited
  (`redline <file> --reopen` overrides).
- **Delivery receipts**: every sent note shows 👀 Seen when the agent's poll takes it, 🛠️ Working
  when the artifact changes after that, and ✅ Done when the agent replies. Before Seen it says why
  (no agent listening, agent busy).
- **What changed**: when the agent saves, the page reloads in place and the blocks that changed flash.
  The comparison is between the parsed pages, so reformatting alone never counts. An **N changes**
  chip steps through them, each agent reply links to what it changed, and a wholesale rewrite is
  labelled instead.
- Agent replies render a small Markdown subset: paragraphs, headings, lists, code, bold, italic, links.
- The browser tab takes the artifact's `<title>` and icon; an artifact can update them live with
  `window.redline.setDocumentMeta({ title, icon })`.
- On a desktop the conversation panel can be hidden from the top bar; while hidden, the button counts
  queued notes or marks a new reply.
- **On a phone** (under 860px) the artifact takes the screen and the conversation is a bottom sheet:
  tap or swipe the dock up; tap the dimmed page, swipe down or press Escape to lower it.

## Export

`redline export <file>` (or **Export standalone HTML** in the review page's menu) writes
`<name>.export.html`: one file with the artifact's local stylesheets, scripts, images, fonts and media
inlined, and redline's `/design/*` Tailwind + DaisyUI files too. Remote `https://` references stay
links. Nothing is fetched, reads are confined to the artifact's folder, other `file://` URLs become
`about:blank`, and the SDK is never included. Anything that could not be inlined is listed as a
warning. Caps: `REDLINE_EXPORT_MAX_ASSET_BYTES` (10 MB) and `REDLINE_EXPORT_MAX_BUNDLE_BYTES` (25 MB).

`redline <file>` and `export` also warn when an artifact never paints its own page background, since
its text could be invisible depending on where it is shown.

## CLI

| Command | |
|---|---|
| `redline` | Open sessions, guidance and playbooks |
| `redline <html-file> [--no-open] [--reopen]` | Open or resume a review session |
| `redline poll <html-file> [--owner <label>] [--takeover] [--agent-reply "…" \| --agent-reply-file <path\|->]` | Wait for the reviewer's feedback |
| `redline reply <html-file> (--agent-reply "…" \| --agent-reply-file <path\|->)` | Post a reply, exit 0 once accepted |
| `redline end <html-file>` | End the session as the agent (reopens normally later) |
| `redline export <html-file> [--out <path>]` | Write a portable copy with local assets inlined |
| `redline stop` | Stop the background server |
| `redline design` | Design guidance and the Tailwind + DaisyUI snippets (served locally at `/design/*`, or from a CDN) |
| `redline playbook [id]` | Artifact playbooks: diagram, table, comparison, plan, code, input, explanation, slides |

Output is plain text. Failures print `redline: <message>` with `hint:` lines and exit 2 for invalid
input, 1 otherwise. A CLI newer than the running server replaces it; an older one uses it as is.

## Configuration

| Variable | Default | |
|---|---|---|
| `REDLINE_STATE_DIR` | `~/.redline` | Database (`redline.db`), `attachments/` and `server.log` |
| `REDLINE_PORT` | `4389` | Server port |
| `REDLINE_HOST` | none | Also listen on this concrete address (overrides Tailscale detection; wildcards stay loopback) |
| `REDLINE_LINK_HOST` | none | Host used in session links |
| `REDLINE_ALLOWED_HOSTS` | none | Extra hostnames for the Host allowlist (whitespace-separated, `*` disables it) |
| `REDLINE_DISCONNECT_GRACE_MS` | 5 minutes | How long a waiting poll survives every review page disconnecting; `0`/`off` never releases it |
| `REDLINE_NO_OPEN` | none | `1` never opens a browser |
| `REDLINE_MAX_ATTACHMENT_BYTES` | 10 MiB | Per image |
| `REDLINE_ATTACHMENT_TTL_MS` | 7 days | Unused images expire; `0`/`off` disables |
| `REDLINE_MAX_ATTACHMENT_DISK_MB` | `512` | Image disk budget (old unused images are evicted first); `0`/`off` disables |

**Remote review.** With Tailscale running (and MagicDNS on), the server also listens on this machine's
tailnet address and session links use its MagicDNS name, so you can review from your phone. The server
has no authentication: anything that can reach it can read the artifacts it serves, so only expose it
on a network you trust.

## Development

Building needs only Go. There is no JavaScript toolchain: the browser code in `web/` is hand-written
and embedded into the binary as-is.

```sh
go build                          # the redline binary
scripts/check.sh                  # gofmt, go mod tidy, go vet, staticcheck, go test -race
go test -tags e2e ./e2e/...       # real-browser tests; needs Chrome (REDLINE_CHROME overrides the path)
REDLINE_SCREENSHOT_DIR=shots go test -tags e2e -run TestScreenshots ./e2e/   # review-page screenshots
```

## License

MIT. Portions are adapted from lavish-axi (MIT, Copyright (c) 2026 Kun Chen); see [NOTICE](NOTICE).
