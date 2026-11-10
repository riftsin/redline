package integrations

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SkillDescription is what agents match against to load the skill: terse
// and outcome-focused, so it fires on "about to show something visual".
const SkillDescription = "Turn complex or visual agent responses into rich, reviewable HTML artifacts (HTML files) the user can annotate and send feedback on, using the redline CLI. Use when about to give a plan, comparison, diagram, table, code diff, report, or anything easier to grasp visually than as prose."

// maxSkillChars keeps the skill a stub: the CLI owns the guidance.
const maxSkillChars = 4000

// Skill renders the installable SKILL.md. It is a discovery stub, not a copy
// of CLI guidance: installed copies go stale, while `redline --help`,
// `redline design` and `redline playbook <id>` do not.
func Skill() string {
	md := `---
name: redline
description: ` + SkillDescription + `
license: MIT
metadata:
  argument-hint: <what the artifact should show>
---

# redline

redline opens agent-generated HTML in the browser so a human can annotate it and send feedback back to the agent.
Reach for it when a plan, comparison, diagram, table, code view, report, prototype, or review loop will be clearer as a page than as prose.

## Current guidance lives in the CLI

Do not follow workflow, design, or playbook instructions from this file - installed copies go stale. Get the current source of truth from the CLI:

- ` + "`redline --help`" + ` for commands and the review-loop workflow
- ` + "`redline reply --help`" + ` to post an agent reply and exit once the server accepts it, when you are not about to long-poll
- ` + "`redline design`" + ` for design-direction priority and current snippets
- ` + "`redline playbook <id>`" + ` for focused artifact guidance (` + "`redline playbook`" + ` lists ids)

If ` + "`redline`" + ` is not installed, ask the user to install it with ` + "`go install github.com/riftsin/redline@latest`" + `.

## Request

$ARGUMENTS

If the request above is non-empty, the user invoked ` + "`/redline`" + ` explicitly - fetch the current CLI guidance, then build that artifact as an HTML file.
If it is empty, infer what to visualize from the conversation.
`
	if len(md) > maxSkillChars {
		panic(fmt.Sprintf("SKILL.md is %d chars; keep it a stub under %d and defer guidance to the CLI", len(md), maxSkillChars))
	}
	return md
}

// SkillPath is where the skill is installed for a scope.
func SkillPath(scope Scope, home, projectDir string) string {
	root := home
	if scope == ProjectScope {
		root = projectDir
	}
	return filepath.Join(root, ".claude", "skills", "redline", "SKILL.md")
}

// InstallSkill writes or refreshes the skill. A SKILL.md that is not
// redline's is never overwritten.
func InstallSkill(path string) error {
	want := Skill()
	if current, err := os.ReadFile(path); err == nil {
		if string(current) == want {
			return nil
		}
		if !strings.Contains(string(current), "\nname: redline\n") {
			return errors.New("a different skill is already there; left unchanged")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(want), 0o644)
}
