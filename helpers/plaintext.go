package helpers

import (
	"html"
	"strconv"
	"strings"
	"unicode/utf8"

	xhtml "golang.org/x/net/html"
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

// HTMLToText is rich text (a task's description) as plain text that
// PlainTextToHTML reads back the same: a paragraph, heading, quote or code
// block ends with a blank line, a line break is one, and a list item is a line
// of its own, "- " (numbered in an ordered list; "[x] " or "[ ] " after it in a
// to-do list). Other markup goes, entities are decoded, and white space
// collapses as HTML shows it, except in a code block. For text read outside
// the app and brought back in (a task list's CSV).
func HTMLToText(s string) string {
	var sb strings.Builder
	// lists holds each open list's next number: 0 for one with bullets.
	var lists []int
	pre := 0
	lineStart := func() bool {
		out := sb.String()
		return out == "" || out[len(out)-1] == '\n'
	}
	z := xhtml.NewTokenizer(strings.NewReader(s))
	for {
		switch z.Next() {
		case xhtml.ErrorToken:
			return NormaliseText(sb.String())
		case xhtml.TextToken:
			text := string(z.Text())
			if pre == 0 {
				text = collapseSpaces(text)
				if lineStart() {
					text = strings.TrimLeft(text, " ")
				}
			}
			sb.WriteString(text)
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			name, more := z.TagName()
			switch string(name) {
			case "br":
				sb.WriteString("\n")
			case "pre":
				pre++
			case "ul":
				lists = append(lists, 0)
			case "ol":
				lists = append(lists, 1)
			case "li":
				if n := len(lists); n > 0 && lists[n-1] > 0 {
					sb.WriteString("\n" + strconv.Itoa(lists[n-1]) + ". ")
					lists[n-1]++
				} else {
					sb.WriteString("\n- ")
				}
				for more {
					var key, val []byte
					key, val, more = z.TagAttr()
					if string(key) != "data-checked" {
						continue
					}
					if string(val) == "true" {
						sb.WriteString("[x] ")
					} else {
						sb.WriteString("[ ] ")
					}
				}
			}
		case xhtml.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "ul", "ol":
				if len(lists) > 0 {
					lists = lists[:len(lists)-1]
				}
				if len(lists) == 0 {
					sb.WriteString("\n\n")
				}
			case "pre":
				pre = max(pre-1, 0)
				sb.WriteString("\n\n")
			case "p", "div", "blockquote", "h1", "h2", "h3", "h4", "h5", "h6":
				// A list item's paragraphs stay on its line.
				if len(lists) > 0 {
					sb.WriteString(" ")
				} else {
					sb.WriteString("\n\n")
				}
			}
		}
	}
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
