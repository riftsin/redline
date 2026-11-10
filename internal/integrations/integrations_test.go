package integrations

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sessionStartCommands(t *testing.T, path string) []string {
	t.Helper()
	var out []string
	groups, _ := readJSON(t, path)["hooks"].(map[string]any)["SessionStart"].([]any)
	for _, g := range groups {
		for _, h := range g.(map[string]any)["hooks"].([]any) {
			out = append(out, h.(map[string]any)["command"].(string))
		}
	}
	return out
}

func TestInstallHooksPreservesOthersAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	targets := TargetsFor(UserScope, home, "", "")
	write(t, targets.ClaudeSettings, `{
  "model": "opus",
  "hooks": {
    "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "other-tool context", "timeout": 5}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "notify"}]}]
  }
}`)
	write(t, targets.CodexConfig, "model = \"o4\"\n\n[features]\nweb_search = true\n\n[profiles.fast]\nmodel = \"mini\"\n")

	files, errs := Install(targets, "redline")
	if len(errs) > 0 || len(files) != 5 {
		t.Fatalf("install: %v %v", files, errs)
	}
	claude := readJSON(t, targets.ClaudeSettings)
	if claude["model"] != "opus" || claude["hooks"].(map[string]any)["Stop"] == nil {
		t.Fatalf("other settings lost: %v", claude)
	}
	if got := strings.Join(sessionStartCommands(t, targets.ClaudeSettings), "|"); got != "other-tool context|redline" {
		t.Fatalf("Claude SessionStart = %s", got)
	}
	if got := strings.Join(sessionStartCommands(t, targets.CodexHooks), "|"); got != "redline" {
		t.Fatalf("Codex SessionStart = %s", got)
	}
	codex, _ := os.ReadFile(targets.CodexConfig)
	if string(codex) != "model = \"o4\"\n\n[features]\nweb_search = true\n\nhooks = true\n[profiles.fast]\nmodel = \"mini\"\n" {
		t.Fatalf("codex config:\n%s", codex)
	}
	copilot := readJSON(t, targets.CopilotHooks)
	hook := copilot["hooks"].(map[string]any)["sessionStart"].([]any)[0].(map[string]any)
	if copilot["version"] != float64(1) || hook["bash"] != "redline hook copilot" {
		t.Fatalf("copilot = %v", copilot)
	}
	plugin, _ := os.ReadFile(targets.OpenCodePlugin)
	if !strings.HasPrefix(string(plugin), managedPrefix) || !strings.Contains(string(plugin), `const command = "redline";`) {
		t.Fatalf("opencode plugin:\n%.200s", plugin)
	}

	// A second run changes nothing.
	before := snapshot(t, targets)
	if _, errs := Install(targets, "redline"); len(errs) > 0 {
		t.Fatal(errs)
	}
	if after := snapshot(t, targets); after != before {
		t.Fatal("a second install changed files")
	}
}

func snapshot(t *testing.T, targets Targets) string {
	t.Helper()
	var b strings.Builder
	for _, p := range []string{targets.ClaudeSettings, targets.CodexHooks, targets.CodexConfig, targets.OpenCodePlugin, targets.CopilotHooks} {
		data, _ := os.ReadFile(p)
		b.Write(data)
	}
	return b.String()
}

func TestInstallRepairsStaleAndDuplicateEntries(t *testing.T) {
	home := t.TempDir()
	targets := TargetsFor(UserScope, home, "", "")
	write(t, targets.ClaudeSettings, `{"hooks": {"SessionStart": [
		{"matcher": "", "hooks": [{"type": "command", "command": "/old/path/redline", "timeout": 3}]},
		{"matcher": "", "hooks": [{"type": "command", "command": "redline"}]},
		{"matcher": "", "hooks": [{"type": "command", "command": "redliner --x"}]}
	]}}`)
	if _, errs := Install(targets, "redline"); len(errs) > 0 {
		t.Fatal(errs)
	}
	if got := strings.Join(sessionStartCommands(t, targets.ClaudeSettings), "|"); got != "redline|redliner --x" {
		t.Fatalf("SessionStart = %s", got)
	}
}

func TestInstallRefusesFilesItDoesNotOwn(t *testing.T) {
	home := t.TempDir()
	targets := TargetsFor(UserScope, home, "", "")
	write(t, targets.ClaudeSettings, `{ not json`)
	write(t, targets.OpenCodePlugin, "export default {} // someone else's plugin\n")
	_, errs := Install(targets, "redline")
	if len(errs) != 2 {
		t.Fatalf("errors = %v", errs)
	}
	if data, _ := os.ReadFile(targets.ClaudeSettings); string(data) != `{ not json` {
		t.Fatal("an unparsable settings file was rewritten")
	}
	if data, _ := os.ReadFile(targets.OpenCodePlugin); !strings.Contains(string(data), "someone else's") {
		t.Fatal("an unmanaged OpenCode plugin was overwritten")
	}
}

func TestProjectScope(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	targets := TargetsFor(ProjectScope, home, project, "")
	if !strings.HasPrefix(targets.ClaudeSettings, project) || !strings.HasPrefix(targets.OpenCodePlugin, project) || targets.CopilotHooks != "" {
		t.Fatalf("targets = %+v", targets)
	}
	if !strings.HasPrefix(targets.CodexConfig, home) {
		t.Fatal("Codex's feature flag lives in the user's config")
	}
}

func TestEnableCodexHooks(t *testing.T) {
	for in, want := range map[string]string{
		"":                                      "[features]\nhooks = true\n",
		"[features]\nhooks = false\n":           "[features]\nhooks = true\n",
		"[features]\nhooks = true # on\n":       "[features]\nhooks = true # on\n",
		"model = \"x\"":                         "model = \"x\"\n\n[features]\nhooks = true\n",
		"[features]\nother = 1":                 "[features]\nother = 1\nhooks = true\n",
		"a = 1\r\n[features]\r\nb = 2\r\n":      "a = 1\r\n[features]\r\nb = 2\r\nhooks = true\r\n",
		"[[tools]]\nname = \"x\"\n[features]\n": "[[tools]]\nname = \"x\"\n[features]\nhooks = true\n",
	} {
		got, _ := enableCodexHooks(in)
		if got != want {
			t.Errorf("enableCodexHooks(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestSkill(t *testing.T) {
	path := SkillPath(UserScope, t.TempDir(), "")
	if err := InstallSkill(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != Skill() || !strings.Contains(Skill(), "redline --help") {
		t.Fatal("skill not installed as generated")
	}
	other := SkillPath(UserScope, t.TempDir(), "")
	write(t, other, "---\nname: something-else\n---\n")
	if err := InstallSkill(other); err == nil {
		t.Fatal("a different skill was overwritten")
	}
}

// The committed skills/redline/SKILL.md must match the generator. Run with
// REDLINE_UPDATE_SKILL=1 to regenerate it.
func TestCommittedSkillIsCurrent(t *testing.T) {
	path := filepath.Join("..", "..", "skills", "redline", "SKILL.md")
	if os.Getenv("REDLINE_UPDATE_SKILL") == "1" {
		write(t, path, Skill())
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != Skill() {
		t.Fatalf("skills/redline/SKILL.md is stale; run REDLINE_UPDATE_SKILL=1 go test ./internal/integrations/ (%v)", err)
	}
}

func TestCopilotContext(t *testing.T) {
	var v map[string]string
	if err := json.Unmarshal([]byte(CopilotContext("OPEN SESSIONS\n  none\n")), &v); err != nil || v["additionalContext"] != "## redline context\nOPEN SESSIONS\n  none" {
		t.Fatalf("context = %v %v", v, err)
	}
	if CopilotContext("  ") != "{}" {
		t.Fatal("empty context")
	}
}

func TestHerdrChimeEnabled(t *testing.T) {
	env := map[string]string{"HERDR_ENV": "1"}
	get := func(k string) string { return env[k] }
	if HerdrChimeEnabled(get) {
		t.Fatal("the chime is opt-in")
	}
	env["REDLINE_HERDR_CHIME"] = "1"
	if !HerdrChimeEnabled(get) {
		t.Fatal("opted in inside Herdr")
	}
	delete(env, "HERDR_ENV")
	if HerdrChimeEnabled(get) {
		t.Fatal("outside Herdr there is nothing to notify")
	}
}
