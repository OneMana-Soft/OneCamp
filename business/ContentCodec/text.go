package contentcodec

import (
	"strings"
)

// FromText reads plain text as a doc: a blank line ends a paragraph, a line
// break inside one is kept as one, and web addresses become links. Nothing in
// it is read as Markdown: a "#" or a "*" is just that character.
func FromText(text string) *Node {
	text = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\x00", "\uFFFD").Replace(strings.ToValidUTF8(text, "\uFFFD"))
	doc := newNode("doc")
	var lines []string
	flush := func() {
		if len(lines) == 0 {
			return
		}
		p := newNode("paragraph")
		for i, l := range lines {
			if i > 0 {
				p.Content = append(p.Content, newNode("hardBreak"))
			}
			if l != "" {
				p.Content = append(p.Content, &Node{Type: "text", Text: l})
			}
		}
		doc.Content = append(doc.Content, p)
		lines = nil
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRightFunc(line, isSpaceRune)
		if line == "" {
			flush()
			continue
		}
		lines = append(lines, line)
	}
	flush()
	linkify(doc)
	return Normalize(doc, nil)
}
