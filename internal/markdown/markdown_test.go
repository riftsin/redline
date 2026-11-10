package markdown

import "testing"

func TestRender(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Done.", "<p>Done.</p>"},
		{"line one\nline two\n\nnext para", "<p>line one<br>line two</p><p>next para</p>"},
		{"## Changes\n- moved **Rollout**\n- renamed `step_2`", `<p class="chat-h">Changes</p><ul><li>moved <strong>Rollout</strong></li><li>renamed <code>step_2</code></li></ul>`},
		{"1. first\n2. second\n   wrapped", "<ol><li>first</li><li>second wrapped</li></ol>"},
		{"Intro\n- a\n\nAfter", "<p>Intro</p><ul><li>a</li></ul><p>After</p>"},
		{"```\n<b>raw</b> **not bold**\n```", "<pre><code>&lt;b&gt;raw&lt;/b&gt; **not bold**</code></pre>"},
		{"<script>alert(1)</script>", "<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>"},
		{"see [docs](https://example.com/a?b=1&c=2)", `<p>see <a href="https://example.com/a?b=1&amp;c=2" target="_blank" rel="noopener noreferrer">docs</a></p>`},
		{"see https://example.com/x.", `<p>see <a href="https://example.com/x" target="_blank" rel="noopener noreferrer">https://example.com/x</a>.</p>`},
		{"[bad](javascript:alert(1))", "<p>[bad](javascript:alert(1))</p>"},
		{"*italic* and _also_ but snake_case_name stays", "<p><em>italic</em> and <em>also</em> but snake_case_name stays</p>"},
		{"`a*b*c` and *x*", "<p><code>a*b*c</code> and <em>x</em></p>"},
		{"| a | b |\n|---|---|", "<p>| a | b |<br>|---|---|</p>"},
	}
	for _, c := range cases {
		if got := string(Render(c.in)); got != c.want {
			t.Errorf("Render(%q)\n got %s\nwant %s", c.in, got, c.want)
		}
	}
}
