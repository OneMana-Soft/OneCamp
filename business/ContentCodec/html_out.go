package contentcodec

import (
	"github.com/akashc777/OneCamp/helpers"
	"strings"
)

// ToHTML writes a doc as the codec's canonical HTML: the elements and
// attributes the editor's schema renders (as the collaboration service saves a
// body, without the XML namespace and with HTML void elements), so the editor
// and the service both read it back as the same doc.
//
// Escaping is fixed, and the TypeScript codec does the same: in text & < >
// and the no-break space; in attribute values only & and ". The collaboration
// service's DOM (happy-dom) decodes no other reference inside an attribute, so
// a "&#10;" there would come back as those five characters; a raw line break,
// < or > inside a quoted value is valid HTML and reads back as itself.
func ToHTML(doc *Node) string {
	var b strings.Builder
	if doc == nil {
		return ""
	}
	if doc.Type == "doc" {
		writeBlocks(&b, doc.Content)
	} else {
		writeBlocks(&b, []*Node{doc})
	}
	return b.String()
}

func writeBlocks(b *strings.Builder, nodes []*Node) {
	for _, n := range nodes {
		writeNode(b, n)
	}
}

func writeNode(b *strings.Builder, n *Node) {
	if n == nil {
		return
	}
	switch n.Type {
	case "paragraph":
		b.WriteString("<p")
		writeGlobals(b, n)
		b.WriteString(">")
		writeInline(b, n.Content)
		b.WriteString("</p>")
	case "heading":
		l := clampLevel(n.attrInt("level", 1))
		tag := "h" + string(rune('0'+l))
		b.WriteString("<" + tag)
		writeGlobals(b, n)
		b.WriteString(">")
		writeInline(b, n.Content)
		b.WriteString("</" + tag + ">")
	case "blockquote":
		b.WriteString("<blockquote")
		writeGlobals(b, n)
		b.WriteString(">")
		writeBlocks(b, n.Content)
		b.WriteString("</blockquote>")
	case "bulletList":
		b.WriteString("<ul")
		writeGlobals(b, n)
		b.WriteString(">")
		writeBlocks(b, n.Content)
		b.WriteString("</ul>")
	case "orderedList":
		b.WriteString("<ol")
		if s := n.attrInt("start", 1); s != 1 {
			writeAttr(b, "start", itoa(int64(s)))
		}
		if t := n.attrString("type"); t != "" {
			writeAttr(b, "type", t)
		}
		writeGlobals(b, n)
		b.WriteString(">")
		writeBlocks(b, n.Content)
		b.WriteString("</ol>")
	case "listItem":
		b.WriteString("<li")
		writeGlobals(b, n)
		b.WriteString(">")
		writeBlocks(b, n.Content)
		b.WriteString("</li>")
	case "taskList":
		b.WriteString(`<ul class="task-list" data-type="taskList"`)
		writeGlobals(b, n)
		b.WriteString(">")
		writeBlocks(b, n.Content)
		b.WriteString("</ul>")
	case "taskItem":
		checked := n.attrBool("checked", false)
		b.WriteString(`<li class="task-item"`)
		if checked {
			writeAttr(b, "data-checked", "true")
		} else {
			writeAttr(b, "data-checked", "false")
		}
		b.WriteString(` data-type="taskItem"`)
		writeGlobals(b, n)
		b.WriteString(`><label><input type="checkbox"`)
		if checked {
			b.WriteString(` checked="checked"`)
		}
		b.WriteString("><span></span></label><div>")
		writeBlocks(b, n.Content)
		b.WriteString("</div></li>")
	case "codeBlock":
		b.WriteString("<pre")
		writeGlobals(b, n)
		if n.attrBool("wrap", false) {
			writeAttr(b, "data-wrap", "true")
		}
		if v := n.attrString("caption"); v != "" {
			writeAttr(b, "data-caption", v)
		}
		b.WriteString("><code")
		if lang := n.attrString("language"); lang != "" {
			writeAttr(b, "class", "language-"+lang)
		}
		b.WriteString(">")
		for _, c := range n.Content {
			if c.Type == "text" {
				b.WriteString(escapeText(c.Text))
			}
		}
		b.WriteString("</code></pre>")
	case "horizontalRule":
		b.WriteString("<hr")
		writeGlobals(b, n)
		b.WriteString(">")
	case "blockMath":
		// The TeX as the element's text too, as the editor writes it.
		latex := n.attrString("latex")
		b.WriteString(`<div data-type="block-math"`)
		if latex != "" {
			writeAttr(b, "data-latex", latex)
		}
		b.WriteString(">" + escapeText(latex) + "</div>")
	case "image":
		b.WriteString("<img")
		for _, a := range [][2]string{
			{"src", "src"}, {"alt", "alt"}, {"title", "title"}, {"width", "width"}, {"height", "height"},
			{"id", "id"}, {"caption", "caption"}, {"fileName", "filename"},
			imageMediaAttrs[0], imageMediaAttrs[1], imageMediaAttrs[2],
		} {
			if v := n.attrString(a[0]); v != "" {
				writeAttr(b, a[1], v)
			}
		}
		writeGlobals(b, n)
		b.WriteString(">")
	case "callout":
		b.WriteString(`<div data-type="callout"`)
		if v := n.attrString("emoji"); v != "" {
			writeAttr(b, "data-emoji", v)
		}
		if v := n.attrString("color"); v != "" {
			writeAttr(b, "data-color", v)
		}
		if v := n.attrString("icon"); v != "" {
			writeAttr(b, "data-icon", v)
		}
		writeGlobals(b, n)
		b.WriteString(">")
		writeBlocks(b, n.Content)
		b.WriteString("</div>")
	case "collapsible":
		// Its first line is its summary; open or closed is each reader's own (B2).
		b.WriteString(`<div data-type="collapsible"`)
		writeGlobals(b, n)
		b.WriteString(">")
		writeBlocks(b, n.Content)
		b.WriteString("</div>")
	case "toc":
		b.WriteString(`<div data-type="toc"`)
		writeGlobals(b, n)
		b.WriteString("></div>")
	case "tableEmbed":
		writeEmbed(b, n, "table-embed", [][2]string{{"tableId", "data-table-id"}, {"viewId", "data-view-id"}})
	case "poll":
		writeEmbed(b, n, "poll", [][2]string{{"id", "data-id"}})
	case "chartEmbed":
		writeEmbed(b, n, "chart", [][2]string{{"spec", "data-spec"}})
	case "diffEmbed":
		writeEmbed(b, n, "diff", [][2]string{{"diff", "data-diff"}})
	case "recordingEmbed":
		writeEmbed(b, n, "recording-embed", [][2]string{
			{"egress", "data-egress"}, {"kind", "data-kind"}, {"sid", "data-sid"}, {"u1", "data-u1"}, {"u2", "data-u2"},
		})
	case "table":
		b.WriteString("<table><tbody>")
		writeBlocks(b, n.Content)
		b.WriteString("</tbody></table>")
	case "tableRow":
		b.WriteString("<tr>")
		writeBlocks(b, n.Content)
		b.WriteString("</tr>")
	case "tableHeader", "tableCell":
		tag := "td"
		if n.Type == "tableHeader" {
			tag = "th"
		}
		b.WriteString("<" + tag)
		if c := n.attrInt("colspan", 1); c != 1 {
			writeAttr(b, "colspan", itoa(int64(c)))
		}
		if r := n.attrInt("rowspan", 1); r != 1 {
			writeAttr(b, "rowspan", itoa(int64(r)))
		}
		if ws, ok := n.attr("colwidth").([]any); ok && len(ws) > 0 {
			parts := make([]string, len(ws))
			for i, w := range ws {
				parts[i] = stringOf(w)
			}
			writeAttr(b, "colwidth", strings.Join(parts, ","))
		}
		if v := n.attrString("background"); v != "" {
			writeAttr(b, "data-background", v)
		}
		b.WriteString(">")
		writeBlocks(b, n.Content)
		b.WriteString("</" + tag + ">")
	case "columnList":
		b.WriteString(`<div data-type="column-list"`)
		writeGlobals(b, n)
		b.WriteString(">")
		writeBlocks(b, n.Content)
		b.WriteString("</div>")
	case "column":
		b.WriteString(`<div data-type="column"`)
		if w := n.attrInt("width", 0); w > 0 {
			writeAttr(b, "data-width", itoa(int64(w)))
		}
		writeGlobals(b, n)
		b.WriteString(">")
		writeBlocks(b, n.Content)
		b.WriteString("</div>")
	case "doc":
		writeBlocks(b, n.Content)
	default:
		if writeMediaBlock(b, n) || writeConnectedBlock(b, n) {
			return
		}
		// Inline content where a block belongs, or a type with no rule: its
		// content, so nothing is lost.
		if isInlineNode(n) {
			b.WriteString("<p>")
			writeInline(b, []*Node{n})
			b.WriteString("</p>")
			return
		}
		writeBlocks(b, n.Content)
	}
}

func clampLevel(l int) int {
	if l < 1 {
		return 1
	}
	if l > 6 {
		return 6
	}
	return l
}

func writeEmbed(b *strings.Builder, n *Node, dataType string, attrs [][2]string) {
	b.WriteString(`<div data-type="` + dataType + `"`)
	for _, a := range attrs {
		if v := n.attrString(a[0]); v != "" {
			writeAttr(b, a[1], v)
		}
	}
	writeGlobals(b, n)
	b.WriteString("></div>")
}

// writeInline writes inline content with its marks, keeping a mark open over
// neighbours that share it, as ProseMirror's serializer does: **a _b_** is one
// <strong> holding an <em>.
func writeInline(b *strings.Builder, nodes []*Node) {
	var active []*Mark
	closeTo := func(keep int) {
		for len(active) > keep {
			b.WriteString(markClose(active[len(active)-1]))
			active = active[:len(active)-1]
		}
	}
	for _, n := range nodes {
		if n == nil {
			continue
		}
		marks := n.Marks
		keep := 0
		for keep < len(active) && keep < len(marks) && active[keep].eq(marks[keep]) {
			keep++
		}
		closeTo(keep)
		for _, m := range marks[keep:] {
			b.WriteString(markOpen(m))
			active = append(active, m)
		}
		writeInlineNode(b, n)
	}
	closeTo(0)
}

func writeInlineNode(b *strings.Builder, n *Node) {
	switch n.Type {
	case "text":
		b.WriteString(escapeText(n.Text))
	case "hardBreak":
		b.WriteString("<br>")
	case "mention":
		b.WriteString(`<span data-type="mention"`)
		writeMentionAttrs(b, n)
		b.WriteString(">" + escapeText("@"+helpers.FirstNonEmpty(n.attrString("label"), n.attrString("id"))) + "</span>")
	case "channelMention":
		b.WriteString(`<span data-type="channelMention" class="channel-mention"`)
		writeMentionAttrs(b, n)
		b.WriteString(">" + escapeText("#"+helpers.FirstNonEmpty(n.attrString("label"), n.attrString("id"))) + "</span>")
	case "entityMention":
		b.WriteString(`<span data-type="entityMention" class="entity-mention"`)
		writeMentionAttrs(b, n)
		b.WriteString(">" + escapeText(helpers.FirstNonEmpty(n.attrString("label"), n.attrString("id"))) + "</span>")
	case "inlineMath":
		latex := n.attrString("latex")
		b.WriteString(`<span data-type="inline-math"`)
		if latex != "" {
			writeAttr(b, "data-latex", latex)
		}
		b.WriteString(">" + escapeText(latex) + "</span>")
	case "linkMention":
		writeLinkMention(b, n)
	case "dateMention":
		writeDateMention(b, n)
	default:
		for _, c := range n.Content {
			writeInlineNode(b, c)
		}
	}
}

func writeMentionAttrs(b *strings.Builder, n *Node) {
	if v := n.attrString("id"); v != "" {
		writeAttr(b, "data-id", v)
	}
	if v := n.attrString("label"); v != "" {
		writeAttr(b, "data-label", v)
	}
	ch := n.attrString("mentionSuggestionChar")
	if ch == "" {
		ch = "@"
	}
	writeAttr(b, "data-mention-suggestion-char", ch)
}

func markOpen(m *Mark) string {
	switch m.Type {
	case "bold":
		return "<strong>"
	case "italic":
		return "<em>"
	case "strike":
		return "<s>"
	case "code":
		return "<code>"
	case "underline":
		return "<u>"
	case "link":
		var b strings.Builder
		b.WriteString("<a")
		for _, a := range []string{"target", "rel", "class", "href"} {
			if v := m.attrString(a); v != "" {
				writeAttr(&b, a, v)
			}
		}
		b.WriteString(">")
		return b.String()
	case "textStyle":
		// By name (B2, N15): the colour means the same in either theme and anywhere the doc goes.
		if c := colourName(m.attrString("color")); c != "" {
			var b strings.Builder
			b.WriteString("<span")
			writeAttr(&b, "data-color", c)
			b.WriteString(">")
			return b.String()
		}
		return "<span>"
	case "highlight":
		var b strings.Builder
		b.WriteString("<mark")
		writeAttr(&b, "data-color", highlightColour(m.attrString("color")))
		b.WriteString(">")
		return b.String()
	}
	return ""
}

func markClose(m *Mark) string {
	switch m.Type {
	case "bold":
		return "</strong>"
	case "italic":
		return "</em>"
	case "strike":
		return "</s>"
	case "code":
		return "</code>"
	case "underline":
		return "</u>"
	case "link":
		return "</a>"
	case "textStyle":
		return "</span>"
	case "highlight":
		return "</mark>"
	}
	return ""
}

func writeAttr(b *strings.Builder, name, value string) {
	b.WriteString(" " + name + `="` + escapeAttr(value) + `"`)
}

var textEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\u00a0", "&nbsp;")
var attrEscaper = strings.NewReplacer("&", "&amp;", `"`, "&quot;")

func escapeText(s string) string { return textEscaper.Replace(s) }
func escapeAttr(s string) string { return attrEscaper.Replace(s) }
