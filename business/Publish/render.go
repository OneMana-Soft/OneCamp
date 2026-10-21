package business

// The page's body: the doc's HTML as its editors saved it, made fit for anyone
// on the web (docs C6, D-25). Two passes:
//
//  1. A walk of the parsed HTML that keeps the page and nothing else of the
//     workspace. A block that shows live workspace data (a task list, a table
//     or a board shown in the doc, a poll, a chart, a recording, a button) is a
//     note that it isn't shown. A mention of a person, a page, a task or a
//     channel is its words alone. A link into the app loses its address; one
//     to the web keeps it. A picture or a file is shown only when it is this
//     doc's own, by the page's own address for it. Colours, kept by name, are
//     classes the page's style knows.
//  2. A strict allowlist (bluemonday) over what is left: the elements and
//     attributes a document needs, and nothing that runs, styles or loads.
//
// A block the walk doesn't know shows its words; one whose kind says it embeds
// something is hidden, so a live block added later fails closed.
//
// The same walk makes a copy of the page for someone's own docs (Duplicate,
// copying): what the page hides is left out, what it shows keeps the editor's
// own form, and the doc's files are the copy's own.

import (
	"bytes"
	"net/url"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// hiddenNote is what stands in for a block the page doesn't show.
const hiddenNote = "This part of the page isn't shown on the web."

// Blocks shown on the web as a note: they show live workspace data, or run,
// or (the page's contents) are drawn by the app as it opens.
var hiddenKinds = map[string]bool{
	"task-list-embed": true, "table-embed": true, "board-embed": true, "recording-embed": true,
	"chart": true, "poll": true, "button": true, "toc": true,
}

// Blocks that are their words alone: they name someone or something in the
// workspace, whose address and id stay in it.
var wordsKinds = map[string]bool{
	"mention": true, "entityMention": true, "channelMention": true, "subpage": true, "date-mention": true, "link-mention": true,
}

// Blocks drawn as plain document structure, by the class the page's style
// knows them by.
var structureKinds = map[string]string{
	"callout": "callout", "column-list": "columns", "column": "column", "taskList": "tasks",
}

// docColours are the colours the editor keeps by name (lib/docColours).
var docColours = map[string]bool{"gray": true, "sun": true, "moss": true, "lake": true, "sky": true, "dusk": true, "berry": true}

// attachmentRef finds a doc's own file in an address the editor wrote:
// /doc/getDocAttachment/<doc>/<file>, absolute or not.
var attachmentRef = regexp.MustCompile(`/doc/getDocAttachment/([0-9a-fA-F-]{36})/([0-9a-fA-F-]{36})`)

// renderer makes one doc's body fit for its page, or for a copy of it.
type renderer struct {
	docUUID string
	// fileURL is the page's own address for one of the doc's files; for a
	// copy, the copy's address for its own.
	fileURL func(objUUID string) string
	copying bool
	// embedHosts are the hosts an admin added to the sites docs can embed
	// (the policy embed.allowlist): their embeds become cards too (D-26).
	embedHosts []string
}

// fileOf is the doc's own file an address names, "" for any other.
func (r renderer) fileOf(addr string) string {
	m := attachmentRef.FindStringSubmatch(addr)
	if m == nil || !strings.EqualFold(m[1], r.docUUID) {
		return ""
	}
	return strings.ToLower(m[2])
}

// ownFile is fileOf for a file the page (or the copy) has an address for: a
// copy has none for a file it couldn't be given.
func (r renderer) ownFile(addr string) string {
	if f := r.fileOf(addr); f != "" && r.fileURL(f) != "" {
		return f
	}
	return ""
}

// filesIn are the doc's own files its saved HTML names, each once, in order.
// Pure.
func filesIn(docUUID, saved string) []string {
	r := renderer{docUUID: docUUID}
	seen := map[string]bool{}
	var out []string
	for _, m := range attachmentRef.FindAllString(saved, -1) {
		if f := r.fileOf(m); f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

func element(tag atom.Atom, attrs ...html.Attribute) *html.Node {
	return &html.Node{Type: html.ElementNode, DataAtom: tag, Data: tag.String(), Attr: attrs}
}

func text(s string) *html.Node { return &html.Node{Type: html.TextNode, Data: s} }

func note() *html.Node {
	p := element(atom.P, html.Attribute{Key: "class", Val: "oc-hidden"})
	p.AppendChild(text(hiddenNote))
	return p
}

// webLink is a link to the web, or nil for an address that isn't one.
func webLink(href, words string) *html.Node {
	if !isWebAddress(href) || intoApp(href) || attachmentRef.MatchString(href) {
		return nil
	}
	a := element(atom.A, html.Attribute{Key: "href", Val: href})
	if words == "" {
		words = href
	}
	a.AppendChild(text(words))
	return a
}

// intoApp is an address of the workspace's own (the web app's, or this
// server's): a link into the app, whose address stays in it. Its host is
// compared as the browser would, without case.
func intoApp(href string) bool {
	u, err := url.Parse(strings.TrimSpace(href))
	if err != nil || u.Host == "" {
		return false
	}
	for _, base := range []string{webBase(), apiBase()} {
		if b, err := url.Parse(base); err == nil && b.Host != "" && strings.EqualFold(b.Host, u.Host) {
			return true
		}
	}
	return false
}

func isWebAddress(href string) bool {
	h := strings.ToLower(strings.TrimSpace(href))
	return strings.HasPrefix(h, "https://") || strings.HasPrefix(h, "http://") || strings.HasPrefix(h, "mailto:")
}

// replace puts with in n's place.
func replace(n, with *html.Node) {
	if n.Parent == nil {
		return
	}
	n.Parent.InsertBefore(with, n)
	n.Parent.RemoveChild(n)
}

func remove(n *html.Node) {
	if n.Parent != nil {
		n.Parent.RemoveChild(n)
	}
}

// unwrap puts n's children in its place.
func unwrap(n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		n.RemoveChild(c)
		n.Parent.InsertBefore(c, n)
		c = next
	}
	n.Parent.RemoveChild(n)
}

// adopt moves n's children into box.
func adopt(n, box *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		n.RemoveChild(c)
		box.AppendChild(c)
		c = next
	}
}

// hide takes a block the page doesn't show out: a note on the page, nothing
// in a copy.
func (r renderer) hide(n *html.Node) {
	if r.copying {
		remove(n)
		return
	}
	replace(n, note())
}

// colourClasses are the classes for the colours an element keeps by name: its
// text colour or highlight (data-color), its block's colour (data-block-color,
// "sky" or "sky_background"). Pure.
func colourClasses(n *html.Node) []string {
	var out []string
	if c := attr(n, "data-color"); docColours[c] {
		if n.DataAtom == atom.Mark {
			out = append(out, "h-"+c)
		} else {
			out = append(out, "c-"+c)
		}
	}
	if v := attr(n, "data-block-color"); v != "" {
		c, background, _ := strings.Cut(v, "_")
		switch {
		case docColours[c] && background == "background":
			out = append(out, "bg-"+c)
		case docColours[c] && background == "":
			out = append(out, "bc-"+c)
		}
	}
	return out
}

// withClasses gives el the classes, after any it has.
func withClasses(el *html.Node, classes ...string) {
	if len(classes) == 0 {
		return
	}
	for i, a := range el.Attr {
		if a.Key == "class" {
			el.Attr[i].Val = strings.TrimSpace(a.Val + " " + strings.Join(classes, " "))
			return
		}
	}
	el.Attr = append(el.Attr, html.Attribute{Key: "class", Val: strings.Join(classes, " ")})
}

// walk makes every node under n fit for the web.
func (r renderer) walk(n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		r.visit(c)
		c = next
	}
}

func (r renderer) visit(n *html.Node) {
	if n.Type != html.ElementNode {
		return
	}
	kind := attr(n, "data-type")
	colours := colourClasses(n)
	if r.copying {
		colours = nil
		// A copy keeps the editor's attributes, but nothing that runs.
		kept := n.Attr[:0]
		for _, a := range n.Attr {
			if !strings.HasPrefix(strings.ToLower(a.Key), "on") {
				kept = append(kept, a)
			}
		}
		n.Attr = kept
	}
	switch {
	case kind == "web-embed":
		// An embed the workspace allows is a card naming its site, never a
		// frame (D-26); any other, a plain link.
		href := firstOf(n, "data-src", "src", "href", "data-url")
		if name, host, ok := embedSite(href, r.embedHosts); ok && webLink(href, "") != nil && !r.copying {
			replace(n, embedCard(href, name, host))
		} else if a := webLink(href, ""); a != nil {
			p := element(atom.P)
			p.AppendChild(a)
			replace(n, p)
		} else {
			r.hide(n)
		}
		return
	case kind == "toc" && r.copying:
		// A copy's contents are drawn from its own headings.
		return
	case hiddenKinds[kind] || strings.Contains(kind, "embed"):
		r.hide(n)
		return
	case wordsKinds[kind]:
		words := textOf(n)
		if words == "" {
			words = firstOf(n, "data-label", "data-title", "data-name")
		}
		if kind == "link-mention" {
			if a := webLink(firstOf(n, "href", "data-href", "data-url"), words); a != nil {
				replace(n, a)
				return
			}
		}
		span := element(atom.Span)
		span.AppendChild(text(words))
		replace(n, span)
		return
	case kind == "file" || kind == "bookmark":
		r.link(n, kind)
		return
	case r.copying && (kind == "taskItem" || kind == "collapsible" || structureKinds[kind] != ""):
		r.walk(n)
		return
	case r.copying && (kind == "inline-math" || kind == "block-math" || kind == "diff"):
		return
	case kind == "taskItem":
		done := attr(n, "data-checked") == "true"
		li := element(atom.Li, html.Attribute{Key: "class", Val: map[bool]string{true: "task done", false: "task"}[done]})
		withClasses(li, colours...)
		mark := element(atom.Span, html.Attribute{Key: "class", Val: "box"})
		mark.AppendChild(text(map[bool]string{true: "☑ ", false: "☐ "}[done]))
		li.AppendChild(mark)
		adopt(n, li)
		replace(n, li)
		r.walk(li)
		return
	case kind == "inline-math" || kind == "block-math":
		tex := firstOf(n, "data-latex", "data-content")
		if tex == "" {
			tex = textOf(n)
		}
		code := element(atom.Code, html.Attribute{Key: "class", Val: "math"})
		code.AppendChild(text(tex))
		if kind == "block-math" {
			p := element(atom.P)
			p.AppendChild(code)
			replace(n, p)
			return
		}
		replace(n, code)
		return
	case kind == "diff":
		replace(n, diffBlock(attr(n, "data-diff")))
		return
	case kind == "collapsible":
		// A toggle, as a document can draw one without a script: its first
		// line is its summary.
		details := element(atom.Details, html.Attribute{Key: "class", Val: "toggle"})
		withClasses(details, colours...)
		summary := element(atom.Summary)
		if first := firstElement(n); first != nil {
			n.RemoveChild(first)
			switch first.DataAtom {
			case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
				// A toggle heading keeps its heading.
				summary.AppendChild(first)
			default:
				adopt(first, summary)
			}
		}
		details.AppendChild(summary)
		adopt(n, details)
		replace(n, details)
		r.walk(details)
		return
	case structureKinds[kind] != "":
		tag := atom.Div
		if kind == "taskList" {
			tag = atom.Ul
		}
		box := element(tag, html.Attribute{Key: "class", Val: structureKinds[kind]})
		withClasses(box, colours...)
		inner := box
		if kind == "callout" {
			// Its emoji beside what it says.
			if icon := attr(n, "data-icon"); emojiOnly(icon) {
				span := element(atom.Span, html.Attribute{Key: "class", Val: "ci"})
				span.AppendChild(text(icon))
				box.AppendChild(span)
			}
			inner = element(atom.Div)
			box.AppendChild(inner)
		}
		adopt(n, inner)
		replace(n, box)
		r.walk(inner)
		return
	}
	switch n.DataAtom {
	case atom.A:
		href := attr(n, "href")
		if file := r.ownFile(href); file != "" {
			n.Attr = []html.Attribute{{Key: "href", Val: r.fileURL(file)}}
			r.walk(n)
			return
		}
		if !isWebAddress(href) || intoApp(href) || attachmentRef.MatchString(href) {
			// Into the app (another doc's file among it), or nowhere a
			// visitor can go: its words stay.
			r.walk(n)
			unwrap(n)
			return
		}
		n.Attr = []html.Attribute{{Key: "href", Val: href}}
		r.walk(n)
		return
	case atom.Img:
		file := r.ownFile(attr(n, "src"))
		if file == "" {
			// Another doc's picture, or one from another site: its words, if any.
			if alt := strings.TrimSpace(attr(n, "alt")); alt != "" {
				span := element(atom.Span)
				span.AppendChild(text(alt))
				replace(n, span)
			} else {
				remove(n)
			}
			return
		}
		if r.copying {
			r.ownRefs(n)
			return
		}
		keep := []html.Attribute{{Key: "src", Val: r.fileURL(file)}, {Key: "loading", Val: "lazy"}}
		for _, k := range []string{"alt", "width", "height"} {
			if v := attr(n, k); v != "" {
				keep = append(keep, html.Attribute{Key: k, Val: v})
			}
		}
		n.Attr = keep
		return
	case atom.Iframe, atom.Video, atom.Audio, atom.Object, atom.Embed, atom.Canvas, atom.Svg, atom.Math:
		r.hide(n)
		return
	case atom.Script, atom.Style, atom.Link, atom.Meta, atom.Template, atom.Noscript, atom.Form, atom.Input, atom.Button, atom.Select, atom.Textarea:
		remove(n)
		return
	}
	if !r.copying {
		keepClasses(n, colours)
	}
	r.walk(n)
}

// keepClasses leaves n the classes the page's style knows, and adds extra.
func keepClasses(n *html.Node, extra []string) {
	var keep []string
	for _, c := range strings.Fields(attr(n, "class")) {
		if knownClass.MatchString(c) {
			keep = append(keep, c)
		}
	}
	keep = append(keep, extra...)
	attrs := n.Attr[:0]
	for _, a := range n.Attr {
		if a.Key != "class" {
			attrs = append(attrs, a)
		}
	}
	n.Attr = attrs
	if len(keep) > 0 {
		n.Attr = append(n.Attr, html.Attribute{Key: "class", Val: strings.Join(keep, " ")})
	}
}

// ownRefs gives every address under n that names one of the doc's files the
// copy's own address for it, and drops any that names another doc's.
func (r renderer) ownRefs(n *html.Node) {
	if n.Type == html.ElementNode {
		kept := n.Attr[:0]
		for _, a := range n.Attr {
			if attachmentRef.MatchString(a.Val) {
				file := r.ownFile(a.Val)
				if file == "" {
					continue
				}
				a.Val = r.fileURL(file)
			}
			kept = append(kept, a)
		}
		n.Attr = kept
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		r.ownRefs(c)
	}
}

func firstElement(n *html.Node) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			return c
		}
	}
	return nil
}

// emojiOnly is a short run of symbols with nothing that reads as words: a
// callout's emoji. Pure.
func emojiOnly(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if r < 0x2000 && r != 0x200D && r != 0x20E3 && r != '#' && r != '*' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// diffBlock is a patch as a document draws it: each line marked as added,
// removed or a header, by class.
func diffBlock(patch string) *html.Node {
	pre := element(atom.Pre, html.Attribute{Key: "class", Val: "diff"})
	code := element(atom.Code)
	pre.AppendChild(code)
	for _, line := range strings.Split(strings.TrimRight(patch, "\n"), "\n") {
		class := ""
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"), strings.HasPrefix(line, "@@"), strings.HasPrefix(line, "diff "):
			class = "dm"
		case strings.HasPrefix(line, "+"):
			class = "da"
		case strings.HasPrefix(line, "-"):
			class = "dd"
		}
		if class == "" {
			code.AppendChild(text(line + "\n"))
			continue
		}
		span := element(atom.Span, html.Attribute{Key: "class", Val: class})
		span.AppendChild(text(line + "\n"))
		code.AppendChild(span)
	}
	return pre
}

// link is a file or a bookmark: a file only when it is this doc's own, a
// bookmark only to the web. On the page it is a link; in a copy it keeps its
// block, with the copy's own address for a file.
func (r renderer) link(n *html.Node, kind string) {
	addr := firstOf(n, "data-src", "src", "href", "data-url", "data-href")
	if addr == "" {
		// The address may be on the link the block holds.
		var find func(*html.Node)
		find = func(c *html.Node) {
			if addr != "" {
				return
			}
			if c.Type == html.ElementNode && c.DataAtom == atom.A {
				addr = attr(c, "href")
			}
			for k := c.FirstChild; k != nil; k = k.NextSibling {
				find(k)
			}
		}
		find(n)
	}
	words := firstOf(n, "data-name", "data-title", "data-caption")
	if words == "" {
		words = textOf(n)
	}
	own := kind == "file" && r.ownFile(addr) != ""
	web := kind == "bookmark" && webLink(addr, "") != nil
	if r.copying && (own || web) {
		r.ownRefs(n)
		return
	}
	p := element(atom.P)
	switch {
	case own:
		a := element(atom.A, html.Attribute{Key: "href", Val: r.fileURL(r.ownFile(addr))})
		if words == "" {
			words = "A file"
		}
		a.AppendChild(text(words))
		p.AppendChild(a)
	case web:
		p.AppendChild(webLink(addr, words))
	case words != "":
		p.AppendChild(text(words))
	default:
		remove(n)
		return
	}
	replace(n, p)
}

func firstOf(n *html.Node, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(attr(n, k)); v != "" {
			return v
		}
	}
	return ""
}

// classToken is one class the page's style knows; an element's classes are
// one or more of them.
const classToken = `(callout|columns|column|toggle|tasks|task|done|box|math|diff|da|dd|dm|ci|oc-hidden|embed|es|eh|language-[a-z0-9+#-]{1,24}|(c|h|bc|bg)-(gray|sun|moss|lake|sky|dusk|berry))`

var (
	allowedClass = regexp.MustCompile(`^` + classToken + `( ` + classToken + `)*$`)
	knownClass   = regexp.MustCompile(`^` + classToken + `$`)
)

// pageFile is a page's own file, by its published address or its link's
// token, with a password page's unlock.
const pageFile = `/public/(p/[a-z0-9-]{1,64}|d/[A-Za-z0-9_-]{8,128})/files/[0-9a-f-]{36}(\?k=[A-Za-z0-9._-]+)?`

// policy is the allowlist the walked HTML passes through.
var policy = func() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("p", "br", "hr", "h1", "h2", "h3", "h4", "h5", "h6", "strong", "b", "em", "i", "u", "s", "del", "ins",
		"mark", "sub", "sup", "code", "pre", "blockquote", "ul", "ol", "li", "table", "thead", "tbody", "tfoot", "tr", "th",
		"td", "span", "div", "figure", "figcaption", "details", "summary", "a", "img", "colgroup", "col")
	p.AllowURLSchemes("http", "https", "mailto")
	p.AllowRelativeURLs(true)
	p.RequireParseableURLs(true)
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	// A link goes to the web, or to one of this page's own files.
	p.AllowAttrs("href").Matching(regexp.MustCompile(`^(https?://|mailto:|` + pageFile + `$)`)).OnElements("a")
	// A picture is one of this page's own.
	p.AllowAttrs("src").Matching(regexp.MustCompile(`^` + pageFile + `$`)).OnElements("img")
	p.AllowAttrs("alt").OnElements("img")
	p.AllowAttrs("width", "height").Matching(bluemonday.Integer).OnElements("img")
	p.AllowAttrs("loading").Matching(regexp.MustCompile(`^lazy$`)).OnElements("img")
	p.AllowAttrs("colspan", "rowspan").Matching(bluemonday.Integer).OnElements("td", "th")
	p.AllowAttrs("start").Matching(bluemonday.Integer).OnElements("ol")
	p.AllowAttrs("class").Matching(allowedClass).OnElements("div", "span", "ul", "ol", "li", "code", "p", "pre", "mark",
		"h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "details", "td", "th")
	return p
}()

// parse is saved HTML as nodes under one root.
func parse(saved string) *html.Node {
	root := element(atom.Div)
	nodes, err := html.ParseFragment(strings.NewReader(saved), element(atom.Div))
	if err != nil {
		return root
	}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	return root
}

func renderChildren(root *html.Node) (string, bool) {
	var b bytes.Buffer
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&b, c); err != nil {
			return "", false
		}
	}
	return b.String(), true
}

// body is a doc's saved HTML as its page shows it.
func (r renderer) body(saved string) string {
	root := parse(saved)
	r.walk(root)
	out, ok := renderChildren(root)
	if !ok {
		return ""
	}
	return policy.Sanitize(out)
}

// copyOf is a doc's saved HTML as a copy of its page keeps it: the editor's
// own form, without what the page hides.
func (r renderer) copyOf(saved string) string {
	r.copying = true
	root := parse(saved)
	r.walk(root)
	out, _ := renderChildren(root)
	return out
}
