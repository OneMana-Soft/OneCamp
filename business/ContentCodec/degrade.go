package contentcodec

// degrade keeps a block the schema has no node for as well as it can.
//
// A simple table (until the editor has one) becomes its Markdown in a code
// block: nothing in it is lost, it reads as a table, and it can be pasted
// back as one later. A callout becomes a quote, a toggle its lines (an old
// one's title in bold first), a task list a bulleted list, an embed its
// Markdown link, a table of contents nothing.
// Anything else keeps its content.
func (n *normalizer) degrade(c *Node) []*Node {
	switch c.Type {
	case "table":
		w := &mdWriter{}
		lines := w.table(Normalize(&Node{Type: "doc", Content: []*Node{c}}, everything()).Content[0])
		text := ""
		for i, l := range lines {
			if i > 0 {
				text += "\n"
			}
			text += l
		}
		cb := &Node{Type: "codeBlock", Attrs: map[string]any{"language": "markdown"}}
		if text != "" {
			cb.Content = []*Node{{Type: "text", Text: text}}
		}
		return n.block(cb)
	case "callout":
		return n.block(&Node{Type: "blockquote", Content: c.Content})
	case "collapsible":
		title := c.attrString("title")
		out := []*Node{}
		if title != "" {
			out = append(out, &Node{Type: "paragraph", Content: []*Node{{Type: "text", Text: title, Marks: []*Mark{{Type: "bold"}}}}})
		}
		return append(n.blocks(out), n.blocks(c.Content)...)
	case "taskList":
		return n.block(&Node{Type: "bulletList", Content: c.Content})
	case "toc":
		// What it lists is worked out where it is shown: nothing to keep.
		return nil
	case "blockMath":
		cb := &Node{Type: "codeBlock", Attrs: map[string]any{"language": "math"}}
		if latex := c.attrString("latex"); latex != "" {
			cb.Content = []*Node{{Type: "text", Text: latex}}
		}
		return n.block(cb)
	}
	if s := specOf(c.Type); s != nil && s.content == holdsNothing {
		w := &mdWriter{}
		lines := w.block(c, false)
		if len(lines) == 0 {
			return nil
		}
		md := ""
		for i, l := range lines {
			if i > 0 {
				md += "\n"
			}
			md += l
		}
		doc, err := FromMarkdown(md, n.schema)
		if err != nil || doc == nil {
			return nil
		}
		return doc.Content
	}
	return n.blocks(c.Content)
}
