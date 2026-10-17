package notion

// Notion's export zip (Export > HTML, or Markdown & CSV), imported as docs by
// anyone, with no connection to Notion. The tree is the zip's folders, as the
// plain zip reading has it (a page's subpages sit in a folder named as the
// page's file), and every name carries the page's 32-digit id, which the
// titles lose. What this adds to that reading is Notion's own HTML: each page
// is a whole document whose header holds its title and icon, and whose blocks
// are Notion's markup, turned here into the blocks a doc has.
//
// THE EXPORT, as Notion and the open importers that read it describe it (read
// 14 Oct 2026: notion.com/help/export-your-content; obsidianmd/obsidian-
// importer src/formats/notion, notion-utils.ts and convert-to-md.ts; the
// stylesheet Notion ships in every HTML export):
//   - a file per page, "<title> <id>.html" (or .md), its subpages in the
//     folder "<title> <id>/", with the files the page shows;
//   - a page is <article class="page"> whose <header> holds
//     .page-header-icon (an emoji in span.icon) and h1.page-title, then
//     table.properties for a database's row, and div.page-body its blocks;
//   - colours are classes: block-color-<colour> on a block, highlight-<colour>
//     on a mark (both <colour>_background for a background);
//   - callouts are figure.callout (the icon in span.icon), toggles
//     ul.toggle > li > details, to-dos ul.to-do-list with div.checkbox-on or
//     -off, equations figure.equation with their TeX in
//     annotation[encoding="application/x-tex"] (inline, in
//     span.notion-text-equation-token), links to pages
//     figure.link-to-page, bookmarks figure.bookmark, and columns
//     div.column-list > div.column; each list item is a list of its own;
//   - a database is a page of its rows (table.collection-content), with its
//     rows' pages in its folder; the Markdown export writes it as a CSV.

import (
	"context"
	"path"
	"regexp"
	"strings"

	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	"github.com/akashc777/OneCamp/business/ContentIO/adapters/htmldom"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

func init() { contentIOBusiness.RegisterZipLayout(export{}) }

// exportedName is a file Notion's export wrote: its name ends in the page's
// id.
var exportedName = regexp.MustCompile(`[ -][0-9a-f]{32}\.(html|md|csv)$`)

type export struct{}

func (export) Name() string { return "notion" }

// Claims a zip whose pages are named as Notion's export names them.
func (export) Claims(names []string) bool {
	for _, n := range names {
		if exportedName.MatchString(path.Base(n)) {
			return true
		}
	}
	return false
}

// Read hands out the zip's pages as the plain zip reading does, an HTML page
// read as Notion wrote it; a database's CSV is said to be left out.
func (export) Read(ctx context.Context, z *contentIOBusiness.ZipArchive, out contentIOBusiness.Output) error {
	for _, n := range z.Names() {
		if strings.HasSuffix(n, ".csv") {
			out.Skip(n, "a Notion database: its rows are the pages in its folder; to bring a database across as a table, an admin imports from Notion in Settings")
		}
	}
	return z.Pages(ctx, out, func(name, text string, assets contentIOBusiness.AssetOpener) (contentIOBusiness.Page, error) {
		if !strings.HasSuffix(strings.ToLower(name), ".html") {
			return contentIOBusiness.TextPage(name, text, assets), nil
		}
		return exportedPage(name, text, assets)
	})
}

// exportedPage reads one page of Notion's HTML export: the title and icon
// from its header, and its blocks.
func exportedPage(key, text string, assets contentIOBusiness.AssetOpener) (contentIOBusiness.Page, error) {
	doc, err := html.Parse(strings.NewReader(text))
	if err != nil {
		return contentIOBusiness.Page{}, err
	}
	title, icon := "", ""
	if h := htmldom.First(doc, htmldom.Classed("page-title")); h != nil {
		title = htmldom.Text(h)
	} else if t := htmldom.First(doc, htmldom.Tag(atom.Title)); t != nil {
		title = htmldom.Text(t)
	}
	if hi := htmldom.First(doc, htmldom.Classed("page-header-icon")); hi != nil {
		if span := htmldom.First(hi, htmldom.Classed("icon")); span != nil && span.DataAtom == atom.Span {
			icon = htmldom.Text(span)
		}
	}
	body := htmldom.First(doc, htmldom.Classed("page-body"))
	if body == nil {
		return contentIOBusiness.Page{Key: key, Title: title, Icon: icon, Body: contentcodec.Normalize(nil, nil), Assets: assets}, nil
	}
	blocks(body)
	inner, err := htmldom.Inner(body)
	if err != nil {
		return contentIOBusiness.Page{}, err
	}
	parsed, err := contentcodec.FromHTML(inner, contentcodec.HTMLOptions{Foreign: true})
	if err != nil {
		return contentIOBusiness.Page{}, err
	}
	return contentIOBusiness.Page{Key: key, Title: title, Icon: icon, Body: parsed, Assets: assets}, nil
}

// blocks turns Notion's markup into the HTML a doc's blocks read.
func blocks(root *html.Node) {
	colours(root)
	for _, c := range htmldom.Nested(root, htmldom.Classed("callout")) {
		emoji := ""
		if span := htmldom.First(c, htmldom.Classed("icon")); span != nil {
			emoji = htmldom.Text(span)
			// The icon's box; the text is in the box beside it.
			if box := span.Parent; box != nil && box != c {
				htmldom.Remove(box)
			} else {
				htmldom.Remove(span)
			}
		}
		callout := htmldom.Element(atom.Div, nil, c)
		callout.Attr = []html.Attribute{{Key: "data-type", Val: "callout"}, {Key: "data-color", Val: CalloutColour(colourClass(c, "block-color-"))}}
		if emoji != "" {
			callout.Attr = append(callout.Attr, html.Attribute{Key: "data-emoji", Val: emoji})
		}
		htmldom.Replace(c, callout)
	}
	for _, token := range htmldom.All(root, htmldom.Classed("notion-text-equation-token")) {
		if tex := texOf(token); tex != "" {
			math := htmldom.Element(atom.Span, nil, nil)
			math.Attr = []html.Attribute{{Key: "data-type", Val: "inline-math"}, {Key: "data-latex", Val: tex}}
			htmldom.Replace(token, math)
		}
	}
	for _, list := range htmldom.Nested(root, htmldom.Classed("to-do-list")) {
		list.Attr = []html.Attribute{{Key: "data-type", Val: "taskList"}}
		for li := list.FirstChild; li != nil; li = li.NextSibling {
			if li.DataAtom != atom.Li {
				continue
			}
			checked := "false"
			if box := htmldom.First(li, htmldom.Classed("checkbox")); box != nil {
				if htmldom.HasClass(box, "checkbox-on") {
					checked = "true"
				}
				htmldom.Remove(box)
			}
			li.Attr = []html.Attribute{{Key: "data-type", Val: "taskItem"}, {Key: "data-checked", Val: checked}}
		}
	}
	for _, list := range htmldom.Nested(root, htmldom.Classed("toggle")) {
		// ul.toggle > li > details: the details alone.
		if d := htmldom.First(list, htmldom.Tag(atom.Details)); d != nil {
			htmldom.Remove(d)
			htmldom.Replace(list, d)
		}
	}
	for _, eq := range htmldom.All(root, htmldom.Classed("equation")) {
		if tex := texOf(eq); tex != "" {
			math := htmldom.Element(atom.Div, nil, nil)
			math.Attr = []html.Attribute{{Key: "data-type", Val: "block-math"}, {Key: "data-latex", Val: tex}}
			htmldom.Replace(eq, math)
		}
	}
	for _, f := range htmldom.All(root, func(n *html.Node) bool {
		return htmldom.HasClass(n, "link-to-page") || htmldom.HasClass(n, "bookmark")
	}) {
		a := htmldom.First(f, htmldom.Tag(atom.A))
		if a == nil {
			htmldom.Remove(f)
			continue
		}
		for _, icon := range htmldom.All(a, htmldom.Classed("icon")) {
			htmldom.Remove(icon)
		}
		label := htmldom.Text(a)
		if t := htmldom.First(a, htmldom.Classed("bookmark-title")); t != nil {
			label = htmldom.Text(t)
		}
		if label == "" {
			label = htmldom.Attr(a, "href")
		}
		link := htmldom.Element(atom.A, []*html.Node{htmldom.TextNode(label)}, nil)
		link.Attr = []html.Attribute{{Key: "href", Val: htmldom.Attr(a, "href")}}
		htmldom.Replace(f, htmldom.Element(atom.P, []*html.Node{link}, nil))
	}
	for _, cols := range htmldom.All(root, htmldom.Classed("column-list")) {
		for _, col := range htmldom.All(cols, htmldom.Classed("column")) {
			unwrap(col)
		}
		unwrap(cols)
	}
	joinLists(root)
}

// texOf is the TeX an equation KaTeX drew was written in.
func texOf(n *html.Node) string {
	tex := htmldom.First(n, func(x *html.Node) bool {
		return x.Data == "annotation" && htmldom.Attr(x, "encoding") == "application/x-tex"
	})
	if tex == nil {
		return ""
	}
	return strings.TrimSpace(htmldom.RawText(tex))
}

// colourClass is the colour a Notion class with the prefix names on n
// ("block-color-blue_background" is "blue_background"), or "".
func colourClass(n *html.Node, prefix string) string {
	for _, c := range strings.Fields(htmldom.Attr(n, "class")) {
		if colour, ok := strings.CutPrefix(c, prefix); ok {
			return colour
		}
	}
	return ""
}

// colours writes Notion's colour classes as a doc's colours: a block's as
// its stored colour, a highlight's as a text colour or a highlight.
func colours(root *html.Node) {
	for _, n := range htmldom.All(root, func(x *html.Node) bool { return colourClass(x, "block-color-") != "" }) {
		if colour := BlockColour(colourClass(n, "block-color-")); colour != "" {
			n.Attr = append(n.Attr, html.Attribute{Key: "data-block-color", Val: colour})
		}
		colours(n)
	}
	for _, m := range htmldom.All(root, func(x *html.Node) bool { return x.DataAtom == atom.Mark && colourClass(x, "highlight-") != "" }) {
		name, background := DocColour(colourClass(m, "highlight-"))
		if !background {
			// A text colour: Notion writes it as a mark too.
			m.DataAtom, m.Data = atom.Span, "span"
		}
		m.Attr = nil
		if name != "" {
			m.Attr = []html.Attribute{{Key: "data-color", Val: name}}
		}
	}
}

// unwrap puts n's children where n is.
func unwrap(n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		n.RemoveChild(c)
		n.Parent.InsertBefore(c, n)
		c = next
	}
	htmldom.Remove(n)
}

// joinLists makes Notion's run of one-item lists one list: each item it
// exports is a list of its own, and a numbered list would count 1, 1, 1. One
// walk: the lists of its kind after a list move into it, then what is inside
// it is joined.
func joinLists(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		if c.DataAtom == atom.Ul || c.DataAtom == atom.Ol {
			for next := nextElement(c); next != nil && next.DataAtom == c.DataAtom && htmldom.Attr(next, "class") == htmldom.Attr(c, "class") &&
				htmldom.Attr(next, "data-type") == htmldom.Attr(c, "data-type"); next = nextElement(c) {
				for li := next.FirstChild; li != nil; {
					after := li.NextSibling
					next.RemoveChild(li)
					c.AppendChild(li)
					li = after
				}
				htmldom.Remove(next)
			}
		}
		joinLists(c)
	}
}

// nextElement is the element after n, past whitespace.
func nextElement(n *html.Node) *html.Node {
	for s := n.NextSibling; s != nil; s = s.NextSibling {
		if s.Type == html.ElementNode {
			return s
		}
		if s.Type == html.TextNode && strings.TrimSpace(s.Data) != "" {
			return nil
		}
	}
	return nil
}
