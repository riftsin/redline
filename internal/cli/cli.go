// Package cli implements the redline command line.
package cli

import (
	"fmt"
	"io"
	"runtime/debug"
)

// Version reports the module version redline was built from. A build from a
// local checkout reports "(devel)".
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "(devel)"
	}
	return info.Main.Version
}

// Run executes the command named by args and returns the process exit status.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 {
		switch args[0] {
		case "--version", "-v", "-V":
			fmt.Fprintln(stdout, Version())
			return 0
		}
	}
	fmt.Fprintln(stderr, "redline: not implemented yet")
	return 1
}
