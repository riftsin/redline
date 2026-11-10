// Command redline is a local review loop between a person and a coding agent
// over one HTML file.
package main

import (
	"os"

	"github.com/riftsin/redline/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
