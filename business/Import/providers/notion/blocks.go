// Notion block-tree → HTML renderer.
//
// Notion's content model is a tree of "blocks". Each block has a `type`
// and a payload-shaped object under that key. Rich text is itself a
// list of "rich_text" segments, each carrying annotations + plain text.
//
// We render the subset of block types that meaningfully show up in
// task descriptions:
//   paragraph, heading_1/2/3, bulleted_list_item, numbered_list_item,
//   to_do, code, quote, callout, divider, image, file, bookmark, link,
//   table, table_row, equation.
//
// Unsupported types render as a small "<div class='notion-unsupported'>"
// chip with the type name so the operator sees that something was
// dropped — matches the conservative behaviour of the slack mrkdwn
// renderer.
//
// Block trees are rendered depth-first. Lists are coalesced when
// consecutive bulleted/numbered items appear at the same level.

package notion

import (
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

// renderBlocks turns a slice of decoded blocks into HTML.
// blocks may have nested children fetched via /v1/blocks/{id}/children
// before this is called; we follow .Children when present.
func renderBlocks(blocks []notionBlock) string {
	var b strings.Builder
	renderBlocksInto(blocks, &b)
	return b.String()
}

func renderBlocksInto(blocks []notionBlock, b *strings.Builder) {
	// Group consecutive list items so we wrap them in <ul>/<ol>.
	i := 0
	for i < len(blocks) {
		blk := blocks[i]
		switch blk.Type {
		case "bulleted_list_item":
			j := i
			for j < len(blocks) && blocks[j].Type == "bulleted_list_item" {
				j++
			}
			b.WriteString("<ul>")
			for _, item := range blocks[i:j] {
				b.WriteString("<li>")
				if item.BulletedListItem != nil {
					b.WriteString(renderRichText(item.BulletedListItem.RichText))
				}
				if len(item.Children) > 0 {
					renderBlocksInto(item.Children, b)
				}
				b.WriteString("</li>")
			}
			b.WriteString("</ul>")
			i = j
			continue
		case "numbered_list_item":
			j := i
			for j < len(blocks) && blocks[j].Type == "numbered_list_item" {
				j++
			}
			b.WriteString("<ol>")
			for _, item := range blocks[i:j] {
				b.WriteString("<li>")
				if item.NumberedListItem != nil {
					b.WriteString(renderRichText(item.NumberedListItem.RichText))
				}
				if len(item.Children) > 0 {
					renderBlocksInto(item.Children, b)
				}
				b.WriteString("</li>")
			}
			b.WriteString("</ol>")
			i = j
			continue
		}
		renderOneBlock(blk, b)
		i++
	}
}

func renderOneBlock(blk notionBlock, b *strings.Builder) {
	switch blk.Type {
	case "paragraph":
		if blk.Paragraph == nil {
			return
		}
		b.WriteString("<p>")
		b.WriteString(renderRichText(blk.Paragraph.RichText))
		b.WriteString("</p>")
		if len(blk.Children) > 0 {
			renderBlocksInto(blk.Children, b)
		}
	case "heading_1":
		if blk.Heading1 == nil {
			return
		}
		b.WriteString("<h1>")
		b.WriteString(renderRichText(blk.Heading1.RichText))
		b.WriteString("</h1>")
	case "heading_2":
		if blk.Heading2 == nil {
			return
		}
		b.WriteString("<h2>")
		b.WriteString(renderRichText(blk.Heading2.RichText))
		b.WriteString("</h2>")
	case "heading_3":
		if blk.Heading3 == nil {
			return
		}
		b.WriteString("<h3>")
		b.WriteString(renderRichText(blk.Heading3.RichText))
		b.WriteString("</h3>")
	case "to_do":
		if blk.ToDo == nil {
			return
		}
		checked := ""
		if blk.ToDo.Checked {
			checked = " checked"
		}
		b.WriteString(fmt.Sprintf(`<p><input type="checkbox" disabled%s /> `, checked))
		b.WriteString(renderRichText(blk.ToDo.RichText))
		b.WriteString("</p>")
		if len(blk.Children) > 0 {
			renderBlocksInto(blk.Children, b)
		}
	case "code":
		if blk.Code == nil {
			return
		}
		lang := ""
		if blk.Code.Language != "" {
			lang = ` class="language-` + escapeHTMLAttr(blk.Code.Language) + `"`
		}
		b.WriteString("<pre><code")
		b.WriteString(lang)
		b.WriteString(">")
		b.WriteString(renderRichTextPlain(blk.Code.RichText))
		b.WriteString("</code></pre>")
	case "quote":
		if blk.Quote == nil {
			return
		}
		b.WriteString("<blockquote>")
		b.WriteString(renderRichText(blk.Quote.RichText))
		b.WriteString("</blockquote>")
	case "callout":
		if blk.Callout == nil {
			return
		}
		b.WriteString(`<div class="notion-callout">`)
		b.WriteString(renderRichText(blk.Callout.RichText))
		if len(blk.Children) > 0 {
			renderBlocksInto(blk.Children, b)
		}
		b.WriteString("</div>")
	case "divider":
		b.WriteString("<hr/>")
	case "image":
		if blk.Image == nil {
			return
		}
		src := blk.Image.fileURL()
		alt := renderRichTextPlain(blk.Image.Caption)
		if src != "" {
			b.WriteString(fmt.Sprintf(`<img src="%s" alt="%s"/>`, escapeHTMLAttr(src), escapeHTMLAttr(alt)))
		}
	case "file":
		if blk.File == nil {
			return
		}
		src := blk.File.fileURL()
		name := renderRichTextPlain(blk.File.Caption)
		if name == "" {
			name = blk.File.Name
		}
		if src != "" {
			b.WriteString(fmt.Sprintf(`<a href="%s" target="_blank" rel="noopener noreferrer">%s</a>`,
				escapeHTMLAttr(src), htmlEscapeText(helpers.FirstNonEmpty(name, "file"))))
		}
	case "bookmark":
		if blk.Bookmark == nil {
			return
		}
		b.WriteString(fmt.Sprintf(`<a href="%s" target="_blank" rel="noopener noreferrer">%s</a>`,
			escapeHTMLAttr(blk.Bookmark.URL), htmlEscapeText(blk.Bookmark.URL)))
	case "equation":
		if blk.Equation == nil {
			return
		}
		// We don't run a TeX renderer on import; surface the raw expr.
		b.WriteString(`<code class="notion-equation">`)
		b.WriteString(htmlEscapeText(blk.Equation.Expression))
		b.WriteString("</code>")
	case "table":
		// Children of a table are table_row blocks. The orchestrator
		// passes them in via blk.Children.
		if len(blk.Children) == 0 {
			return
		}
		b.WriteString("<table>")
		for _, row := range blk.Children {
			if row.Type != "table_row" || row.TableRow == nil {
				continue
			}
			b.WriteString("<tr>")
			for _, cell := range row.TableRow.Cells {
				b.WriteString("<td>")
				b.WriteString(renderRichText(cell))
				b.WriteString("</td>")
			}
			b.WriteString("</tr>")
		}
		b.WriteString("</table>")
	default:
		// Unknown / unsupported block type. Surface as a styled chip so
		// the operator can spot what we dropped.
		b.WriteString(`<div class="notion-unsupported" data-type="`)
		b.WriteString(escapeHTMLAttr(blk.Type))
		b.WriteString(`">[Notion ` + escapeHTMLAttr(blk.Type) + ` block]</div>`)
	}
}

// renderRichText turns a Notion rich_text array into HTML.
// Annotations: bold, italic, strikethrough, underline, code, color.
// Links: text.link.url.
//
// We don't model Notion's user/page mention links as OneCamp mentions
// because the OC Dgraph mention model is post/chat/comment-scoped and
// we'd need another id_map indirection. Instead, mentions render as
// <span class="notion-mention">@name</span> so the FE can style them
// distinctively without a schema change.
func renderRichText(rts []notionRichText) string {
	var b strings.Builder
	for _, rt := range rts {
		text := htmlEscapeText(rt.PlainText)
		if rt.Type == "mention" && rt.Mention != nil {
			name := rt.PlainText
			b.WriteString(`<span class="notion-mention">`)
			b.WriteString(htmlEscapeText(name))
			b.WriteString(`</span>`)
			continue
		}
		// Apply annotations innermost-first so the markup nests correctly.
		if rt.Annotations.Code {
			text = "<code>" + text + "</code>"
		}
		if rt.Annotations.Bold {
			text = "<strong>" + text + "</strong>"
		}
		if rt.Annotations.Italic {
			text = "<em>" + text + "</em>"
		}
		if rt.Annotations.Strikethrough {
			text = "<s>" + text + "</s>"
		}
		if rt.Annotations.Underline {
			text = "<u>" + text + "</u>"
		}
		if rt.Text != nil && rt.Text.Link != nil && rt.Text.Link.URL != "" {
			text = `<a href="` + escapeHTMLAttr(rt.Text.Link.URL) +
				`" target="_blank" rel="noopener noreferrer">` + text + `</a>`
		}
		b.WriteString(text)
	}
	return b.String()
}

// renderRichTextPlain returns the unannotated text. Used inside <pre>
// (where HTML markup would corrupt the code) and inside attribute
// contexts (alt text, etc.).
func renderRichTextPlain(rts []notionRichText) string {
	var b strings.Builder
	for _, rt := range rts {
		b.WriteString(htmlEscapeText(rt.PlainText))
	}
	return b.String()
}

// htmlEscapeText escapes HTML body text.
func htmlEscapeText(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}

// escapeHTMLAttr escapes for use inside an HTML attribute value.
// Adds the quote characters since attribute parsers are stricter.
func escapeHTMLAttr(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}
