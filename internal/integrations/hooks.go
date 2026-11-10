// Package integrations connects redline to agent harnesses: SessionStart
// hooks that give every new agent session redline's context (open review
// sessions, playbooks, usage), the installable Agent Skill, and the opt-in
// Herdr readiness chime.
//
// Every change is explicit (run by `redline setup ...`), idempotent, and
// touches only redline's own entries: other hooks, settings and files are
// preserved, and a file redline did not write is never overwritten.
//
// Hook formats follow lavish-axi's (MIT, Copyright (c) 2026 Kun Chen).
package integrations

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Marker identifies redline's own hook entries: a managed hook's command
// contains it.
const Marker = "redline"

// HookTimeoutSeconds bounds how long a harness waits for redline's context.
const HookTimeoutSeconds = 10

// Scope selects user-wide or project-local installation.
type Scope int

const (
	UserScope Scope = iota
	ProjectScope
)

// Targets are the files hook installation writes.
type Targets struct {
	ClaudeSettings string // .claude/settings.json
	CodexHooks     string // .codex/hooks.json
	CodexConfig    string // ~/.codex/config.toml (always user-wide)
	OpenCodePlugin string // .../opencode/plugins/redline.js
	CopilotHooks   string // ~/.copilot/hooks/redline.json (user scope only)
}

// TargetsFor resolves the files for a scope.
func TargetsFor(scope Scope, home, projectDir, copilotHome string) Targets {
	root := home
	if scope == ProjectScope {
		root = projectDir
	}
	t := Targets{
		ClaudeSettings: filepath.Join(root, ".claude", "settings.json"),
		CodexHooks:     filepath.Join(root, ".codex", "hooks.json"),
		CodexConfig:    filepath.Join(home, ".codex", "config.toml"),
		OpenCodePlugin: filepath.Join(home, ".config", "opencode", "plugins", "redline.js"),
	}
	if scope == ProjectScope {
		t.OpenCodePlugin = filepath.Join(projectDir, ".opencode", "plugins", "redline.js")
	} else {
		if copilotHome == "" {
			copilotHome = filepath.Join(home, ".copilot")
		}
		t.CopilotHooks = filepath.Join(copilotHome, "hooks", "redline.json")
	}
	return t
}

// Install writes or repairs every hook. command is how harnesses run
// redline (normally "redline", found on PATH). It returns the files written
// or already current, and one error per file it could not update.
func Install(t Targets, command string) ([]string, []error) {
	var files []string
	var errs []error
	record := func(path string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			return
		}
		files = append(files, path)
	}

	for _, path := range []string{t.ClaudeSettings, t.CodexHooks} {
		record(path, updateJSON(path, func(settings map[string]any) bool {
			return addSessionStartHook(settings, command)
		}))
	}
	record(t.CodexConfig, updateText(t.CodexConfig, enableCodexHooks))
	record(t.OpenCodePlugin, writeManaged(t.OpenCodePlugin, openCodePlugin(command)))
	if t.CopilotHooks != "" {
		record(t.CopilotHooks, updateJSON(t.CopilotHooks, func(settings map[string]any) bool {
			return addCopilotHook(settings, command)
		}))
	}
	return files, errs
}

// updateJSON applies change to a JSON object file, creating it when
// missing. A file that is not a JSON object is refused rather than
// rewritten.
func updateJSON(path string, change func(map[string]any) bool) error {
	settings := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil && len(strings.TrimSpace(string(data))) > 0:
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("not valid JSON, left unchanged (%v)", err)
		}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	if !change(settings) {
		return nil
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

func updateText(path string, change func(string) (string, bool)) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	updated, changed := change(string(data))
	if !changed {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(updated), 0o644)
}

func isManaged(hook any) bool {
	h, ok := hook.(map[string]any)
	if !ok {
		return false
	}
	for _, field := range []string{"command", "bash"} {
		if c, ok := h[field].(string); ok && containsCommand(c) {
			return true
		}
	}
	return false
}

var markerWord = regexp.MustCompile(`(^|[\s/\\"'])redline(\.exe)?($|[\s"'])`)

// containsCommand reports whether a hook command runs redline (and not, say,
// a different tool whose name merely contains it).
func containsCommand(command string) bool {
	return markerWord.MatchString(command)
}

// addSessionStartHook installs redline's entry in a Claude Code or Codex
// settings object, repairing a stale one and dropping duplicates.
func addSessionStartHook(settings map[string]any, command string) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	changed := false
	if hooks == nil {
		hooks = map[string]any{}
		settings["hooks"] = hooks
		changed = true
	}
	groups, _ := hooks["SessionStart"].([]any)
	want := map[string]any{"type": "command", "command": command, "timeout": float64(HookTimeoutSeconds)}
	found := false
	var kept []any
	for _, g := range groups {
		group, ok := g.(map[string]any)
		inner, isList := group["hooks"].([]any)
		if !ok || !isList {
			kept = append(kept, g)
			continue
		}
		var keptHooks []any
		for _, h := range inner {
			if !isManaged(h) {
				keptHooks = append(keptHooks, h)
				continue
			}
			if found {
				changed = true // a duplicate
				continue
			}
			found = true
			if !sameHook(h.(map[string]any), want) {
				h = want
				changed = true
			}
			keptHooks = append(keptHooks, h)
		}
		if len(keptHooks) == 0 && len(inner) > 0 {
			changed = true
			continue
		}
		group["hooks"] = keptHooks
		kept = append(kept, group)
	}
	if !found {
		kept = append(kept, map[string]any{"matcher": "", "hooks": []any{want}})
		changed = true
	}
	if _, ok := hooks["SessionStart"].([]any); !ok {
		changed = true
	}
	hooks["SessionStart"] = kept
	return changed
}

func sameHook(a, b map[string]any) bool {
	return a["type"] == b["type"] && a["command"] == b["command"] && a["timeout"] == b["timeout"]
}

// addCopilotHook installs redline's sessionStart entry for GitHub Copilot
// CLI, which takes JSON with an additionalContext field.
func addCopilotHook(settings map[string]any, command string) bool {
	changed := false
	if v, _ := settings["version"].(float64); v != 1 {
		settings["version"] = float64(1)
		changed = true
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		settings["hooks"] = hooks
		changed = true
	}
	current, _ := hooks["sessionStart"].([]any)
	run := command + " hook copilot"
	want := map[string]any{"type": "command", "bash": run, "powershell": run, "timeoutSec": float64(HookTimeoutSeconds)}
	var next []any
	for _, h := range current {
		if !isManaged(h) {
			next = append(next, h)
		}
	}
	next = append(next, want)
	before, _ := json.Marshal(current)
	after, _ := json.Marshal(next)
	if string(before) != string(after) {
		hooks["sessionStart"] = next
		changed = true
	}
	return changed
}

// enableCodexHooks turns on Codex's hooks feature in config.toml, keeping
// everything else in the file as written.
func enableCodexHooks(content string) (string, bool) {
	newline := "\n"
	if strings.Contains(content, "\r\n") {
		newline = "\r\n"
	}
	if strings.TrimSpace(content) == "" {
		return "[features]" + newline + "hooks = true" + newline, true
	}
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	section := regexp.MustCompile(`^\s*\[\[?([^\]]+)\]\]?\s*(#.*)?$`)
	flag := regexp.MustCompile(`^\s*hooks\s*=\s*(true|false)\s*(#.*)?$`)
	inFeatures, sawFeatures := false, false
	for i, line := range lines {
		if m := section.FindStringSubmatch(line); m != nil {
			if inFeatures {
				lines = append(lines[:i], append([]string{"hooks = true"}, lines[i:]...)...)
				return strings.Join(lines, newline), true
			}
			inFeatures = strings.TrimSpace(m[1]) == "features"
			sawFeatures = sawFeatures || inFeatures
			continue
		}
		if !inFeatures {
			continue
		}
		if m := flag.FindStringSubmatch(line); m != nil {
			if m[1] == "true" {
				return content, false
			}
			lines[i] = strings.Replace(line, "false", "true", 1)
			return strings.Join(lines, newline), true
		}
	}
	joined := strings.Join(lines, newline)
	if sawFeatures {
		if !strings.HasSuffix(joined, newline) {
			joined += newline
		}
		return joined + "hooks = true" + newline, true
	}
	sep := newline + newline
	if strings.HasSuffix(joined, newline) {
		sep = newline
	}
	return joined + sep + "[features]" + newline + "hooks = true" + newline, true
}

const managedPrefix = "// redline managed file:"

// writeManaged writes a generated file, refusing to replace one redline did
// not generate.
func writeManaged(path, content string) error {
	if current, err := os.ReadFile(path); err == nil {
		if !strings.HasPrefix(string(current), managedPrefix) {
			return errors.New("an unmanaged file is already there; left unchanged")
		}
		if string(current) == content {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// openCodePlugin is an OpenCode plugin that adds redline's context to each
// session's system prompt, running `redline` once per session. OpenCode 2
// runs setup(ctx); OpenCode 1 runs server(input).
func openCodePlugin(command string) string {
	cmd, _ := json.Marshal(command)
	return managedPrefix + ` redline SessionStart context for OpenCode.
// Generated by ` + "`redline setup hooks`" + `; remove the line above before editing it by hand.
import { spawn } from "node:child_process";

const command = ` + string(cmd) + `;
const header = "## redline context";
const timeoutMs = ` + fmt.Sprint(HookTimeoutSeconds*1000) + `;

function homeView(cwd) {
  return new Promise((resolve) => {
    const child = spawn(command, [], { cwd: cwd || process.cwd(), env: process.env, shell: false, stdio: ["ignore", "pipe", "pipe"] });
    let out = "";
    let err = "";
    let done = false;
    const finish = (text) => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      resolve(text);
    };
    const timer = setTimeout(() => {
      child.kill("SIGTERM");
      finish("error: redline context timed out after " + timeoutMs + "ms");
    }, timeoutMs);
    child.stdout.setEncoding("utf-8");
    child.stderr.setEncoding("utf-8");
    child.stdout.on("data", (c) => (out += c));
    child.stderr.on("data", (c) => (err += c));
    child.on("error", (e) => finish("error: redline context failed: " + e.message));
    child.on("close", (code) => finish(code === 0 ? out.trim() : "error: redline context failed: " + (err || out || "exit " + code).trim()));
  });
}

function contextFor(directory) {
  const cache = new Map();
  return async (sessionID) => {
    const key = sessionID ?? "__global__";
    if (!cache.has(key)) cache.set(key, await homeView(directory));
    const text = cache.get(key);
    return text ? header + "\n" + text : "";
  };
}

export default {
  id: "redline",
  async setup(ctx) {
    const context = contextFor(ctx.location.directory);
    await ctx.session.hook("context", async (event) => {
      const text = await context(event.sessionID);
      if (text) event.system.push({ type: "text", text });
    });
  },
  async server({ directory }) {
    const context = contextFor(directory);
    return {
      "experimental.chat.system.transform": async (input, output) => {
        const text = await context(input.sessionID);
        if (text) output.system.push(text);
      },
    };
  },
};
`
}

// CopilotContext wraps the home view the way GitHub Copilot CLI's
// sessionStart hook expects it.
func CopilotContext(home string) string {
	text := strings.TrimSpace(home)
	if text == "" {
		return "{}"
	}
	b, _ := json.Marshal(map[string]string{"additionalContext": "## redline context\n" + text})
	return string(b)
}
