#!/bin/sh
# Every gate a change must pass before it is pushed. CI runs the same script.
set -eu
cd "$(dirname "$0")/.."

unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
	echo "gofmt needed:" >&2
	echo "$unformatted" >&2
	exit 1
fi

go mod tidy -diff
go vet ./...
go vet -tags e2e ./...
staticcheck ./...
staticcheck -tags e2e ./...
go test -race ./...
