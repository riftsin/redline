# redline

A local review loop between you and your coding agent, over one HTML file.

Your agent writes a rich HTML artifact (a plan, a comparison, a diagram, a report) and opens it with
`redline <file.html>`. You review it in the browser: point at elements, select text, answer the
artifact's own controls, and chat. Your feedback reaches the agent, precisely anchored, through
`redline poll`. The agent edits the file, the page live-reloads, and the loop continues until either
side ends the session.

redline is a Go rewrite of [lavish-axi](https://github.com/kunchenguid/lavish-axi). It runs entirely
on your machine and makes no outbound network requests.

> Work in progress: the review loop is being built in phases.

## Install

```sh
go install github.com/riftsin/redline@latest
```

## Development

Building needs only Go. There is no JavaScript toolchain: the browser code in `web/` is hand-written
and embedded into the binary as-is.

```sh
go build                          # the redline binary
scripts/check.sh                  # gofmt, go vet, staticcheck, go test -race
go test -tags e2e ./e2e/...       # real-browser tests; needs Chrome (REDLINE_CHROME overrides the path)
```

## License

MIT. Portions are adapted from lavish-axi (MIT, Copyright (c) 2026 Kun Chen); see [NOTICE](NOTICE).
