// Package markdown renders the small Markdown subset agents may use in
// replies: paragraphs, line breaks, headings (as bold lead lines), bullet
// and numbered lists, fenced and inline code, bold, italic and http(s)
// links. Everything else - tables, images, raw HTML - stays as the text the
// agent wrote. All text is escaped before any markup is added, so a reply
// can never inject HTML into the review page.
package markdown

import (
	"html"
	"html/template"
	"regexp"
	"strings"
)

// Render converts text to safe HTML.
func Render(text string) template.HTML {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var b strings.Builder
	var para []string
	var list []string
	listTag := ""

	flushPara := func() {
		if len(para) == 0 {
			return
		}
		parts := make([]string, len(para))
		for i, l := range para {
			parts[i] = inline(l)
		}
		b.WriteString("<p>" + strings.Join(parts, "<br>") + "</p>")
		para = nil
	}
	flushList := func() {
		if len(list) == 0 {
			return
		}
		b.WriteString("<" + listTag + ">")
		for _, item := range list {
			b.WriteString("<li>" + inline(item) + "</li>")
		}
		b.WriteString("</" + listTag + ">")
		list, listTag = nil, ""
	}
	flush := func() { flushPara(); flushList() }

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if fence, ok := fenceStart(trimmed); ok {
			flush()
			var code []string
			for i++; i < len(lines); i++ {
				if strings.HasPrefix(strings.TrimSpace(lines[i]), fence) {
					break
				}
				code = append(code, lines[i])
			}
			b.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>")
			continue
		}
		if trimmed == "" {
			flush()
			continue
		}
		if m := heading.FindStringSubmatch(trimmed); m != nil {
			flush()
			b.WriteString(`<p class="chat-h">` + inline(m[1]) + "</p>")
			continue
		}
		if m := bullet.FindStringSubmatch(line); m != nil {
			flushPara()
			if listTag != "ul" {
				flushList()
				listTag = "ul"
			}
			list = append(list, m[1])
			continue
		}
		if m := numbered.FindStringSubmatch(line); m != nil {
			flushPara()
			if listTag != "ol" {
				flushList()
				listTag = "ol"
			}
			list = append(list, m[1])
			continue
		}
		if len(list) > 0 && (strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")) {
			// A continuation line of the last list item.
			list[len(list)-1] += " " + trimmed
			continue
		}
		flushList()
		para = append(para, trimmed)
	}
	flush()
	return template.HTML(b.String())
}

var (
	heading  = regexp.MustCompile(`^#{1,6}\s+(.+?)\s*#*$`)
	bullet   = regexp.MustCompile(`^\s{0,3}[-*+]\s+(.+)$`)
	numbered = regexp.MustCompile(`^\s{0,3}\d{1,9}[.)]\s+(.+)$`)
)

func fenceStart(line string) (string, bool) {
	for _, f := range []string{"```", "~~~"} {
		if strings.HasPrefix(line, f) {
			return f, true
		}
	}
	return "", false
}

var (
	codeSpan = regexp.MustCompile("`([^`]+)`")
	link     = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^\s)]+)\)`)
	bare     = regexp.MustCompile(`(^|[\s(])(https?://[^\s<]+[^\s<.,;:!?)\]'"])`)
	bold     = regexp.MustCompile(`\*\*([^*]+)\*\*|__([^_]+)__`)
	italic   = regexp.MustCompile(`(^|[^*\w])\*([^*\s][^*]*)\*|(^|[^_\w])_([^_\s][^_]*)_`)
)

// inline renders code spans, links, bold and italic on one escaped line.
// Code spans are cut out first so nothing inside them is formatted.
func inline(s string) string {
	var spans []string
	s = codeSpan.ReplaceAllStringFunc(s, func(m string) string {
		spans = append(spans, "<code>"+html.EscapeString(m[1:len(m)-1])+"</code>")
		return "\x00" + string(rune('0'+len(spans)-1)) + "\x00"
	})
	s = html.EscapeString(s)

	var links []string
	s = link.ReplaceAllStringFunc(s, func(m string) string {
		parts := link.FindStringSubmatch(m)
		links = append(links, `<a href="`+parts[2]+`" target="_blank" rel="noopener noreferrer">`+parts[1]+"</a>")
		return "\x01" + string(rune('0'+len(links)-1)) + "\x01"
	})
	s = bare.ReplaceAllStringFunc(s, func(m string) string {
		parts := bare.FindStringSubmatch(m)
		links = append(links, `<a href="`+parts[2]+`" target="_blank" rel="noopener noreferrer">`+parts[2]+"</a>")
		return parts[1] + "\x01" + string(rune('0'+len(links)-1)) + "\x01"
	})

	s = bold.ReplaceAllStringFunc(s, func(m string) string {
		p := bold.FindStringSubmatch(m)
		return "<strong>" + p[1] + p[2] + "</strong>"
	})
	s = italic.ReplaceAllStringFunc(s, func(m string) string {
		p := italic.FindStringSubmatch(m)
		if p[2] != "" {
			return p[1] + "<em>" + p[2] + "</em>"
		}
		return p[3] + "<em>" + p[4] + "</em>"
	})

	for i, l := range links {
		s = strings.Replace(s, "\x01"+string(rune('0'+i))+"\x01", l, 1)
	}
	for i, c := range spans {
		s = strings.Replace(s, "\x00"+string(rune('0'+i))+"\x00", c, 1)
	}
	return s
}
