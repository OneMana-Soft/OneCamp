package contentcodec

import (
	"github.com/akashc777/OneCamp/helpers"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MarkdownOptions says how to write Markdown.
type MarkdownOptions struct {
	// AppURL is the web app's origin. Links to channels, docs, tasks and
	// tables are made under it; empty, they are paths ("/app/doc/…").
	AppURL string
	// Clipboard is the flavour for copying out of a doc into a chat, an
	// email or a ticket: underline, which Markdown has no syntax for, is left
	// out rather than written as <u>.
	Clipboard bool
}

// ToMarkdown writes a doc as GitHub-flavoured Markdown.
//
// everything Markdown has syntax for is written in it: headings h1–h6, marks,
// links, nested lists, task lists, quotes, code blocks, rules, images, tables.
// A callout is a quote that starts with an alert (> [!NOTE]), a toggle is
// <details> with its first line as the <summary>, an underline <u>, a
// highlight <mark>, a table of contents [TOC], a chart its ```chart block (as
// agents write one). Mentions are names (@Sam) or links (#channel, docs,
// tasks), embeds links. What Markdown cannot say is left out: colours, image
// sizes and captions, an item's list style, a quote's size, a block's indent
// and direction; columns are their blocks, in order.
//
// The TypeScript codec writes the same bytes; the fixtures in testdata/codec
// hold both to that.
func ToMarkdown(doc *Node, opts MarkdownOptions) string {
	if doc == nil {
		return ""
	}
	w := &mdWriter{opts: opts}
	var lines []string
	if doc.Type == "doc" {
		lines = w.blocks(doc.Content)
	} else {
		lines = w.blocks([]*Node{doc})
	}
	// No blank lines at either end.
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

type mdWriter struct {
	opts MarkdownOptions
}

func (w *mdWriter) appURL(path string) string {
	return strings.TrimRight(w.opts.AppURL, "/") + path
}

// blocks writes sibling blocks, a blank line between each.
func (w *mdWriter) blocks(nodes []*Node) []string {
	var out []string
	for _, lines := range w.sequence(nodes) {
		if lines == nil {
			continue
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, lines...)
	}
	return out
}

// sequence writes sibling blocks, each to its own lines (nil when it writes
// nothing). Lists of one kind that follow each other take turns between two
// markers ("-" and "*", "." and ")"): with the same marker, Markdown reads
// them as one list.
func (w *mdWriter) sequence(nodes []*Node) [][]string {
	return w.sequenceAfter(nodes, "", false)
}

// sequenceAfter is sequence, continuing from a list of prevKind written with
// the alternate marker or not.
func (w *mdWriter) sequenceAfter(nodes []*Node, prevKind string, prevAlt bool) [][]string {
	out := make([][]string, len(nodes))
	for i, n := range nodes {
		kind := listKind(n.Type)
		alt := kind != "" && kind == prevKind && !prevAlt
		lines := w.block(n, alt)
		out[i] = lines
		if lines == nil {
			continue
		}
		prevKind, prevAlt = kind, alt
	}
	return out
}

func listKind(typ string) string {
	switch typ {
	case "bulletList", "taskList":
		return "bullet"
	case "orderedList":
		return "ordered"
	}
	return ""
}

// block writes one block as lines. nil means the block writes nothing (an
// empty paragraph: Markdown has no way to keep one).
func (w *mdWriter) block(n *Node, alt bool) []string {
	switch n.Type {
	case "paragraph":
		text := w.inline(n.Content, inlineCtx{})
		if blank(text) {
			return nil
		}
		// Markdown drops the spaces a line starts with (and four of them
		// would start a code block).
		lines := strings.Split(text, "\n")
		for i, l := range lines {
			lines[i] = strings.TrimLeft(l, " \t")
		}
		return lines
	case "heading":
		l := clampLevel(n.attrInt("level", 1))
		marker := strings.Repeat("#", l)
		text := w.inline(n.Content, inlineCtx{heading: true})
		if text == "" {
			return []string{marker}
		}
		return []string{marker + " " + text}
	case "blockquote":
		return quote(w.blocks(n.Content), "")
	case "callout":
		return quote(w.blocks(n.Content), "[!"+calloutAlert(n.attrString("color"))+"]")
	case "bulletList", "taskList":
		marker := "-"
		if alt {
			marker = "*"
		}
		return w.list(n, func(int) string { return marker })
	case "orderedList":
		start := n.attrInt("start", 1)
		if start < 0 || start > 999999999 {
			start = 1
		}
		delim := "."
		if alt {
			delim = ")"
		}
		return w.list(n, func(i int) string { return itoa(int64(start+i)) + delim })
	case "codeBlock":
		// A table the editor has no node for was kept as its Markdown in a
		// code block (degrade.go): it is written as the table again, so
		// Markdown that went in comes out the same.
		if lines := keptTable(n); lines != nil {
			return lines
		}
		return fence(codeText(n), n.attrString("language"))
	case "horizontalRule":
		return []string{"---"}
	case "blockMath":
		return fence(n.attrString("latex"), "math")
	case "image":
		return []string{w.image(n)}
	case "bookmark", "webEmbed", "fileBlock":
		// The link it stands for, written as a paragraph holding it is.
		var text string
		if link := mediaLink(n); link != nil {
			text = w.inline([]*Node{link}, inlineCtx{})
		} else {
			text = escapeMarkdownText(n.attrString("name"), true)
		}
		if blank(text) {
			return nil
		}
		lines := strings.Split(text, "\n")
		for i, l := range lines {
			lines[i] = strings.TrimLeft(l, " \t")
		}
		return lines
	case "collapsible":
		// Its first line is the <summary>. Open or closed is each reader's
		// own, so none is written open.
		body, summary := n.Content, ""
		if len(body) > 0 && (body[0].Type == "paragraph" || body[0].Type == "heading") {
			summary, body = w.summaryHTML(body[0]), body[1:]
		}
		out := []string{"<details>", "<summary>" + summary + "</summary>", ""}
		inner := w.blocks(body)
		if len(inner) > 0 {
			out = append(out, inner...)
			out = append(out, "")
		}
		return append(out, "</details>")
	case "table":
		return w.table(n)
	case "toc":
		return []string{"[TOC]"}
	case "tableEmbed":
		id := n.attrString("tableId")
		if id == "" {
			return []string{"Table"}
		}
		return []string{"[Table](" + linkDest(w.appURL("/app/tables/"+id)) + ")"}
	case "poll":
		return []string{"*Poll*"}
	case "subpage", "taskListEmbed", "boardEmbed":
		return w.connectedMarkdown(n)
	case "chartEmbed":
		return fence(n.attrString("spec"), "chart")
	case "diffEmbed":
		return fence(n.attrString("diff"), "diff")
	case "recordingEmbed":
		return []string{"[Call recording](" + linkDest(w.appURL("/app/recordings")) + ")"}
	case "columnList":
		// Markdown has no columns: their blocks, column after column, in order.
		var inner []*Node
		for _, c := range n.Content {
			if c.Type == "column" {
				inner = append(inner, c.Content...)
			} else {
				inner = append(inner, c)
			}
		}
		return w.blocks(inner)
	case "doc":
		return w.blocks(n.Content)
	}
	if isInlineNode(n) {
		text := w.inline([]*Node{n}, inlineCtx{})
		if text == "" {
			return nil
		}
		return strings.Split(text, "\n")
	}
	return w.blocks(n.Content)
}

// summaryHTML is a toggle's first line as its <summary> holds it: HTML, which
// is all a <details> block holds, so Markdown inside it would be read as
// text. It keeps what Markdown keeps (the marks it writes, a link's address,
// a mention as its name or its link) and a heading is <hN>, so a toggle
// heading stays one.
func (w *mdWriter) summaryHTML(line *Node) string {
	var content []*Node
	for _, c := range line.Content {
		marks := w.mdMarks(c)
		linked := func(text, path string) *Node {
			if path != "" && !hasMark(marks, "link") {
				marks = append([]*Mark{{Type: "link", Attrs: map[string]any{"href": w.appURL(path)}}}, marks...)
			}
			return &Node{Type: "text", Text: text, Marks: marks}
		}
		label := helpers.FirstNonEmpty(c.attrString("label"), c.attrString("id"))
		switch c.Type {
		case "text":
			content = append(content, &Node{Type: "text", Text: strings.ReplaceAll(c.Text, "\n", " "), Marks: marks})
		case "hardBreak":
			content = append(content, &Node{Type: "hardBreak", Marks: marks})
		case "mention":
			content = append(content, linked("@"+label, ""))
		case "channelMention":
			path := ""
			if id := strings.TrimPrefix(c.attrString("id"), "channel:"); id != "" {
				path = "/app/channel/" + id
			}
			content = append(content, linked("#"+label, path))
		case "entityMention":
			content = append(content, linked(label, entityHref(c.attrString("id"))))
		default:
			if t := PlainText(c); t != "" {
				content = append(content, &Node{Type: "text", Text: strings.ReplaceAll(t, "\n", " "), Marks: marks})
			}
		}
	}
	// Each mark kept open over neighbours that share it, as the HTML writer does.
	var b strings.Builder
	var active []*Mark
	closeTo := func(keep int) {
		for len(active) > keep {
			b.WriteString(summaryTag(active[len(active)-1], false))
			active = active[:len(active)-1]
		}
	}
	for _, n := range mergeText(content) {
		keep := 0
		for keep < len(active) && keep < len(n.Marks) && active[keep].eq(n.Marks[keep]) {
			keep++
		}
		closeTo(keep)
		for _, m := range n.Marks[keep:] {
			b.WriteString(summaryTag(m, true))
			active = append(active, m)
		}
		if n.Type == "hardBreak" {
			b.WriteString("<br>")
		} else {
			b.WriteString(escapeText(n.Text))
		}
	}
	closeTo(0)
	if line.Type == "heading" {
		tag := "h" + itoa(int64(clampLevel(line.attrInt("level", 1))))
		return "<" + tag + ">" + b.String() + "</" + tag + ">"
	}
	return b.String()
}

// summaryTags are the tags of the marks Markdown writes, in a <summary>.
var summaryTags = map[string]string{
	"bold": "strong", "italic": "em", "strike": "s", "code": "code", "underline": "u", "highlight": "mark", "link": "a",
}

// summaryTag opens or closes a mark in a <summary>: a link by its address
// alone, a highlight without its colour.
func summaryTag(m *Mark, open bool) string {
	tag := summaryTags[m.Type]
	if !open {
		return "</" + tag + ">"
	}
	if m.Type == "link" {
		var b strings.Builder
		b.WriteString("<a")
		writeAttr(&b, "href", m.attrString("href"))
		b.WriteString(">")
		return b.String()
	}
	return "<" + tag + ">"
}

// isSpaceRune is whitespace as both codecs count it: every character either
// Markdown reader (goldmark, markdown-it) or JavaScript's trim treats as
// whitespace, so the two codecs decide alike and never too little.
func isSpaceRune(r rune) bool {
	switch {
	case r == ' ', r == '\t', r == '\n', r == '\v', r == '\f', r == '\r', r == 0x85, r == 0xa0, r == 0x1680,
		r >= 0x2000 && r <= 0x200a, r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000, r == 0xfeff:
		return true
	}
	return false
}

// trimSpace trims the whitespace both codecs count (isSpaceRune).
func trimSpace(s string) string { return strings.TrimFunc(s, isSpaceRune) }

// blank reports whether s holds only whitespace.
func blank(s string) bool {
	return strings.TrimFunc(s, isSpaceRune) == ""
}

func calloutAlert(color string) string {
	switch color {
	case "green":
		return "TIP"
	case "purple":
		return "IMPORTANT"
	case "yellow":
		return "WARNING"
	case "red":
		return "CAUTION"
	case "gray":
		return "REMARK"
	case "agent":
		return "AI"
	}
	return "NOTE"
}

// quote prefixes lines with "> ", a first line (an alert) before them.
func quote(lines []string, first string) []string {
	var out []string
	if first != "" {
		out = append(out, "> "+first)
	}
	for _, l := range lines {
		if l == "" {
			out = append(out, ">")
		} else {
			out = append(out, "> "+l)
		}
	}
	if len(out) == 0 {
		out = []string{">"}
	}
	return out
}

// list writes a list's items, each with its marker; a tight list has no
// blank lines between its items.
func (w *mdWriter) list(n *Node, marker func(i int) string) []string {
	tight := isTightList(n)
	var out []string
	for i, item := range n.Content {
		m := marker(i)
		prefix := m + " "
		if item.Type == "taskItem" {
			if item.attrBool("checked", false) {
				prefix += "[x] "
			} else {
				prefix += "[ ] "
			}
		}
		indent := strings.Repeat(" ", len(m)+1)
		// An empty first paragraph writes nothing: what follows it starts on
		// the marker's line, which reads back as the same item.
		content := item.Content
		// A bulleted list that starts on this item's line takes the other
		// marker, so a run of them never reads as a rule ("- - -").
		prevKind, prevAlt := "", false
		if len(content) > 1 && content[0].Type == "paragraph" && w.block(content[0], false) == nil {
			content = content[1:]
			if m == "-" {
				prevKind = "bullet"
			}
		}
		var body []string
		for k, lines := range w.sequenceAfter(content, prevKind, prevAlt) {
			if lines == nil {
				continue
			}
			if !tight && k > 0 && len(body) > 0 {
				body = append(body, "")
			}
			body = append(body, lines...)
		}
		if len(body) == 0 {
			body = []string{""}
		}
		if !tight && i > 0 {
			out = append(out, "")
		}
		for j, l := range body {
			switch {
			case j == 0:
				out = append(out, strings.TrimRight(prefix+l, " "))
			case l == "":
				out = append(out, "")
			default:
				out = append(out, indent+l)
			}
		}
	}
	return out
}

// isTightList reports whether every item holds one paragraph, then lists:
// such a list is written with no blank lines.
func isTightList(n *Node) bool {
	for _, item := range n.Content {
		for j, c := range item.Content {
			if j == 0 {
				if c.Type != "paragraph" {
					return false
				}
				continue
			}
			switch c.Type {
			case "bulletList", "orderedList", "taskList":
			default:
				return false
			}
		}
	}
	return true
}

// fence writes a fenced code block, its fence longer than any run of
// backticks in the code.
func fence(text, lang string) []string {
	longest := 0
	run := 0
	for _, c := range text {
		if c == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", max(3, longest+1))
	lang = cleanLanguage(lang)
	out := []string{f + lang}
	if text != "" {
		out = append(out, strings.Split(text, "\n")...)
	}
	return append(out, f)
}

var languageChars = regexp.MustCompile(`[^A-Za-z0-9_+#.-]`)

func cleanLanguage(lang string) string {
	return languageChars.ReplaceAllString(lang, "")
}

func codeText(n *Node) string {
	var b strings.Builder
	for _, c := range n.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

func (w *mdWriter) image(n *Node) string {
	alt := escapeLinkText(n.attrString("alt"))
	src := n.attrString("src")
	title := n.attrString("title")
	out := "![" + alt + "](" + linkDest(src)
	if title != "" {
		out += ` "` + escapeEntityAmp(strings.ReplaceAll(strings.ReplaceAll(title, `\`, `\\`), `"`, `\"`)) + `"`
	}
	return out + ")"
}

// table writes a GitHub table: the first row is its header.
func (w *mdWriter) table(n *Node) []string {
	var rows [][]string
	cols := 0
	for _, r := range n.Content {
		var cells []string
		for _, c := range r.Content {
			var parts []string
			for _, b := range c.Content {
				var text string
				if b.Type == "paragraph" || b.Type == "heading" {
					text = w.inline(b.Content, inlineCtx{cell: true})
				} else {
					text = escapeCell(escapeMarkdownText(PlainText(b), false))
				}
				if text != "" {
					parts = append(parts, text)
				}
			}
			cells = append(cells, strings.Join(parts, "<br>"))
		}
		if len(cells) > cols {
			cols = len(cells)
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 || cols == 0 {
		return nil
	}
	line := func(cells []string) string {
		for len(cells) < cols {
			cells = append(cells, "")
		}
		var b strings.Builder
		b.WriteString("|")
		for _, c := range cells {
			b.WriteString(" " + c + " |")
		}
		return strings.ReplaceAll(b.String(), "  |", " |")
	}
	out := []string{line(rows[0])}
	sep := make([]string, cols)
	for i := range sep {
		sep[i] = "---"
	}
	out = append(out, line(sep))
	for _, r := range rows[1:] {
		out = append(out, line(r))
	}
	return out
}

func escapeCell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

// ---------------------------------------------------------------------------
// Inline content
// ---------------------------------------------------------------------------

type inlineCtx struct {
	heading bool // a hard break is <br>: a heading is one line
	cell    bool // the same, and | is escaped
}

func (w *mdWriter) mdMarks(n *Node) []*Mark {
	var out []*Mark
	for _, m := range n.Marks {
		switch m.Type {
		case "bold", "italic", "strike", "code", "link":
			out = append(out, m)
		case "underline", "highlight":
			if !w.opts.Clipboard {
				out = append(out, m)
			}
		}
	}
	return out
}

func markDelims(m *Mark) (string, string) {
	switch m.Type {
	case "bold":
		return "**", "**"
	case "italic":
		return "*", "*"
	case "strike":
		return "~~", "~~"
	case "underline":
		return "<u>", "</u>"
	case "highlight":
		// Its colour is left out, as every colour is.
		return "<mark>", "</mark>"
	case "link":
		return "[", "](" + linkDest(m.attrString("href")) + ")"
	}
	return "", ""
}

// An inline run is written as tokens first (text, and marks opening and
// closing), so each emphasis can then be checked against what ends up on
// either side of it.
type mdTok struct {
	open, close bool
	span        *mdSpan
	text        string
	// plain is text of the doc's own (not a code span or an equation).
	plain bool
}

// escapeMathEnds escapes the $ that text starts with straight after a code
// span: it would read back as the end of an equation ($`x`$).
func escapeMathEnds(toks []*mdTok) {
	for i, t := range toks {
		if !t.plain || !strings.HasPrefix(t.text, "$") {
			continue
		}
		j := i - 1
		for j >= 0 && toks[j].render() == "" {
			j--
		}
		if j >= 0 && strings.HasSuffix(toks[j].render(), "`") {
			t.text = "\\" + t.text
		}
	}
}

// mdSpan is one mark's stretch of text.
type mdSpan struct {
	mark *Mark
	// html writes it as an HTML tag: Markdown's ** * ~~ need the right
	// characters on either side (CommonMark's "flanking" rules), and
	// **!**word is not bold. <strong>!</strong>word is.
	html bool
}

func (t *mdTok) render() string {
	if !t.open && !t.close {
		return t.text
	}
	op, cl := markDelims(t.span.mark)
	if t.span.html {
		op, cl = htmlDelims(t.span.mark)
	}
	if t.open {
		return op
	}
	return cl
}

// inline writes inline content. Marks are opened in the order that keeps
// each open longest, and whitespace at their edges is moved outside them,
// where Markdown needs it to be.
func (w *mdWriter) inline(in []*Node, ctx inlineCtx) string {
	// A link mention is written as the link it stands for.
	nodes := make([]*Node, len(in))
	for i, c := range in {
		nodes[i] = c
		if c != nil && c.Type == "linkMention" {
			if nodes[i] = mediaLink(c); nodes[i] == nil {
				nodes[i] = &Node{Type: "text", Text: helpers.FirstNonEmpty(c.attrString("title"), c.attrString("href")), Marks: c.Marks}
			}
		}
	}
	// Trailing and leading hard breaks write nothing Markdown keeps.
	start, end := 0, len(nodes)
	for start < end && nodes[start].Type == "hardBreak" {
		start++
	}
	for end > start && nodes[end-1].Type == "hardBreak" {
		end--
	}
	nodes = nodes[start:end]

	var toks []*mdTok
	type openSpan struct {
		mark *Mark
		span *mdSpan
	}
	var active []openSpan
	atLineStart := !ctx.heading && !ctx.cell
	text := func(s string) {
		if s == "" {
			return
		}
		toks = append(toks, &mdTok{text: s})
		if strings.TrimLeft(s, " \t") != "" {
			atLineStart = false
		}
	}
	closeTo := func(keep int) {
		for len(active) > keep {
			a := active[len(active)-1]
			toks = append(toks, &mdTok{close: true, span: a.span})
			active = active[:len(active)-1]
		}
	}
	activeHas := func(marks []*Mark, upto int) func(*Mark) bool {
		return func(m *Mark) bool {
			for _, a := range active[:upto] {
				if a.mark.eq(m) {
					return true
				}
			}
			return false
		}
	}
	for i := 0; i < len(nodes); i++ {
		n := nodes[i]
		marks := w.mdMarks(n)
		if hasMark(marks, "code") && n.Type == "text" {
			marks = []*Mark{findMark(marks, "code")}
		}

		// An autolink: a link whose text is its address.
		if n.Type == "text" && len(marks) == 1 && marks[0].Type == "link" && isAutolinkable(n.Text, marks[0].attrString("href")) &&
			!(i+1 < len(nodes) && hasMarkEq(w.mdMarks(nodes[i+1]), marks[0])) &&
			!(i > 0 && hasMarkEq(w.mdMarks(nodes[i-1]), marks[0])) {
			closeTo(0)
			text("<" + n.Text + ">")
			continue
		}

		body := ""
		lead, trail := "", ""
		if n.Type == "text" {
			body = n.Text
			if !hasMark(marks, "code") {
				body = strings.ReplaceAll(body, "\n", " ")
			}
		}

		// Keep open the active marks this node still has, in their order.
		keep := 0
		for keep < len(active) && hasMarkEq(marks, active[keep].mark) {
			keep++
		}
		// Marks this node adds, longest-running first.
		kept := activeHas(marks, keep)
		var adds []*Mark
		for _, m := range marks {
			if !kept(m) {
				adds = append(adds, m)
			}
		}
		sort.SliceStable(adds, func(a, c int) bool {
			ra, rc := runLength(w, nodes, i, adds[a]), runLength(w, nodes, i, adds[c])
			if ra != rc {
				return ra > rc
			}
			return markSpecs[adds[a].Type].rank < markSpecs[adds[c].Type].rank
		})
		// Whitespace at the edge of a mark goes outside it.
		if n.Type == "text" && !hasMark(marks, "code") {
			if len(adds) > 0 || keep < len(active) {
				trimmed := strings.TrimLeftFunc(body, isSpaceRune)
				lead = body[:len(body)-len(trimmed)]
				body = trimmed
			}
			closing := i+1 >= len(nodes)
			if !closing {
				next := w.mdMarks(nodes[i+1])
				for _, m := range marks {
					if !hasMarkEq(next, m) {
						closing = true
						break
					}
				}
			}
			if closing && len(marks) > 0 {
				trimmed := strings.TrimRightFunc(body, isSpaceRune)
				trail = body[len(trimmed):]
				body = trimmed
			}
		}
		closeTo(keep)
		if lead != "" {
			text(lead)
		}
		if n.Type == "text" && body == "" && !hasMark(marks, "code") {
			// Only whitespace: nothing to mark.
			if trail != "" {
				text(trail)
			}
			continue
		}
		for _, m := range adds {
			if m.Type == "code" {
				continue
			}
			sp := &mdSpan{mark: m}
			toks = append(toks, &mdTok{open: true, span: sp})
			active = append(active, openSpan{mark: m, span: sp})
		}
		switch n.Type {
		case "text":
			if hasMark(marks, "code") {
				text(codeSpan(body))
			} else {
				esc := escapeMarkdownText(body, atLineStart)
				if ctx.cell {
					esc = escapeCell(esc)
				}
				text(esc)
				if esc != "" {
					toks[len(toks)-1].plain = true
				}
			}
		case "dateMention":
			text(escapeMarkdownText(dateWords(n.attr("date")), atLineStart))
		case "inlineMath":
			// GitHub's inline maths: a code span between dollars, on one line.
			if latex := strings.ReplaceAll(n.attrString("latex"), "\n", " "); latex != "" {
				text("$" + codeSpan(latex) + "$")
			}
		case "hardBreak":
			if ctx.heading || ctx.cell {
				text("<br>")
			} else {
				toks = append(toks, &mdTok{text: "\\\n"})
				atLineStart = true
			}
		case "mention":
			text(escapeMarkdownText("@"+helpers.FirstNonEmpty(n.attrString("label"), n.attrString("id")), atLineStart))
		case "channelMention":
			label := "#" + helpers.FirstNonEmpty(n.attrString("label"), n.attrString("id"))
			id := strings.TrimPrefix(n.attrString("id"), "channel:")
			if id == "" || hasMark(marks, "link") {
				text(escapeMarkdownText(label, atLineStart))
			} else {
				text("[" + escapeLinkText(label) + "](" + linkDest(w.appURL("/app/channel/"+id)) + ")")
			}
		case "entityMention":
			label := helpers.FirstNonEmpty(n.attrString("label"), n.attrString("id"))
			href := entityHref(n.attrString("id"))
			if href == "" || hasMark(marks, "link") {
				text(escapeMarkdownText(label, atLineStart))
			} else {
				text("[" + escapeLinkText(label) + "](" + linkDest(w.appURL(href)) + ")")
			}
		}
		if trail != "" {
			// Close what ends here before the whitespace.
			nextMarks := []*Mark(nil)
			if i+1 < len(nodes) {
				nextMarks = w.mdMarks(nodes[i+1])
			}
			k := 0
			for k < len(active) && hasMarkEq(nextMarks, active[k].mark) {
				k++
			}
			closeTo(k)
			text(trail)
		}
	}
	closeTo(0)
	escapeMathEnds(toks)
	chooseEmphasis(toks)
	var b strings.Builder
	for _, t := range toks {
		b.WriteString(t.render())
	}
	out := b.String()
	if ctx.heading && strings.HasSuffix(out, "#") {
		// A heading's closing #s would be read as part of its syntax.
		out = out[:len(out)-1] + `\#`
	}
	return out
}

func htmlDelims(m *Mark) (string, string) {
	switch m.Type {
	case "bold":
		return "<strong>", "</strong>"
	case "italic":
		return "<em>", "</em>"
	case "strike":
		return "<s>", "</s>"
	}
	return markDelims(m)
}

// delimChar is the character an emphasis is written with, 0 for marks that
// are not emphasis (links, underline, code).
func delimChar(m *Mark) byte {
	switch m.Type {
	case "bold", "italic":
		return '*'
	case "strike":
		return '~'
	}
	return 0
}

// chooseEmphasis writes as HTML each emphasis whose ** * ~~ would not be read
// as one: its opening run must be left-flanking and its closing run
// right-flanking, given the characters around them once written.
func chooseEmphasis(toks []*mdTok) {
	// The characters around a run of delimiters of one character: runs of the
	// same character read as one (*** is bold and italic at once).
	around := func(i int, c byte) (before, after rune) {
		before, after = '\n', '\n'
		j := i
		for j >= 0 && (toks[j].open || toks[j].close) && delimChar(toks[j].span.mark) == c && !toks[j].span.html {
			j--
		}
		for ; j >= 0; j-- {
			if s := toks[j].render(); s != "" {
				r, _ := utf8.DecodeLastRuneInString(s)
				before = r
				break
			}
		}
		k := i
		for k < len(toks) && (toks[k].open || toks[k].close) && delimChar(toks[k].span.mark) == c && !toks[k].span.html {
			k++
		}
		for ; k < len(toks); k++ {
			if s := toks[k].render(); s != "" {
				r, _ := utf8.DecodeRuneInString(s)
				after = r
				break
			}
		}
		return before, after
	}
	for i, t := range toks {
		if !t.open && !t.close {
			continue
		}
		c := delimChar(t.span.mark)
		if c == 0 || t.span.html {
			continue
		}
		before, after := around(i, c)
		left, right := flanking(before, after)
		if t.open && !left || t.close && !right {
			t.span.html = true
		}
	}
}

// flanking is CommonMark's: whether a delimiter run between before and after
// is left-flanking (can open) and right-flanking (can close).
func flanking(before, after rune) (left, right bool) {
	bs, as := mdSpace(before), mdSpace(after)
	bp, ap := mdPunct(before), mdPunct(after)
	left = !as && (!ap || bs || bp)
	right = !bs && (!bp || as || ap)
	return left, right
}

func mdSpace(r rune) bool { return isSpaceRune(r) || unicode.Is(unicode.Zs, r) }

func mdPunct(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) }

func hasMark(marks []*Mark, typ string) bool { return findMark(marks, typ) != nil }

func findMark(marks []*Mark, typ string) *Mark {
	for _, m := range marks {
		if m.Type == typ {
			return m
		}
	}
	return nil
}

func hasMarkEq(marks []*Mark, m *Mark) bool {
	for _, o := range marks {
		if o.eq(m) {
			return true
		}
	}
	return false
}

// runLength is how many nodes from i on carry mark m.
func runLength(w *mdWriter, nodes []*Node, i int, m *Mark) int {
	n := 0
	for j := i; j < len(nodes); j++ {
		if !hasMarkEq(w.mdMarks(nodes[j]), m) {
			break
		}
		n++
	}
	return n
}

// entityHref is the app path a +reference opens: "doc:<id>" is /app/doc/<id>.
func entityHref(id string) string {
	typ, rest, ok := strings.Cut(id, ":")
	if !ok || rest == "" {
		return ""
	}
	switch typ {
	case "doc", "board", "task", "project", "channel":
		return "/app/" + typ + "/" + rest
	}
	return ""
}

func isAutolinkable(text, href string) bool {
	if text != href || href == "" {
		return false
	}
	low := strings.ToLower(href)
	if !(strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") || strings.HasPrefix(low, "mailto:")) {
		return false
	}
	for i := 0; i < len(href); i++ {
		if c := href[i]; c <= 0x20 || c == 0x7f || c == '<' || c == '>' {
			return false
		}
	}
	return safeHref(href)
}

// codeSpan writes code, fenced by more backticks than it holds.
func codeSpan(text string) string {
	longest, run := 0, 0
	for _, c := range text {
		if c == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", longest+1)
	pad := strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") ||
		(strings.HasPrefix(text, " ") && strings.HasSuffix(text, " ") && strings.Trim(text, " ") != "")
	if pad {
		return f + " " + text + " " + f
	}
	return f + text + f
}

// linkDest writes a link's destination, in <> when it holds what a bare one
// cannot.
func linkDest(href string) string {
	needsAngle := false
	depth := 0
	for _, c := range href {
		switch {
		case c == ' ' || c == '\t' || c == '<' || c == '>' || c < 0x20:
			needsAngle = true
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth < 0 {
				needsAngle = true
			}
		}
	}
	if depth != 0 {
		needsAngle = true
	}
	if href == "" {
		return "<>"
	}
	esc := escapeEntityAmp(strings.ReplaceAll(href, `\`, `\\`))
	if needsAngle {
		esc = strings.NewReplacer("<", `\<`, ">", `\>`, "\n", " ").Replace(esc)
		return "<" + esc + ">"
	}
	return esc
}

// escapeEntityAmp escapes an & that would be read as a character reference.
func escapeEntityAmp(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '&' && entityLike.MatchString(s[i:]) {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func escapeLinkText(s string) string {
	return strings.NewReplacer(`\`, `\\`, "[", `\[`, "]", `\]`, "`", "\\`", "*", `\*`, "_", `\_`, "~", `\~`, "<", `\<`).Replace(s)
}

var (
	entityLike   = regexp.MustCompile(`^&(#[0-9]{1,7};|#[xX][0-9a-fA-F]{1,6};|[A-Za-z][A-Za-z0-9]{1,31};)`)
	atxStart     = regexp.MustCompile(`^(#{1,6})([ \t]|$)`)
	orderedStart = regexp.MustCompile(`^(\d{1,9})([.)])([ \t]|$)`)
	setextLine   = regexp.MustCompile(`^=+[ \t]*$`)
)

// isAlnumByte is an ASCII letter or digit: an underscore between two of them
// can't open or close emphasis, so it needs no escape.
func isAlnumByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// escapeMarkdownText escapes text so Markdown reads it back as the same
// text. atLineStart: the text starts a line, where #, >, -, +, = and "1."
// would start a block.
func escapeMarkdownText(s string, atLineStart bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\', '`', '*', '~', '[', ']':
			b.WriteByte('\\')
		case '_':
			if !(i > 0 && i+1 < len(s) && isAlnumByte(s[i-1]) && isAlnumByte(s[i+1])) {
				b.WriteByte('\\')
			}
		case '<':
			if i+1 < len(s) {
				n := s[i+1]
				if n >= 'a' && n <= 'z' || n >= 'A' && n <= 'Z' || n == '/' || n == '!' || n == '?' {
					b.WriteByte('\\')
				}
			}
		case '&':
			if entityLike.MatchString(s[i:]) {
				b.WriteByte('\\')
			}
		}
		b.WriteByte(c)
	}
	out := b.String()
	if atLineStart {
		out = escapeLineStart(out)
	}
	return out
}

// escapeLineStart escapes what would start a block at the start of a line.
func escapeLineStart(s string) string {
	t := strings.TrimLeft(s, " \t")
	lead := s[:len(s)-len(t)]
	switch {
	case t == "":
		return s
	case atxStart.MatchString(t):
		return lead + `\` + t
	case strings.HasPrefix(t, "-") || strings.HasPrefix(t, ">"):
		return lead + `\` + t
	case strings.HasPrefix(t, "+ ") || t == "+" || strings.HasPrefix(t, "+\t"):
		return lead + `\` + t
	case setextLine.MatchString(t):
		return lead + `\` + t
	}
	if m := orderedStart.FindStringSubmatchIndex(t); m != nil {
		return lead + t[:m[3]] + `\` + t[m[4]:]
	}
	return s
}

// keptTable is the table a code block holds when degrade kept it there: a
// Markdown code block that is exactly one table, written as the codec
// writes tables. Any other code block, a table someone wrote by hand among
// them, stays a code block.
func keptTable(n *Node) []string {
	text := codeText(n)
	if n.attrString("language") != "markdown" || !strings.HasPrefix(text, "|") {
		return nil
	}
	doc, err := FromMarkdown(text, everything())
	if err != nil || doc == nil || len(doc.Content) != 1 || doc.Content[0].Type != "table" {
		return nil
	}
	lines := (&mdWriter{}).table(doc.Content[0])
	if strings.Join(lines, "\n") != text {
		return nil
	}
	return lines
}
