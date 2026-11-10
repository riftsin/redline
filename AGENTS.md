# AGENTS.md

Rules that apply to almost every change in redline. Keep this file short; detail lives with the code
that owns it.

## Constraints

- `go build` is the only build step. No npm, bundler, minifier or code generator in the build.
- Go standard library only. The single approved module is `modernc.org/sqlite`. Ask before adding any
  other module, including `golang.org/x/...`.
- Browser code in `web/` is hand-written vanilla JS and CSS, embedded with `go:embed` and served as-is.
  No JavaScript dependencies. The only third-party browser files are the vendored, unmodified
  Tailwind/DaisyUI files under `web/design/`, served to artifacts that opt in.
- No outbound network requests: no telemetry, self-update or hosted sharing.

## Commands

```sh
scripts/check.sh                       # every gate CI runs: gofmt, mod tidy, vet, staticcheck, test -race
go test -tags e2e -race ./e2e/...      # real headless Chrome over the DevTools protocol
```

Run `scripts/check.sh` and the e2e tests before pushing.

## Architecture

- The server owns all review state (SQLite at `~/.redline/redline.db`, overridable with
  `REDLINE_STATE_DIR`). Only the background server opens the database; the CLI talks to it over HTTP.
- The review page is rendered on the server with `html/template`; changed regions are pushed over SSE
  and swapped in by `web/app.js`. The composer and open note card are never re-rendered.
- The SDK injected into the artifact is a sensor: it reports browser facts through one `postMessage`
  channel and never owns state. The artifact can load before `app.js` runs, so `app.js` sends
  `hello` on start and the SDK answers with `ready` again; never rely on seeing the first `ready`.
- Every state change goes through `review.Manager` (`internal/review/ops.go`), which publishes it on
  the bus; review pages and polls only react to bus events and re-read state.
- SQLite has a single connection: code that runs inside a store transaction (for example
  `SendRequest.Describe`, which resolves attached images) must never use the database, or the
  server deadlocks.
- The artifact is served byte-for-byte plus one `<script>` tag, in a sandboxed iframe, with the CSP
  `sandbox` header. A served artifact must never differ from the file on disk in any other way.

## Testing

- Bug fixes start with a failing test that reproduces the bug as a user would hit it, end-to-end when
  the bug is user-visible.
- e2e tests live in `e2e/` behind the `e2e` build tag. Real input inside the sandboxed artifact frame
  goes through `Page.ClickInFrame` / `RectInFrame`, which wait for the out-of-process frame to paint
  (Chrome drops input aimed at an unpainted frame). Wait for state with `WaitFor`, never fixed sleeps,
  except to let a CSS transition settle before clicking a moving element.
- Tests that start a server set `REDLINE_STATE_DIR` to a temp dir and use an ephemeral port.
- Tests of `setup hooks` / `setup skill` use a temp `HOME`; never run them against the real one.
- `skills/redline/SKILL.md` is generated from `integrations.Skill()`; a test fails when it drifts.
  Regenerate with `REDLINE_UPDATE_SKILL=1 go test ./internal/integrations/`.
