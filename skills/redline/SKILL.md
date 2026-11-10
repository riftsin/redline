---
name: redline
description: Turn complex or visual agent responses into rich, reviewable HTML artifacts (HTML files) the user can annotate and send feedback on, using the redline CLI. Use when about to give a plan, comparison, diagram, table, code diff, report, or anything easier to grasp visually than as prose.
license: MIT
metadata:
  argument-hint: <what the artifact should show>
---

# redline

redline opens agent-generated HTML in the browser so a human can annotate it and send feedback back to the agent.
Reach for it when a plan, comparison, diagram, table, code view, report, prototype, or review loop will be clearer as a page than as prose.

## Current guidance lives in the CLI

Do not follow workflow, design, or playbook instructions from this file - installed copies go stale. Get the current source of truth from the CLI:

- `redline --help` for commands and the review-loop workflow
- `redline reply --help` to post an agent reply and exit once the server accepts it, when you are not about to long-poll
- `redline design` for design-direction priority and current snippets
- `redline playbook <id>` for focused artifact guidance (`redline playbook` lists ids)

If `redline` is not installed, ask the user to install it with `go install github.com/riftsin/redline@latest`.

## Request

$ARGUMENTS

If the request above is non-empty, the user invoked `/redline` explicitly - fetch the current CLI guidance, then build that artifact as an HTML file.
If it is empty, infer what to visualize from the conversation.
