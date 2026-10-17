package contentcodec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	gast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"golang.org/x/net/html"

	"github.com/akashc777/OneCamp/helpers"
)

// ErrTooCostly is Markdown that would take the parser too long to read
// (helpers.MarkdownTooCostly): a doc goldmark's time grows much faster than
// the length of, in a paragraph or a line, or in its nesting.
var ErrTooCostly = errors.New("the markdown would take too long to read")

// ErrTooDeep is the ErrTooCostly of Markdown whose quotes or lists nest deeper
// than a parser should be asked to read (helpers.MarkdownTooDeep).
var ErrTooDeep = fmt.Errorf("%w: it nests quotes or lists more than %d levels deep", ErrTooCostly, helpers.MaxMarkdownNesting)

// FromMarkdown reads GitHub-flavoured Markdown as a doc, in canonical form for
// schema (nil: the shipped schema).
//
// It reads what ToMarkdown writes back to the same doc, and Markdown from
// elsewhere as well as the schema allows: a table the schema has no node for
// is kept as its Markdown in a code block; raw HTML is read as foreign HTML;
// bare web addresses become links. ErrTooDeep for a line that may sit more
// than helpers.MaxMarkdownNesting quotes and lists deep, and ErrTooCostly for
// Markdown whose reading would cost more than helpers.MaxMarkdownSteps: the
// parser's time grows faster than the nesting (a line of 500,000 ">" took
// six minutes and half a gigabyte of stack) and, for some text, than a
// paragraph's length ("*a_ " repeated: a second at 64 KB, over 45 s at
// 256 KB), and the public API takes a megabyte.
func FromMarkdown(md string, schema *Schema) (*Node, error) {
	if helpers.MarkdownTooDeep(md) {
		return nil, ErrTooDeep
	}
	if helpers.MarkdownTooCostly(md, helpers.DocMarkdown) {
		return nil, ErrTooCostly
	}
	return readMarkdown(md, schema), nil
}

// readMarkdown is FromMarkdown's reading, past the guard: in time linear in
// md for what the guard lets through.
func readMarkdown(md string, schema *Schema) *Node {
	// As CommonMark (and markdown-it) prepare input: line ends as \n, and NUL
	// as the replacement character before anything is read.
	md = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\x00", "\uFFFD").Replace(strings.ToValidUTF8(md, "\uFFFD"))
	// A last line of only spaces or tabs is dropped, as markdown-it drops it
	// and goldmark keeps it in a code block left open.
	md = trailingBlankLine.ReplaceAllString(md, "\n")
	src := []byte(md)
	root := markdownParser.Parser().Parse(text.NewReader(src))
	r := &mdReader{src: src}
	doc := newNode("doc")
	doc.Content = r.blocks(root)
	trimTextblocks(doc)
	joinText(doc)
	linkify(doc)
	dropEmptyParagraphs(doc)
	return Normalize(doc, schema)
}

var trailingBlankLine = regexp.MustCompile("\n[ \t]+$")

// joinText joins neighbouring text with the same marks before addresses are
// looked for: goldmark leaves text split where a delimiter matched nothing
// ("snake_case"), markdown-it joins it, and an address must not end at the
// split in one codec only.
func joinText(n *Node) {
	if len(n.Content) == 0 {
		return
	}
	n.Content = mergeText(n.Content)
	for _, c := range n.Content {
		joinText(c)
	}
}

// trimTextblocks trims the whitespace a paragraph or heading starts and ends
// with. goldmark keeps a leading no-break space that markdown-it drops; the
// codecs agree by dropping it in both.
func trimTextblocks(n *Node) {
	if n.Type == "paragraph" || n.Type == "heading" {
		n.Content = trimInlineEdges(n.Content)
		return
	}
	for _, c := range n.Content {
		trimTextblocks(c)
	}
}

// trimInlineEdges drops the whitespace and line breaks (which write nothing
// in Markdown either) inline content starts and ends with, through as many
// nodes as that takes.
func trimInlineEdges(content []*Node) []*Node {
	for len(content) > 0 {
		first := content[0]
		if first.Type == "text" && !hasMark(first.Marks, "code") {
			first.Text = strings.TrimLeftFunc(first.Text, isSpaceRune)
		}
		if first.Type != "hardBreak" && (first.Type != "text" || first.Text != "") {
			break
		}
		content = content[1:]
	}
	for len(content) > 0 {
		last := content[len(content)-1]
		if last.Type == "text" && !hasMark(last.Marks, "code") {
			last.Text = strings.TrimRightFunc(last.Text, isSpaceRune)
		}
		if last.Type != "hardBreak" && (last.Type != "text" || last.Text != "") {
			break
		}
		content = content[:len(content)-1]
	}
	return content
}

// blankInline reports whether inline content is nothing but whitespace.
func blankInline(content []*Node) bool {
	for _, c := range content {
		if c.Type != "text" || !blank(c.Text) {
			return false
		}
	}
	return true
}

// dropEmptyParagraphs removes paragraphs with nothing in them. Markdown has no
// way to write one, so one read from it (inline HTML that held nothing) would
// not survive a round trip; Normalize puts back the ones a node needs (an
// item's first). A toggle's first line stays, empty or not: it is its
// <summary>, and without it the toggle's first block would take its place.
func dropEmptyParagraphs(n *Node) {
	if len(n.Content) == 0 {
		return
	}
	kept := n.Content[:0]
	for i, c := range n.Content {
		if c.Type == "paragraph" && blankInline(c.Content) && !(n.Type == "collapsible" && i == 0) {
			continue
		}
		dropEmptyParagraphs(c)
		kept = append(kept, c)
	}
	n.Content = kept
}

var markdownParser = goldmark.New(
	goldmark.WithExtensions(extension.Table, extension.TaskList, doubleTilde{}, dollarMath{}),
)

type mdReader struct {
	src []byte
}

// blocks reads a container's children, folding <details> … </details> runs
// into toggles.
func (r *mdReader) blocks(parent gast.Node) []*Node {
	var out []*Node
	type openToggle struct {
		node *Node
	}
	var stack []*openToggle
	emit := func(ns ...*Node) {
		if len(stack) > 0 {
			t := stack[len(stack)-1]
			t.node.Content = append(t.node.Content, ns...)
			return
		}
		out = append(out, ns...)
	}
	for c := parent.FirstChild(); c != nil; c = c.NextSibling() {
		if hb, ok := c.(*gast.HTMLBlock); ok {
			raw := string(r.htmlBlockText(hb))
			if summary, rest, ok := detailsOpen(raw); ok {
				t := &openToggle{node: newNode("collapsible")}
				t.node.Content = summary
				stack = append(stack, t)
				if trimSpace(rest) != "" {
					emit(r.rawHTMLBlocks(rest)...)
				}
				continue
			}
			if before, ok := detailsClose(raw); ok && len(stack) > 0 {
				if trimSpace(before) != "" {
					emit(r.rawHTMLBlocks(before)...)
				}
				t := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				emit(t.node)
				continue
			}
			emit(r.rawHTMLBlocks(raw)...)
			continue
		}
		emit(r.block(c)...)
	}
	// A <details> never closed holds everything after it.
	for len(stack) > 0 {
		t := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		emit(t.node)
	}
	return out
}

func (r *mdReader) htmlBlockText(hb *gast.HTMLBlock) []byte {
	var b bytes.Buffer
	lines := hb.Lines()
	for i := 0; i < lines.Len(); i++ {
		s := lines.At(i)
		b.Write(s.Value(r.src))
	}
	if hb.HasClosure() {
		b.Write(hb.ClosureLine.Value(r.src))
	}
	return b.Bytes()
}

// detailsOpen recognises an HTML block that opens a toggle: <details> with
// its <summary>, and no closing </details> in the same block. The summary is
// the toggle's first line, and whether it was open is each reader's own.
func detailsOpen(raw string) (summary []*Node, rest string, ok bool) {
	t := trimSpace(raw)
	low := strings.ToLower(t)
	if !strings.HasPrefix(low, "<details") {
		return nil, "", false
	}
	end := strings.IndexByte(t, '>')
	if end < 0 {
		return nil, "", false
	}
	if strings.Contains(low, "</details>") {
		return nil, "", false
	}
	rest = t[end+1:]
	inner := ""
	lowRest := strings.ToLower(rest)
	if s := strings.Index(lowRest, "<summary"); s >= 0 {
		gt := strings.IndexByte(rest[s:], '>')
		ce := strings.Index(lowRest, "</summary>")
		if gt >= 0 && ce > s+gt {
			inner = rest[s+gt+1 : ce]
			rest = rest[ce+len("</summary>"):]
		}
	}
	return summaryBlocks(inner), rest, true
}

// summaryBlocks reads a <summary>'s HTML as a toggle's first line: a heading
// when it holds one (a toggle heading), else a paragraph, then anything else
// it held.
func summaryBlocks(inner string) []*Node {
	var out []*Node
	if doc, err := FromHTML(inner, HTMLOptions{Foreign: true, Schema: everything()}); err == nil && doc != nil {
		out = doc.Content
	}
	if len(out) == 0 || out[0].Type != "paragraph" && out[0].Type != "heading" {
		out = append([]*Node{newNode("paragraph")}, out...)
	}
	return out
}

// detailsClose recognises an HTML block that ends a toggle, returning what
// came before the </details>.
func detailsClose(raw string) (string, bool) {
	low := strings.ToLower(raw)
	i := strings.Index(low, "</details>")
	if i < 0 {
		return "", false
	}
	if trimSpace(raw[i+len("</details>"):]) != "" {
		return "", false
	}
	return raw[:i], true
}

// rawHTMLBlocks reads a raw HTML block as foreign HTML.
func (r *mdReader) rawHTMLBlocks(raw string) []*Node {
	doc, err := FromHTML(raw, HTMLOptions{Foreign: true, Schema: everything()})
	if err != nil || doc == nil {
		return nil
	}
	if len(doc.Content) == 1 && doc.Content[0].Type == "paragraph" && len(doc.Content[0].Content) == 0 {
		return nil
	}
	return doc.Content
}

func (r *mdReader) block(c gast.Node) []*Node {
	switch n := c.(type) {
	case *gast.Paragraph, *gast.TextBlock:
		if isTocLine(trimSpace(r.linesText(n))) {
			return []*Node{newNode("toc")}
		}
		return r.paragraph(n)
	case *gast.Heading:
		h := newNode("heading")
		h.setAttr("level", float64(n.Level))
		h.Content = r.inlineFlat(n, nil)
		return []*Node{h}
	case *gast.ThematicBreak:
		return []*Node{newNode("horizontalRule")}
	case *gast.CodeBlock:
		return []*Node{codeBlockNode(r.linesText(n), "")}
	case *gast.FencedCodeBlock:
		lang := ""
		if n.Info != nil {
			lang = string(n.Language(r.src))
		}
		body := r.linesText(n)
		if strings.EqualFold(lang, "chart") {
			if spec := trimSpace(body); json.Valid([]byte(spec)) && strings.HasPrefix(spec, "{") {
				e := newNode("chartEmbed")
				e.setAttr("spec", spec)
				return []*Node{e}
			}
		}
		if strings.EqualFold(lang, "math") {
			e := newNode("blockMath")
			if body != "" {
				e.setAttr("latex", body)
			}
			return []*Node{e}
		}
		return []*Node{codeBlockNode(body, lang)}
	case *gast.Blockquote:
		inner := r.blocks(n)
		if callout := asCallout(inner); callout != nil {
			return []*Node{callout}
		}
		q := newNode("blockquote")
		q.Content = inner
		return []*Node{q}
	case *gast.List:
		return r.list(n)
	case *extast.Table:
		return []*Node{r.table(n)}
	}
	return nil
}

func codeBlockNode(body, lang string) *Node {
	cb := newNode("codeBlock")
	if lang = cleanLanguage(lang); lang != "" {
		cb.setAttr("language", lang)
	}
	if body != "" {
		cb.Content = []*Node{{Type: "text", Text: body}}
	}
	return cb
}

// linesText is a code block's text: its lines, without the last line's end.
func (r *mdReader) linesText(n gast.Node) string {
	var b bytes.Buffer
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		s := lines.At(i)
		b.Write(s.Value(r.src))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// paragraph reads a paragraph; an image in it is a block of its own, so it
// splits the paragraph.
func (r *mdReader) paragraph(n gast.Node) []*Node {
	inline := r.inlineFlat(n, nil)
	var out []*Node
	var run []*Node
	flush := func() {
		run = trimRun(run)
		if len(run) == 0 {
			return
		}
		p := newNode("paragraph")
		p.Content = run
		run = nil
		out = append(out, p)
	}
	for _, k := range inline {
		if isInlineNode(k) {
			run = append(run, k)
			continue
		}
		flush()
		out = append(out, k)
	}
	flush()
	if len(out) == 0 {
		return []*Node{newNode("paragraph")}
	}
	return out
}

// trimRun drops the whitespace and line breaks a piece of a paragraph split
// around an image starts and ends with.
func trimRun(run []*Node) []*Node {
	for len(run) > 0 {
		first := run[0]
		if first.Type == "hardBreak" {
			run = run[1:]
			continue
		}
		if first.Type == "text" && !hasMark(first.Marks, "code") {
			t := strings.TrimLeft(first.Text, " \t\n")
			if t == "" {
				run = run[1:]
				continue
			}
			run[0] = &Node{Type: "text", Text: t, Marks: first.Marks}
		}
		break
	}
	for len(run) > 0 {
		last := run[len(run)-1]
		if last.Type == "hardBreak" {
			run = run[:len(run)-1]
			continue
		}
		if last.Type == "text" && !hasMark(last.Marks, "code") {
			t := strings.TrimRight(last.Text, " \t\n")
			if t == "" {
				run = run[:len(run)-1]
				continue
			}
			run[len(run)-1] = &Node{Type: "text", Text: t, Marks: last.Marks}
		}
		break
	}
	return run
}

// asCallout reads a quote that starts with an alert ([!NOTE]) as a callout.
func asCallout(inner []*Node) *Node {
	if len(inner) == 0 || inner[0].Type != "paragraph" || len(inner[0].Content) == 0 {
		return nil
	}
	inner[0].Content = mergeText(inner[0].Content)
	first := inner[0].Content[0]
	if first.Type != "text" || len(first.Marks) > 0 || !strings.HasPrefix(first.Text, "[!") {
		return nil
	}
	end := strings.IndexByte(first.Text, ']')
	if end < 0 {
		return nil
	}
	kind := strings.ToUpper(first.Text[2:end])
	color, ok := alertColor[kind]
	if !ok {
		return nil
	}
	c := newNode("callout")
	c.setAttr("color", color)
	rest := strings.TrimLeft(first.Text[end+1:], " ")
	para := inner[0]
	var content []*Node
	switch {
	case rest != "":
		content = append([]*Node{{Type: "text", Text: rest}}, para.Content[1:]...)
	default:
		content = para.Content[1:]
		for len(content) > 0 && content[0].Type == "hardBreak" {
			content = content[1:]
		}
		if len(content) > 0 && content[0].Type == "text" && len(content[0].Marks) == 0 {
			content[0] = &Node{Type: "text", Text: strings.TrimLeft(content[0].Text, " ")}
		}
	}
	body := inner[1:]
	if len(content) > 0 {
		p := newNode("paragraph")
		p.Content = content
		body = append([]*Node{p}, body...)
	}
	c.Content = body
	return c
}

// mergeText joins neighbouring text nodes with the same marks.
func mergeText(nodes []*Node) []*Node {
	var out []*Node
	// The text after the first of a run of texts with the same marks, joined
	// once when the run ends: joining at each one copied the run so far, and
	// 32,000 "[a" took seconds.
	var run strings.Builder
	flush := func() {
		if run.Len() > 0 {
			first := out[len(out)-1]
			out[len(out)-1] = &Node{Type: "text", Text: first.Text + run.String(), Marks: first.Marks}
			run.Reset()
		}
	}
	for _, n := range nodes {
		if n.Type == "text" && len(out) > 0 {
			prev := out[len(out)-1]
			if prev.Type == "text" && sameMarks(prev.Marks, n.Marks) {
				run.WriteString(n.Text)
				continue
			}
		}
		flush()
		out = append(out, n)
	}
	flush()
	return out
}

var alertColor = map[string]string{
	"NOTE": "blue", "TIP": "green", "IMPORTANT": "purple", "WARNING": "yellow",
	"CAUTION": "red", "REMARK": "gray", "AI": "agent",
}

// list reads a list. Items with checkboxes and items without are separate
// lists: the editor's task list holds only tasks.
func (r *mdReader) list(l *gast.List) []*Node {
	var out []*Node
	var cur *Node
	for c := l.FirstChild(); c != nil; c = c.NextSibling() {
		li, ok := c.(*gast.ListItem)
		if !ok {
			continue
		}
		checked, isTask := taskBox(li)
		kind := "bulletList"
		if l.IsOrdered() {
			kind = "orderedList"
		}
		if isTask {
			kind = "taskList"
		}
		if cur == nil || cur.Type != kind {
			cur = newNode(kind)
			if kind == "orderedList" {
				cur.setAttr("start", float64(l.Start))
			}
			out = append(out, cur)
		}
		item := newNode("listItem")
		if isTask {
			item = newNode("taskItem")
			item.setAttr("checked", checked)
		}
		item.Content = r.blocks(li)
		cur.Content = append(cur.Content, item)
	}
	return out
}

func taskBox(li *gast.ListItem) (checked, ok bool) {
	first := li.FirstChild()
	if first == nil {
		return false, false
	}
	if box, isBox := first.FirstChild().(*extast.TaskCheckBox); isBox && box != nil {
		return box.IsChecked, true
	}
	return false, false
}

func (r *mdReader) table(t *extast.Table) *Node {
	tbl := newNode("table")
	for row := t.FirstChild(); row != nil; row = row.NextSibling() {
		tr := newNode("tableRow")
		_, header := row.(*extast.TableHeader)
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			typ := "tableCell"
			if header {
				typ = "tableHeader"
			}
			td := newNode(typ)
			p := newNode("paragraph")
			p.Content = r.inlineFlat(cell, nil)
			td.Content = []*Node{p}
			tr.Content = append(tr.Content, td)
		}
		tbl.Content = append(tbl.Content, tr)
	}
	return tbl
}

// ---------------------------------------------------------------------------
// Inline content
// ---------------------------------------------------------------------------

// inlineFlat reads a block's inline children as a flat run of text and
// inline nodes, each with its marks. Images come back as image nodes, which
// the caller lifts out.
func (r *mdReader) inlineFlat(parent gast.Node, marks []*Mark) []*Node {
	var out []*Node
	r.inlineInto(&out, parent, marks)
	return out
}

// inlineInto is inlineFlat appending to out: what a mark's span holds goes
// straight into the paragraph's list, not into a list of its own copied up
// through every span around it (emphasis nested a thousand deep copied each
// text a thousand times).
func (r *mdReader) inlineInto(out *[]*Node, parent gast.Node, marks []*Mark) {
	// Marks opened by inline HTML (<u>, <b>, <a href>), a stack for each type,
	// so that a closing tag closes the latest of its own.
	htmlMarks := map[string][]*Mark{}
	cur := func() []*Mark {
		all := append([]*Mark{}, marks...)
		for _, open := range htmlMarks {
			if len(open) > 0 {
				all = withMark(all, open[len(open)-1])
			}
		}
		return all
	}
	for c := parent.FirstChild(); c != nil; c = c.NextSibling() {
		switch n := c.(type) {
		case *gast.Text:
			v := r.textValue(n)
			if v != "" {
				*out = append(*out, &Node{Type: "text", Text: v, Marks: cur()})
			}
			if n.HardLineBreak() {
				*out = append(*out, &Node{Type: "hardBreak", Marks: cur()})
			} else if n.SoftLineBreak() {
				*out = append(*out, &Node{Type: "text", Text: " ", Marks: cur()})
			}
		case *gast.String:
			*out = append(*out, &Node{Type: "text", Text: string(n.Value), Marks: cur()})
		case *mathNode:
			*out = append(*out, &Node{Type: "inlineMath", Attrs: map[string]any{"latex": n.latex}, Marks: cur()})
		case *gast.CodeSpan:
			var b bytes.Buffer
			for k := n.FirstChild(); k != nil; k = k.NextSibling() {
				switch t := k.(type) {
				case *gast.Text:
					v := t.Segment.Value(r.src)
					if bytes.HasSuffix(v, []byte("\n")) {
						b.Write(v[:len(v)-1])
						b.WriteByte(' ')
					} else {
						b.Write(v)
					}
				case *gast.String:
					b.Write(t.Value)
				}
			}
			*out = append(*out, &Node{Type: "text", Text: b.String(), Marks: withMark(cur(), &Mark{Type: "code"})})
		case *gast.Emphasis:
			m := &Mark{Type: "italic"}
			if n.Level >= 2 {
				m = &Mark{Type: "bold"}
			}
			r.inlineInto(out, n, withMark(cur(), m))
		case *extast.Strikethrough:
			r.inlineInto(out, n, withMark(cur(), &Mark{Type: "strike"}))
		case *gast.Link:
			href := unescapeMarkdown(n.Destination)
			if safeHref(href) {
				r.inlineInto(out, n, withMark(cur(), &Mark{Type: "link", Attrs: map[string]any{"href": href}}))
			} else {
				r.inlineInto(out, n, cur())
			}
		case *gast.AutoLink:
			url := string(n.URL(r.src))
			label := string(n.Label(r.src))
			if n.AutoLinkType == gast.AutoLinkEmail && !strings.HasPrefix(strings.ToLower(url), "mailto:") {
				url = "mailto:" + url
			}
			if safeHref(url) {
				*out = append(*out, &Node{Type: "text", Text: label, Marks: withMark(cur(), &Mark{Type: "link", Attrs: map[string]any{"href": url}})})
			} else {
				*out = append(*out, &Node{Type: "text", Text: label, Marks: cur()})
			}
		case *gast.Image:
			img := newNode("image")
			src := unescapeMarkdown(n.Destination)
			img.setAttr("src", src)
			alt := PlainText(&Node{Type: "paragraph", Content: r.inlineFlat(n, nil)})
			if alt != "" {
				img.setAttr("alt", alt)
			}
			if len(n.Title) > 0 {
				img.setAttr("title", unescapeMarkdown(n.Title))
			}
			if safeImageSrc(src, true) {
				*out = append(*out, img)
			} else if alt != "" {
				*out = append(*out, &Node{Type: "text", Text: alt, Marks: cur()})
			}
		case *gast.RawHTML:
			var b bytes.Buffer
			for i := 0; i < n.Segments.Len(); i++ {
				s := n.Segments.At(i)
				b.Write(s.Value(r.src))
			}
			*out = append(*out, r.inlineHTML(b.String(), cur(), htmlMarks)...)
		case *extast.TaskCheckBox:
			// Read by the list.
		default:
			r.inlineInto(out, n, cur())
		}
	}
	return
}

// inlineHTML reads one inline HTML tag: a tag that opens or closes a mark,
// a line break, an image. Any other tag is left out (its text, which is
// Markdown around it, is kept). open is the marks tags have opened, a stack
// for each type.
func (r *mdReader) inlineHTML(raw string, marks []*Mark, open map[string][]*Mark) []*Node {
	t := trimSpace(raw)
	low := strings.ToLower(t)
	if strings.HasPrefix(low, "<!--") {
		return nil
	}
	closing := strings.HasPrefix(low, "</")
	name := low
	name = strings.TrimPrefix(strings.TrimPrefix(name, "</"), "<")
	if i := strings.IndexAny(name, " \t\n/>"); i >= 0 {
		name = name[:i]
	}
	mark := inlineTagMark(name)
	switch {
	case name == "br":
		return []*Node{{Type: "hardBreak", Marks: marks}}
	case name == "img" && !closing:
		doc, err := FromHTML(t, HTMLOptions{Foreign: true, Schema: everything()})
		if err == nil {
			for _, b := range doc.Content {
				if b.Type == "image" {
					return []*Node{b}
				}
			}
		}
		return nil
	case mark != "" && closing:
		if s := open[mark]; len(s) > 0 {
			open[mark] = s[:len(s)-1]
		}
	case mark != "":
		m := &Mark{Type: mark}
		if mark == "link" {
			href := htmlAttr(t, "href")
			if !safeHref(href) {
				return nil
			}
			m.Attrs = map[string]any{"href": href}
		}
		open[mark] = append(open[mark], m)
	}
	return nil
}

// withMark is marks with m added in place of any mark of its type: a text
// carries one mark of a type, the innermost, as Normalize keeps it. Carried
// whole, emphasis nested a thousand deep gave each text in it a thousand
// marks, copied and normalised for every one (seconds for 16 KB). Every mark
// the reader adds is one Normalize keeps, so the innermost is the one it
// would. Pure.
func withMark(marks []*Mark, m *Mark) []*Mark {
	out := make([]*Mark, 0, len(marks)+1)
	for _, x := range marks {
		if x.Type != m.Type {
			out = append(out, x)
		}
	}
	return append(out, m)
}

func inlineTagMark(name string) string {
	switch name {
	case "u":
		return "underline"
	case "mark":
		return "highlight"
	case "b", "strong":
		return "bold"
	case "i", "em":
		return "italic"
	case "s", "del", "strike":
		return "strike"
	case "code", "kbd":
		return "code"
	case "a":
		return "link"
	}
	return ""
}

// htmlAttr reads one attribute of a single HTML tag, its references decoded.
func htmlAttr(tag, name string) string {
	z := html.NewTokenizer(strings.NewReader(tag))
	if tt := z.Next(); tt != html.StartTagToken && tt != html.SelfClosingTagToken {
		return ""
	}
	for {
		key, val, more := z.TagAttr()
		if strings.EqualFold(string(key), name) {
			return string(val)
		}
		if !more {
			return ""
		}
	}
}

// textValue is a text node's text: backslash escapes and character
// references resolved, as CommonMark reads them.
func (r *mdReader) textValue(n *gast.Text) string {
	v := n.Segment.Value(r.src)
	if n.IsRaw() {
		return string(v)
	}
	return unescapeMarkdown(v)
}

// unescapeMarkdown resolves backslash escapes of ASCII punctuation and
// character references (&amp; &#38; &#x26;), as goldmark's HTML writer does.
func unescapeMarkdown(src []byte) string {
	var b strings.Builder
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '\\' && i+1 < len(src) && util.IsPunct(src[i+1]) {
			b.WriteByte(src[i+1])
			i++
			continue
		}
		if c == 0 {
			b.WriteString("\uFFFD")
			continue
		}
		if c == '&' {
			if r, n := readReference(src[i:]); n > 0 {
				b.WriteString(r)
				i += n - 1
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// readReference reads a character reference at the start of src, returning
// what it stands for and how many bytes it took (0: not a reference).
func readReference(src []byte) (string, int) {
	if len(src) < 3 || src[0] != '&' {
		return "", 0
	}
	if src[1] == '#' {
		i := 2
		hex := false
		if i < len(src) && (src[i] == 'x' || src[i] == 'X') {
			hex = true
			i++
		}
		start := i
		for i < len(src) && (hex && isHexDigit(src[i]) || !hex && src[i] >= '0' && src[i] <= '9') {
			i++
		}
		digits := i - start
		if digits == 0 || i >= len(src) || src[i] != ';' {
			return "", 0
		}
		if hex && digits > 6 || !hex && digits > 7 {
			return "", 0
		}
		base := 10
		if hex {
			base = 16
		}
		v, err := strconv.ParseUint(string(src[start:i]), base, 32)
		if err != nil {
			return "", 0
		}
		r := rune(v)
		if v == 0 || v > utf8.MaxRune || (v >= 0xd800 && v <= 0xdfff) {
			r = utf8.RuneError
		}
		return string(r), i + 1
	}
	i := 1
	for i < len(src) && i < 33 && (src[i] >= 'a' && src[i] <= 'z' || src[i] >= 'A' && src[i] <= 'Z' || src[i] >= '0' && src[i] <= '9') {
		i++
	}
	if i == 1 || i >= len(src) || src[i] != ';' {
		return "", 0
	}
	if e, ok := util.LookUpHTML5EntityByName(string(src[1:i])); ok {
		return string(e.Characters), i + 1
	}
	return "", 0
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// ---------------------------------------------------------------------------
// ~~strikethrough~~, two tildes only, as markdown-it reads it
// ---------------------------------------------------------------------------

// dollarMath reads GitHub's inline maths, as the web codec's mathInline
// does: $, a code span on one line, then $ straight after it ($`x^2`$). Its
// TeX is the span's text, a space either side dropped when both are there.
// An escaped \$ never reaches it.
type dollarMath struct{}

func (dollarMath) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithInlineParsers(util.Prioritized(dollarMathParser{}, 90)))
}

var kindMath = gast.NewNodeKind("InlineMath")

type mathNode struct {
	gast.BaseInline
	latex string
}

func (n *mathNode) Kind() gast.NodeKind { return kindMath }

func (n *mathNode) Dump(src []byte, level int) {
	gast.DumpHelper(n, src, level, map[string]string{"latex": n.latex}, nil)
}

type dollarMathParser struct{}

func (dollarMathParser) Trigger() []byte { return []byte{'$'} }

func (dollarMathParser) Parse(parent gast.Node, block text.Reader, pc parser.Context) gast.Node {
	line, _ := block.PeekLine()
	if len(line) < 2 || line[0] != '$' || line[1] != '`' {
		return nil
	}
	p := 1
	for p < len(line) && line[p] == '`' {
		p++
	}
	ticks := p - 1
	for q := p; q < len(line); {
		c := line[q]
		if c == '\n' || c == '\r' {
			return nil
		}
		if c != '`' {
			q++
			continue
		}
		e := q
		for e < len(line) && line[e] == '`' {
			e++
		}
		if e-q == ticks {
			if e >= len(line) || line[e] != '$' {
				return nil
			}
			latex := string(line[p:q])
			if len(latex) > 2 && strings.HasPrefix(latex, " ") && strings.HasSuffix(latex, " ") && strings.TrimLeft(latex, " ") != "" {
				latex = latex[1 : len(latex)-1]
			}
			block.Advance(e + 1)
			return &mathNode{latex: latex}
		}
		q = e
	}
	return nil
}

func (dollarMathParser) CloseBlock(parent gast.Node, pc parser.Context) {}

type doubleTilde struct{}

func (doubleTilde) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithInlineParsers(util.Prioritized(doubleTildeParser{}, 500)))
}

type doubleTildeDelimiter struct{}

func (doubleTildeDelimiter) IsDelimiter(b byte) bool { return b == '~' }
func (doubleTildeDelimiter) CanOpenCloser(opener, closer *parser.Delimiter) bool {
	return opener.Char == closer.Char
}
func (doubleTildeDelimiter) OnMatch(consumes int) gast.Node { return extast.NewStrikethrough() }

type doubleTildeParser struct{}

func (doubleTildeParser) Trigger() []byte { return []byte{'~'} }

func (doubleTildeParser) Parse(parent gast.Node, block text.Reader, pc parser.Context) gast.Node {
	before := block.PrecendingCharacter()
	line, segment := block.PeekLine()
	node := parser.ScanDelimiter(line, before, 2, doubleTildeDelimiter{})
	if node == nil || node.OriginalLength != 2 || before == '~' {
		return nil
	}
	node.Segment = segment.WithStop(segment.Start + node.OriginalLength)
	block.Advance(node.OriginalLength)
	pc.PushDelimiter(node)
	return node
}

func (doubleTildeParser) CloseBlock(parent gast.Node, pc parser.Context) {}
