// Package cli implements the redline command line, the agent's interface to
// redline. Output is plain text: stdout carries only the final response,
// progress notes for humans go to stderr. A failure prints "redline: <message>"
// plus hint lines and exits 2 for invalid input, 1 otherwise.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/riftsin/redline/internal/guidance"
	"github.com/riftsin/redline/internal/paths"
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

// Error is a command failure shown to the agent.
type Error struct {
	Message string
	Hints   []string
	// Usage marks invalid input (exit 2) rather than a failure (exit 1).
	Usage bool
}

func (e *Error) Error() string { return e.Message }

func usageError(message string, hints ...string) *Error {
	return &Error{Message: message, Hints: hints, Usage: true}
}

func failure(message string, hints ...string) *Error {
	return &Error{Message: message, Hints: hints}
}

// env is how a command sees the world; tests substitute parts of it.
type env struct {
	stdout, stderr io.Writer
	stdin          io.Reader
	getenv         func(string) string
	agent          guidance.Agent
	// openBrowser opens a URL; it reports whether it did.
	openBrowser func(url string) bool
	// startServer launches the background server process.
	startServer func(port int) error
}

var commands = map[string]func(*env, []string) (string, error){
	"open":     openCommand,
	"poll":     pollCommand,
	"reply":    replyCommand,
	"end":      endCommand,
	"stop":     stopCommand,
	"design":   designCommand,
	"export":   exportCommand,
	"setup":    setupCommand,
	"hook":     hookCommand,
	"playbook": playbookCommand,
	"server":   serverCommand,
}

// Run executes the command named by args and returns the exit status.
func Run(args []string, stdout, stderr io.Writer) int {
	e := &env{
		stdout:      stdout,
		stderr:      stderr,
		stdin:       os.Stdin,
		getenv:      os.Getenv,
		openBrowser: systemOpen,
		startServer: spawnServer,
	}
	e.agent = guidance.InvokingAgent(e.getenv)
	return e.run(args)
}

func (e *env) run(args []string) int {
	if len(args) == 1 {
		switch args[0] {
		case "--version", "-v", "-V":
			fmt.Fprintln(e.stdout, Version())
			return 0
		case "--help", "-h", "help":
			fmt.Fprint(e.stdout, guidance.Help(e.agent))
			return 0
		}
	}

	var out string
	var err error
	if len(args) == 0 {
		out, err = homeCommand(e)
	} else {
		name, rest, resolveErr := resolveCommand(args)
		switch {
		case resolveErr != nil:
			err = resolveErr
		case hasHelpFlag(rest) && guidance.CommandHelp(name, e.agent) != "":
			out = guidance.CommandHelp(name, e.agent)
		default:
			out, err = commands[name](e, rest)
		}
	}

	if err != nil {
		var ce *Error
		if !errors.As(err, &ce) {
			ce = failure(err.Error())
		}
		fmt.Fprintln(e.stdout, "redline: "+ce.Message)
		for _, h := range ce.Hints {
			fmt.Fprintln(e.stdout, "hint: "+h)
		}
		if ce.Usage {
			return 2
		}
		return 1
	}
	fmt.Fprint(e.stdout, out)
	if out != "" && !strings.HasSuffix(out, "\n") {
		fmt.Fprintln(e.stdout)
	}
	return 0
}

// resolveCommand maps the arguments to a command: a known command name, or
// an HTML file path, which means "open".
func resolveCommand(args []string) (string, []string, error) {
	first := args[0]
	if _, ok := commands[first]; ok && first != "open" {
		return first, args[1:], nil
	}
	if first == "open" {
		return "open", args[1:], nil
	}
	if strings.HasPrefix(first, "-") {
		for _, a := range args {
			if paths.IsHTMLPath(a) {
				return "open", args, nil
			}
		}
		return "", nil, usageError("flags must come after the command",
			"run `redline <command> [args] [flags]`", "move `"+first+"` after the command")
	}
	if paths.IsHTMLPath(first) {
		return "open", args, nil
	}
	if _, err := os.Stat(first); err == nil {
		return "open", args, nil
	}
	return "", nil, usageError("unknown command: "+first, "run `redline --help` to see the commands")
}

func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

// parse parses flags anywhere among args and returns the positionals.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageError(strings.TrimPrefix(err.Error(), "flag provided but not defined: ")+": unknown or malformed flag",
				"run the command with --help")
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// fileArg parses flags and requires exactly one HTML file argument, which it
// returns in canonical form.
func fileArg(fs *flag.FlagSet, args []string, usage string) (string, error) {
	positional, err := parse(fs, args)
	if err != nil {
		return "", err
	}
	switch {
	case len(positional) == 0:
		return "", usageError("an HTML file path is required", "run `"+usage+"`")
	case len(positional) > 1:
		return "", usageError("unexpected arguments: " + strings.Join(positional[1:], " "))
	}
	file, err := paths.CanonicalHTML(positional[0])
	if err != nil {
		return "", usageError(err.Error(), "write the artifact first, then run `redline <html-file>`")
	}
	return file, nil
}

func noSession(file string) *Error {
	return failure("no active review session for "+file, "run `redline "+file+"` first")
}
