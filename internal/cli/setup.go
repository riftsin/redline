package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/riftsin/redline/internal/integrations"
)

func setupCommand(e *env, args []string) (string, error) {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	project := fs.Bool("project", false, "")
	positional, err := parse(fs, args)
	if err != nil {
		return "", err
	}
	if len(positional) != 1 || (positional[0] != "hooks" && positional[0] != "skill") {
		return "", usageError("unknown setup action", "run `redline setup hooks`", "run `redline setup skill`")
	}
	home := e.getenv("HOME")
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return "", failure("cannot find your home directory: " + err.Error())
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", failure(err.Error())
	}
	scope, where := integrations.UserScope, "your home directory"
	if *project {
		scope, where = integrations.ProjectScope, cwd
	}

	if positional[0] == "skill" {
		path := integrations.SkillPath(scope, home, cwd)
		if err := integrations.InstallSkill(path); err != nil {
			return "", failure(path + ": " + err.Error())
		}
		return "Installed the redline Agent Skill at " + path + ".\n\nAgents that support Agent Skills (Claude Code: /redline) load it on demand. Re-run after upgrading redline.\n", nil
	}

	// Hooks run `redline`, so it must be found on PATH.
	if _, err := exec.LookPath("redline"); err != nil {
		return "", failure("`redline` is not on your PATH, so agent hooks could not run it",
			"install it with `go install github.com/riftsin/redline@latest` and make sure $(go env GOPATH)/bin is on PATH, then retry")
	}
	targets := integrations.TargetsFor(scope, home, cwd, e.getenv("COPILOT_HOME"))
	files, errs := integrations.Install(targets, "redline")
	if len(errs) > 0 {
		hints := make([]string, len(errs))
		for i, err := range errs {
			hints[i] = err.Error()
		}
		return "", failure("some agent hooks could not be installed", hints...)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Installed redline's SessionStart hooks in %s for Claude Code, Codex, OpenCode", where)
	if targets.CopilotHooks != "" {
		b.WriteString(" and GitHub Copilot CLI")
	}
	b.WriteString(":\n")
	for _, f := range files {
		b.WriteString("  " + f + "\n")
	}
	b.WriteString("\nRestart your agent session to receive redline's context (open review sessions, playbooks, usage). Only redline's own entries were changed.\n")
	return b.String(), nil
}

// hookCommand prints redline's context in a harness's own format. It is
// what installed hooks run; it never starts the server.
func hookCommand(e *env, args []string) (string, error) {
	if len(args) != 1 || args[0] != "copilot" {
		return "", usageError("usage: redline hook copilot")
	}
	home, _ := homeCommand(e)
	return integrations.CopilotContext(home) + "\n", nil
}
