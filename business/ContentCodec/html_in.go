package contentcodec

import (
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// HTMLOptions says how to read HTML.
type HTMLOptions struct {
	// Foreign is HTML from outside the editor: an imported file, another
	// tool's export. It is read as untrusted: classes, and colours other
	// than ours by name (data-color), are left out, and only safe links and
	// images are kept. The editor's own HTML (a stored body, the codec's
	// output) keeps every attribute the schema has.
	Foreign bool
	// Assets is foreign HTML that comes with its own files (an import): a
	// file block may name one by a relative path, as an image may, for the
	// import to upload and point at its upload.
	Assets bool
	// Schema is what the doc may hold; nil is the shipped schema.
	Schema *Schema
}

// FromHTML reads HTML as a doc, in canonical form.
//
// It reads the way ProseMirror's DOM parser does with the editor's parse
// rules: block elements the schema has no node for are boundaries between
// paragraphs, inline ones are passed through, whitespace outside <pre> is
// collapsed, and scripts, styles, frames and form controls are dropped with
// everything inside them.
func FromHTML(src string, opts HTMLOptions) (*Node, error) {
	nodes, err := parseBody(src)
	if err != nil {
		return nil, err
	}
	return Normalize(readNodesWith(nodes, &htmlParser{foreign: opts.Foreign, assets: opts.Foreign && opts.Assets}), opts.Schema), nil
}

// parseBody parses HTML as what a <body> holds: how FromHTML and Blocks
// (blocks.go) both start.
func parseBody(src string) ([]*html.Node, error) {
	body := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	return html.ParseFragment(strings.NewReader(strings.ToValidUTF8(src, "\uFFFD")), body)
}

// readNodes reads parsed HTML as a doc's nodes, not yet normalised.
func readNodes(nodes []*html.Node, foreign bool) *Node {
	return readNodesWith(nodes, &htmlParser{foreign: foreign})
}

func readNodesWith(nodes []*html.Node, p *htmlParser) *Node {
	root := &pnode{node: newNode("doc"), spec: specOf("doc")}
	p.stack = []*pnode{root}
	for _, n := range nodes {
		p.add(n)
	}
	for len(p.stack) > 1 {
		p.pop()
	}
	return root.node
}

type pnode struct {
	node     *Node
	spec     *nodeSpec
	preserve bool // inside <pre>: whitespace is kept
	implicit bool // a paragraph opened for loose inline content
	loose    bool // a column opened for blocks loose in a row
}

type htmlParser struct {
	stack   []*pnode
	marks   []*Mark
	foreign bool
	assets  bool // HTMLOptions.Assets
}

func (p *htmlParser) top() *pnode { return p.stack[len(p.stack)-1] }

// pop closes the innermost open node and adds it to its parent.
func (p *htmlParser) pop() {
	t := p.top()
	p.stack = p.stack[:len(p.stack)-1]
	if t.spec != nil && t.spec.content == holdsInline && !t.preserve {
		stripTrailingSpace(t.node)
	}
	parent := p.top()
	parent.node.Content = append(parent.node.Content, t.node)
}

func stripTrailingSpace(n *Node) {
	if len(n.Content) == 0 {
		return
	}
	last := n.Content[len(n.Content)-1]
	if last.Type != "text" {
		return
	}
	trimmed := strings.TrimRight(last.Text, " \t\r\n\f")
	if trimmed == "" {
		n.Content = n.Content[:len(n.Content)-1]
		return
	}
	last.Text = trimmed
}

// inInline reports whether the innermost open node holds inline content.
func (p *htmlParser) inInline() bool {
	t := p.top()
	return t.spec != nil && (t.spec.content == holdsInline || t.spec.content == holdsText)
}

// closeInline closes an open paragraph, heading or code block, so a block can
// follow.
func (p *htmlParser) closeInline() {
	for len(p.stack) > 1 && p.inInline() {
		p.pop()
	}
}

// openImplicitParagraph makes room for inline content in a block container.
func (p *htmlParser) ensureInline() {
	if p.inInline() {
		return
	}
	t := p.top()
	if t.spec != nil && (t.spec.content == holdsListItems || t.spec.content == holdsTaskItems) {
		item := "listItem"
		if t.spec.content == holdsTaskItems {
			item = "taskItem"
		}
		p.push(newNode(item), false)
	}
	if t := p.top(); t.spec != nil && (t.spec.content == holdsRows) {
		p.push(newNode("tableRow"), false)
	}
	if t := p.top(); t.spec != nil && (t.spec.content == holdsCells) {
		p.push(newNode("tableCell"), false)
	}
	if t := p.top(); t.spec != nil && (t.spec.content == holdsColumns) {
		p.push(newNode("column"), false).loose = true
	}
	par := &pnode{node: newNode("paragraph"), spec: specOf("paragraph"), implicit: true}
	p.stack = append(p.stack, par)
}

func (p *htmlParser) push(n *Node, preserve bool) *pnode {
	pn := &pnode{node: n, spec: specOf(n.Type), preserve: preserve}
	p.stack = append(p.stack, pn)
	return pn
}

// openBlock opens a block node where the innermost open node can hold it.
func (p *htmlParser) openBlock(n *Node, preserve bool) {
	p.closeInline()
	// A column after blocks loose in its row: theirs closes, and it stands beside it.
	if n.Type == "column" {
		for len(p.stack) > 1 && p.top().loose {
			p.pop()
		}
	}
	t := p.top()
	if t.spec != nil {
		switch t.spec.content {
		case holdsListItems, holdsTaskItems:
			if n.Type != "listItem" && n.Type != "taskItem" {
				item := "listItem"
				if t.spec.content == holdsTaskItems {
					item = "taskItem"
				}
				p.push(newNode(item), false)
			}
		case holdsRows:
			if n.Type != "tableRow" {
				p.push(newNode("tableRow"), false)
			}
		case holdsColumns:
			if n.Type != "column" {
				p.push(newNode("column"), false).loose = true
			}
		}
	}
	if t := p.top(); t.spec != nil && t.spec.content == holdsCells && n.Type != "tableCell" && n.Type != "tableHeader" {
		p.push(newNode("tableCell"), false)
	}
	if t := p.top(); t.spec != nil && t.spec.content == holdsItemBody && len(t.node.Content) == 0 && n.Type != "paragraph" {
		t.node.Content = append(t.node.Content, newNode("paragraph"))
	}
	p.push(n, preserve)
}

// closeTo closes open nodes down to (and including) the given entry.
func (p *htmlParser) closeTo(pn *pnode) {
	for len(p.stack) > 1 {
		t := p.top()
		p.pop()
		if t == pn {
			return
		}
	}
}

// blockTags are the elements ProseMirror's parser treats as block boundaries
// when no rule names them.
var blockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true, "canvas": true, "dd": true,
	"div": true, "dl": true, "dt": true, "fieldset": true, "figcaption": true, "figure": true, "footer": true,
	"form": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "header": true,
	"hgroup": true, "hr": true, "li": true, "main": true, "nav": true, "noscript": true, "ol": true,
	"output": true, "p": true, "pre": true, "section": true, "table": true, "tfoot": true, "ul": true,
	"center": true, "summary": true, "details": true,
}

// ignoredTags are dropped with everything inside them.
var ignoredTags = map[string]bool{
	"head": true, "script": true, "style": true, "title": true, "template": true, "noscript": true,
	"iframe": true, "frame": true, "frameset": true, "object": true, "embed": true, "applet": true,
	"svg": true, "math": true, "canvas": true, "audio": true, "video": true, "source": true, "track": true,
	"select": true, "option": true, "optgroup": true, "textarea": true, "button": true, "input": true,
	"meta": true, "link": true, "base": true, "colgroup": true, "col": true, "map": true, "area": true,
	"picture": true, "dialog": true, "slot": true,
}

func attrOf(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if a.Namespace == "" && strings.EqualFold(a.Key, name) {
			return a.Val, true
		}
	}
	return "", false
}

func attrVal(n *html.Node, name string) string {
	v, _ := attrOf(n, name)
	return v
}

// styleOf is an element's inline style as property → value, lower-cased.
func styleOf(n *html.Node) map[string]string {
	raw, ok := attrOf(n, "style")
	if !ok {
		return nil
	}
	out := map[string]string{}
	for _, decl := range strings.Split(raw, ";") {
		k, v, ok := strings.Cut(decl, ":")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		v = strings.TrimSuffix(strings.TrimSuffix(v, "!important"), " ")
		out[k] = strings.TrimSpace(v)
	}
	return out
}

var boldWeight = regexp.MustCompile(`^(bold(er)?|[5-9]\d{2,})$`)

func (p *htmlParser) add(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		p.addText(n.Data)
		return
	case html.ElementNode:
	default:
		return
	}
	tag := strings.ToLower(n.Data)
	if n.Namespace != "" || ignoredTags[tag] {
		return
	}
	style := styleOf(n)
	if tag == "a" && attrVal(n, "data-type") == "link-mention" {
		if m := readLinkMention(n, p.foreign); m != nil {
			p.addInline(m)
			return
		}
	}

	// Marks the element carries, from its tag or its style.
	pushed := 0
	pushMark := func(m *Mark) {
		p.marks = append(p.marks, m)
		pushed++
	}
	clearBold := false
	switch tag {
	case "strong":
		pushMark(&Mark{Type: "bold"})
	case "b":
		if fw := style["font-weight"]; fw != "normal" && fw != "400" {
			pushMark(&Mark{Type: "bold"})
		}
	case "em":
		pushMark(&Mark{Type: "italic"})
	case "i":
		if style["font-style"] != "normal" {
			pushMark(&Mark{Type: "italic"})
		}
	case "s", "del", "strike":
		pushMark(&Mark{Type: "strike"})
	case "u":
		pushMark(&Mark{Type: "underline"})
	case "code", "kbd", "samp", "tt":
		pushMark(&Mark{Type: "code"})
	case "mark":
		// A highlight by colour name, yellow when it names none of ours (B2).
		pushMark(&Mark{Type: "highlight", Attrs: map[string]any{"color": highlightColour(attrVal(n, "data-color"))}})
	case "a":
		if href, ok := attrOf(n, "href"); ok && safeHref(href) {
			m := &Mark{Type: "link", Attrs: map[string]any{"href": href}}
			if !p.foreign {
				for _, a := range []string{"target", "rel"} {
					if v, ok := attrOf(n, a); ok {
						m.Attrs[a] = v
					}
				}
				if c := contentClasses(attrVal(n, "class")); c != "" {
					m.Attrs["class"] = c
				}
			}
			pushMark(m)
		}
	}
	if tag != "b" && tag != "strong" {
		if fw, ok := style["font-weight"]; ok {
			if boldWeight.MatchString(fw) {
				pushMark(&Mark{Type: "bold"})
			} else if fw == "400" || fw == "normal" {
				clearBold = true
			}
		}
	}
	if fs := style["font-style"]; fs == "italic" && tag != "em" && tag != "i" {
		pushMark(&Mark{Type: "italic"})
	}
	deco := style["text-decoration"] + " " + style["text-decoration-line"]
	if strings.Contains(deco, "line-through") && tag != "s" && tag != "del" && tag != "strike" {
		pushMark(&Mark{Type: "strike"})
	}
	// A link's underline is how another tool drew links, not an underline.
	if strings.Contains(deco, "underline") && tag != "u" && !(p.foreign && (tag == "a" || p.inLink())) {
		pushMark(&Mark{Type: "underline"})
	}
	// A text colour is one of ours by name (B2, N15): data-color, or an old
	// doc's style="color: var(--camp-sky-ink)". Any other is none.
	if tag == "span" {
		c := colourName(attrVal(n, "data-color"))
		if c == "" && !p.foreign {
			c = colourName(style["color"])
		}
		if c != "" {
			pushMark(&Mark{Type: "textStyle", Attrs: map[string]any{"color": c}})
		}
	}
	var saved []*Mark
	if clearBold {
		saved = p.marks
		var kept []*Mark
		for _, m := range p.marks {
			if m.Type != "bold" {
				kept = append(kept, m)
			}
		}
		p.marks = kept
	}
	defer func() {
		if clearBold {
			p.marks = saved
		}
		p.marks = p.marks[:len(p.marks)-pushed]
	}()

	if p.addNode(n, tag) {
		return
	}
	// No node for this element: its content, with a block boundary around it
	// when it is a block element.
	if blockTags[tag] {
		p.closeImplicit()
		p.addChildren(n)
		p.closeImplicit()
		return
	}
	p.addChildren(n)
}

func (p *htmlParser) addChildren(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		p.add(c)
	}
}

// closeImplicit closes a paragraph the parser opened for loose text, so the
// next content starts a new block.
func (p *htmlParser) closeImplicit() {
	for len(p.stack) > 1 && p.top().implicit {
		p.pop()
	}
}

func (p *htmlParser) inLink() bool {
	for _, m := range p.marks {
		if m.Type == "link" {
			return true
		}
	}
	return false
}

func (p *htmlParser) currentMarks() []*Mark {
	if len(p.marks) == 0 {
		return nil
	}
	out := make([]*Mark, len(p.marks))
	copy(out, p.marks)
	return out
}

// addText adds a text node's text where it belongs. Text inside <pre> never
// comes here: a code block reads its element's text whole (preText).
func (p *htmlParser) addText(text string) {
	value := collapseSpace(text)
	if !p.inInline() {
		if strings.Trim(value, " ") == "" {
			return
		}
		p.ensureInline()
	}
	t := p.top()
	if strings.HasPrefix(value, " ") {
		var before *Node
		if len(t.node.Content) > 0 {
			before = t.node.Content[len(t.node.Content)-1]
		}
		if before == nil || before.Type == "hardBreak" || (before.Type == "text" && strings.HasSuffix(before.Text, " ")) {
			value = value[1:]
		}
	}
	if value == "" {
		return
	}
	t.node.Content = append(t.node.Content, &Node{Type: "text", Text: value, Marks: p.currentMarks()})
}

// collapseSpace is ProseMirror's: runs of ASCII whitespace are one space.
func collapseSpace(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

func (p *htmlParser) addInline(n *Node) {
	p.ensureInline()
	if p.top().node.Type == "codeBlock" {
		if n.Type == "hardBreak" {
			p.top().node.Content = append(p.top().node.Content, &Node{Type: "text", Text: "\n"})
		}
		return
	}
	n.Marks = p.currentMarks()
	p.top().node.Content = append(p.top().node.Content, n)
}

// addNode handles an element the schema has a node for. It reports whether it
// did.
func (p *htmlParser) addNode(n *html.Node, tag string) bool {
	dataType := attrVal(n, "data-type")
	switch tag {
	case "p":
		p.block(n, newNode("paragraph"))
	case "h1", "h2", "h3", "h4", "h5", "h6":
		h := newNode("heading")
		h.setAttr("level", float64(tag[1]-'0'))
		p.block(n, h)
	case "blockquote":
		p.block(n, newNode("blockquote"))
	case "ul":
		if readsAsTaskList(n) {
			p.block(n, newNode("taskList"))
		} else {
			p.block(n, newNode("bulletList"))
		}
	case "ol":
		ol := newNode("orderedList")
		if s, ok := attrOf(n, "start"); ok {
			if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
				ol.setAttr("start", float64(v))
			}
		}
		if t, ok := attrOf(n, "type"); ok && t != "" {
			ol.setAttr("type", t)
		}
		if readsAsTaskList(n) {
			ol = newNode("taskList")
		}
		p.block(n, ol)
	case "li":
		t := p.top()
		if t.spec != nil && t.spec.content == holdsTaskItems {
			it := newNode("taskItem")
			it.setAttr("checked", itemTicked(n))
			p.block(n, it)
		} else {
			p.block(n, newNode("listItem"))
		}
	case "pre":
		cb := newNode("codeBlock")
		if lang := codeLanguage(n); lang != "" {
			cb.setAttr("language", lang)
		}
		if attrVal(n, "data-wrap") == "true" {
			cb.setAttr("wrap", true)
		}
		if v := attrVal(n, "data-caption"); v != "" {
			cb.setAttr("caption", v)
		}
		p.globals(cb, n)
		p.closeInline()
		p.openBlock(cb, true)
		pn := p.top()
		var text strings.Builder
		preText(n, &text)
		if text.Len() > 0 {
			cb.Content = []*Node{{Type: "text", Text: text.String()}}
		}
		p.closeTo(pn)
	case "hr":
		p.closeInline()
		p.openBlock(p.globals(newNode("horizontalRule"), n), false)
		p.closeTo(p.top())
	case "br":
		p.addInline(newNode("hardBreak"))
	case "img":
		p.image(n, "")
	case "figure":
		img := findFirst(n, "img")
		if img == nil {
			return false
		}
		caption := ""
		if fc := findFirst(n, "figcaption"); fc != nil {
			caption = trimSpace(collapseSpace(textContent(fc)))
		}
		p.image(img, caption)
	case "details":
		// A toggle: its <summary> is its first line, and open or closed is
		// each reader's own (B2).
		c := p.globals(newNode("collapsible"), n)
		p.closeInline()
		p.openBlock(c, false)
		pn := p.top()
		pn.node.Content = append(pn.node.Content, p.summary(findChild(n, "summary"))...)
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			if k.Type == html.ElementNode && strings.EqualFold(k.Data, "summary") {
				continue
			}
			p.add(k)
		}
		p.closeTo(pn)
	case "aside":
		// A box set apart from the text, as Notion exports a callout.
		p.block(n, newNode("callout"))
	case "table":
		p.block(n, newNode("table"))
	case "tr":
		p.block(n, newNode("tableRow"))
	case "th", "td":
		typ := "tableCell"
		if tag == "th" {
			typ = "tableHeader"
		}
		cell := newNode(typ)
		for _, a := range []string{"colspan", "rowspan"} {
			if v, ok := attrOf(n, a); ok {
				if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && i > 1 {
					cell.setAttr(a, float64(i))
				}
			}
		}
		if v, ok := attrOf(n, "colwidth"); ok && colWidths.MatchString(trimSpace(v)) {
			var ws []any
			for _, w := range strings.Split(trimSpace(v), ",") {
				i, _ := strconv.Atoi(w)
				ws = append(ws, float64(i))
			}
			cell.setAttr("colwidth", ws)
		}
		if v := attrVal(n, "data-background"); !p.foreign && IsDocColour(v) {
			cell.setAttr("background", v)
		}
		p.block(n, cell)
	case "thead", "tbody", "tfoot":
		p.addChildren(n)
	case "nav":
		if dataType != "toc" {
			return false
		}
		p.atom(p.globals(newNode("toc"), n))
	case "time":
		if dataType != "date-mention" {
			return false
		}
		p.addInline(readDateMention(n))
		return true
	case "span":
		switch dataType {
		case "inline-math":
			m := newNode("inlineMath")
			m.setAttr("latex", mathSource(n))
			p.addInline(m)
			return true
		case "mention", "channelMention", "entityMention":
			m := newNode(dataType)
			if v, ok := attrOf(n, "data-id"); ok {
				m.setAttr("id", v)
			}
			if v, ok := attrOf(n, "data-label"); ok {
				m.setAttr("label", v)
			}
			if v, ok := attrOf(n, "data-mention-suggestion-char"); ok && v != "" {
				m.setAttr("mentionSuggestionChar", v)
			}
			p.addInline(m)
			return true
		}
		return false
	case "div":
		switch dataType {
		case "callout":
			c := newNode("callout")
			if v := attrVal(n, "data-emoji"); v != "" {
				c.setAttr("emoji", v)
			}
			if v := attrVal(n, "data-color"); v != "" {
				c.setAttr("color", v)
			}
			if v := attrVal(n, "data-icon"); v != "" {
				c.setAttr("icon", v)
			}
			p.block(n, c)
		case "collapsible":
			c := newNode("collapsible")
			// An old toggle's title (data-title, with its data-open): the
			// normaliser makes it the first line.
			_, open := attrOf(n, "data-open")
			if title, ok := attrOf(n, "data-title"); ok || open {
				c.setAttr("title", title)
			}
			p.block(n, c)
		case "toc":
			p.atom(p.globals(newNode("toc"), n))
		case "table-embed":
			p.atom(p.globals(embedNode("tableEmbed", n, [][2]string{{"tableId", "data-table-id"}, {"viewId", "data-view-id"}}), n))
		case "block-math":
			m := newNode("blockMath")
			m.setAttr("latex", mathSource(n))
			p.atom(m)
		case "poll":
			p.atom(p.globals(embedNode("poll", n, [][2]string{{"id", "data-id"}}), n))
		case "chart":
			p.atom(p.globals(embedNode("chartEmbed", n, [][2]string{{"spec", "data-spec"}}), n))
		case "diff":
			p.atom(p.globals(embedNode("diffEmbed", n, [][2]string{{"diff", "data-diff"}}), n))
		case "recording-embed":
			p.atom(p.globals(embedNode("recordingEmbed", n, [][2]string{
				{"egress", "data-egress"}, {"kind", "data-kind"}, {"sid", "data-sid"}, {"u1", "data-u1"}, {"u2", "data-u2"},
			}), n))
		case "column-list":
			p.block(n, newNode("columnList"))
		case "column":
			c := newNode("column")
			if w := strings.TrimSpace(attrVal(n, "data-width")); w != "" && strings.Trim(w, "0123456789") == "" {
				if i, err := strconv.Atoi(w); err == nil && i >= 1 && i <= 99 {
					c.setAttr("width", float64(i))
				}
			}
			// A column outside a row is its blocks (Normalize lifts them out).
			p.block(n, c)
		default:
			m := readConnectedBlock(dataType, n)
			if m == nil {
				m = readMediaBlock(dataType, n, p.foreign, p.assets)
			}
			if m == nil {
				return false
			}
			p.atom(m)
		}
	default:
		return false
	}
	return true
}

// colWidths is a cell's colwidth attribute as Tiptap writes it: "120" or "120,80".
var colWidths = regexp.MustCompile(`^\d{1,5}(,\d{1,5})*$`)

// mathSource is an equation's TeX: its data-latex, or (written by hand) its text.
func mathSource(n *html.Node) string {
	if v, ok := attrOf(n, "data-latex"); ok {
		return v
	}
	return trimSpace(textContent(n))
}

func embedNode(typ string, n *html.Node, attrs [][2]string) *Node {
	e := newNode(typ)
	for _, a := range attrs {
		if v := attrVal(n, a[1]); v != "" {
			e.setAttr(a[0], v)
		}
	}
	return e
}

// block opens a node for an element, adds the element's content to it and
// closes it.
func (p *htmlParser) block(n *html.Node, node *Node) {
	p.globals(node, n)
	p.closeInline()
	p.openBlock(node, false)
	pn := p.top()
	p.addChildren(n)
	p.closeTo(pn)
}

// summary reads a toggle's <summary> as its first line: a heading when it
// holds one (a toggle heading), else a paragraph, then anything else it
// held. No summary is an empty first line.
func (p *htmlParser) summary(s *html.Node) []*Node {
	if s == nil {
		return []*Node{newNode("paragraph")}
	}
	sub := &htmlParser{foreign: p.foreign, assets: p.assets, marks: p.currentMarks()}
	root := &pnode{node: newNode("doc"), spec: specOf("doc")}
	sub.stack = []*pnode{root}
	sub.addChildren(s)
	for len(sub.stack) > 1 {
		sub.pop()
	}
	out := root.node.Content
	if len(out) == 0 || out[0].Type != "paragraph" && out[0].Type != "heading" {
		out = append([]*Node{newNode("paragraph")}, out...)
	}
	return out
}

// atom adds a block with no content.
func (p *htmlParser) atom(node *Node) {
	p.closeInline()
	p.openBlock(node, false)
	p.closeTo(p.top())
}

func (p *htmlParser) image(n *html.Node, caption string) {
	src := trimSpace(attrVal(n, "src"))
	if !safeImageSrc(src, true) {
		// An image with nowhere to load from: its alt text, if it has any.
		if alt := trimSpace(attrVal(n, "alt")); alt != "" {
			p.addTextValue(alt)
		}
		return
	}
	img := newNode("image")
	img.setAttr("src", src)
	for _, a := range []string{"alt", "title"} {
		if v, ok := attrOf(n, a); ok {
			img.setAttr(a, v)
		}
	}
	for _, a := range []string{"width", "height"} {
		if v, ok := attrOf(n, a); ok && v != "" {
			img.setAttr(a, v)
		}
	}
	if !p.foreign {
		for _, a := range [][2]string{{"id", "id"}, {"caption", "caption"}, {"fileName", "filename"}} {
			if v, ok := attrOf(n, a[1]); ok && v != "" {
				img.setAttr(a[0], v)
			}
		}
		readImageMedia(n, img)
	}
	if caption != "" {
		img.setAttr("caption", caption)
	}
	p.atom(p.globals(img, n))
}

// globals reads an element's global attributes (an indent, a direction) onto
// its node.
func (p *htmlParser) globals(node *Node, n *html.Node) *Node {
	for _, g := range globalsOn(node.Type) {
		raw, ok := attrOf(n, g.html)
		if !ok {
			continue
		}
		if v := g.read(raw, p.foreign); !g.isDefault(v) {
			node.setAttr(g.name, v)
		}
	}
	return node
}

// addTextValue adds text that came from an attribute (an image's alt text).
func (p *htmlParser) addTextValue(s string) {
	p.ensureInline()
	p.top().node.Content = append(p.top().node.Content, &Node{Type: "text", Text: s, Marks: p.currentMarks()})
}

// readsAsTaskList reports whether a list element is read as a to-do list: a
// <ul> the editor marked as one, or a list whose every item starts with a
// checkbox.
func readsAsTaskList(n *html.Node) bool {
	switch strings.ToLower(n.Data) {
	case "ul":
		return attrVal(n, "data-type") == "taskList" || isTaskList(n)
	case "ol":
		return isTaskList(n)
	}
	return false
}

// itemTicked reports whether a to-do's element is ticked: data-checked, or a
// checkbox it starts with that is checked.
func itemTicked(n *html.Node) bool {
	if attrVal(n, "data-checked") == "true" {
		return true
	}
	box := leadingCheckbox(n)
	if box == nil {
		return false
	}
	_, checked := attrOf(box, "checked")
	return checked
}

// isTaskList reports whether every item of a list starts with a checkbox, as
// GitHub and most Markdown renderers write a task list.
func isTaskList(n *html.Node) bool {
	items := 0
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || !strings.EqualFold(c.Data, "li") {
			continue
		}
		items++
		if attrVal(c, "data-type") == "taskItem" {
			continue
		}
		if leadingCheckbox(c) == nil {
			return false
		}
	}
	return items > 0
}

// leadingCheckbox is the checkbox an item starts with, looking through
// leading wrappers (<p>, <label>) and skipping whitespace.
func leadingCheckbox(n *html.Node) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case html.TextNode:
			if trimSpace(c.Data) != "" {
				return nil
			}
			continue
		case html.ElementNode:
			tag := strings.ToLower(c.Data)
			if tag == "input" {
				if strings.EqualFold(attrVal(c, "type"), "checkbox") {
					return c
				}
				return nil
			}
			if tag == "p" || tag == "label" || tag == "span" {
				return leadingCheckbox(c)
			}
			return nil
		}
	}
	return nil
}

func codeLanguage(pre *html.Node) string {
	pick := func(class string) string {
		for _, c := range strings.Fields(class) {
			if strings.HasPrefix(c, "language-") && languageClass.MatchString(c) {
				return strings.TrimPrefix(c, "language-")
			}
		}
		return ""
	}
	if l := attrVal(pre, "data-language"); l != "" && languageClass.MatchString("language-"+l) {
		return l
	}
	if l := pick(attrVal(pre, "class")); l != "" {
		return l
	}
	for c := pre.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && strings.EqualFold(c.Data, "code") {
			return pick(attrVal(c, "class"))
		}
	}
	return ""
}

// preText is a <pre>'s text, a <br> as a newline.
func preText(n *html.Node, b *strings.Builder) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case html.TextNode:
			b.WriteString(c.Data)
		case html.ElementNode:
			tag := strings.ToLower(c.Data)
			if tag == "br" {
				b.WriteByte('\n')
				continue
			}
			if ignoredTags[tag] {
				continue
			}
			preText(c, b)
		}
	}
}

func textContent(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			switch c.Type {
			case html.TextNode:
				b.WriteString(c.Data)
			case html.ElementNode:
				if !ignoredTags[strings.ToLower(c.Data)] {
					walk(c)
				}
			}
		}
	}
	walk(n)
	return b.String()
}

func findFirst(n *html.Node, tag string) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			if strings.EqualFold(c.Data, tag) {
				return c
			}
			if f := findFirst(c, tag); f != nil {
				return f
			}
		}
	}
	return nil
}

func findChild(n *html.Node, tag string) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && strings.EqualFold(c.Data, tag) {
			return c
		}
	}
	return nil
}
