package helpers

import (
	"html"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Plain text as people type it, shown as HTML: paragraphs split by a blank
// line, a single line break kept as one, and lines starting "- ", "* " or
// "• " made a list. Everything is escaped, so nothing in the text can become
// markup. One set of rules wherever typed or imported text lands in a rich
// surface (a project update, an email read in the inbox, an imported comment);
// the app renders updates the same way (lib/projectUpdates.ts).

// NormaliseText is the text as stored: line endings unified, trailing spaces
// gone, at most one blank line in a row, none at either end.
func NormaliseText(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// listItem is a list line's text, or false.
func listItem(line string) (string, bool) {
	t := strings.TrimLeft(line, " \t")
	for _, p := range []string{"- ", "* ", "• "} {
		if strings.HasPrefix(t, p) {
			return strings.TrimSpace(t[len(p):]), true
		}
	}
	return "", false
}

// PlainTextToHTML is the text as escaped HTML: paragraphs, line breaks and
// lists. Empty text is empty HTML.
func PlainTextToHTML(s string) string {
	s = NormaliseText(s)
	if s == "" {
		return ""
	}
	var sb strings.Builder
	for _, block := range strings.Split(s, "\n\n") {
		inList := false
		var para []string
		flush := func() {
			if len(para) > 0 {
				sb.WriteString("<p>" + strings.Join(para, "<br>") + "</p>")
				para = nil
			}
		}
		for _, l := range strings.Split(block, "\n") {
			if item, ok := listItem(l); ok {
				flush()
				if !inList {
					sb.WriteString("<ul>")
					inList = true
				}
				sb.WriteString("<li>" + html.EscapeString(item) + "</li>")
				continue
			}
			if inList {
				sb.WriteString("</ul>")
				inList = false
			}
			para = append(para, html.EscapeString(l))
		}
		if inList {
			sb.WriteString("</ul>")
		}
		flush()
	}
	return sb.String()
}

// OneLine is the start of the text on a single line, at most n characters,
// ending in "…" when it was cut: for a subject, a notification or a preview.
func OneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n <= 1 {
		return TruncateRunes("…", n)
	}
	return strings.TrimSpace(TruncateRunes(s, n-1)) + "…"
}

// Count is a number of things as a sentence says it: "1 task", "3 tasks".
func Count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
