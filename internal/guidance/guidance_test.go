package guidance

import (
	"strings"
	"testing"
)

func TestPlaybooks(t *testing.T) {
	ids := strings.Join(PlaybookIDs(), ",")
	if ids != "diagram,table,comparison,plan,code,input,explanation,slides" {
		t.Fatalf("playbook ids = %s", ids)
	}
	p, ok := FindPlaybook("input")
	if !ok {
		t.Fatal("input playbook missing")
	}
	text := p.Text()
	for _, want := range []string{"Playbook: input", "Use when: ", "Design rules:", `data-redline-annotate="off"`, "window.redline.queuePrompt"} {
		if !strings.Contains(text, want) {
			t.Errorf("input playbook lacks %q", want)
		}
	}
	for _, p := range Playbooks() {
		if lower := strings.ToLower(p.Text()); strings.Contains(lower, "athanor") || strings.Contains(lower, "lavish") {
			t.Errorf("playbook %s mentions another tool", p.ID)
		}
	}
}

func TestDesignRendersReference(t *testing.T) {
	d := Design()
	for _, want := range []string{"PLAYBOOK ROUTER", "daisyui@5.5.19", "Themes:", "coffee", "btn-primary", "LAYOUT SAFETY"} {
		if !strings.Contains(d, want) {
			t.Errorf("design output lacks %q", want)
		}
	}
}

func TestCodexGuidance(t *testing.T) {
	env := map[string]string{"CODEX_THREAD_ID": "x"}
	agent := InvokingAgent(func(k string) string { return env[k] })
	if agent != Codex || !strings.Contains(PollExecution(agent), "Codex detected") {
		t.Fatal("Codex guidance missing")
	}
	if strings.Contains(PollExecution(Generic), "Codex") {
		t.Fatal("generic guidance mentions Codex")
	}
}

func TestEveryCommandHasHelp(t *testing.T) {
	for _, c := range []string{"open", "poll", "reply", "end", "stop", "playbook", "design"} {
		if CommandHelp(c, Generic) == "" {
			t.Errorf("no help for %s", c)
		}
	}
}
