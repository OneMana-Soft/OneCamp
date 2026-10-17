// Package confluence reads a Confluence space's HTML export (Space settings >
// Export space > HTML) into docs, keeping the page tree and the titles.
//
// THE EXPORT, as Atlassian documents it and as the open converters read it
// (sources, read 14 Oct 2026: support.atlassian.com "Export content to Word,
// PDF, HTML and XML"; github.com/meridius/confluence-to-markdown, Page.coffee
// and Formatter.coffee):
//   - a zip of one folder per space, holding an HTML file per page, the
//     space's details as index.html, its stylesheet as styles/site.css, and
//     each page's attachments under attachments/<page id>/;
//   - a page's <title> is "<space> : <page>", where <space> is the first
//     breadcrumb (#breadcrumbs .first); the rest of the breadcrumbs are the
//     page's ancestors, the last one its parent;
//   - its content is #main-content; the attachments and comments sections
//     sit outside it;
//   - blog posts and comments are not in the export.
//
// Confluence's macros come as classed HTML, which an import reads as foreign
// HTML (with its classes dropped), so the ones a doc has a block for are
// turned into it first: info, note, tip and warning panels into callouts,
// code blocks into code with their language, expands into toggles, and task
// lists into to-dos.
package confluence

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

func init() { contentIOBusiness.RegisterZipLayout(space{}) }

type space struct{}

func (space) Name() string { return "confluence" }

// Claims a zip with a space folder: an index.html beside styles/site.css.
func (space) Claims(names []string) bool {
	have := make(map[string]bool, len(names))
	for _, n := range names {
		have[n] = true
	}
	for _, n := range names {
		if path.Base(n) == "index.html" && have[path.Join(path.Dir(n), "styles", "site.css")] {
			return true
		}
	}
	return false
}

func (space) Read(ctx context.Context, z *contentIOBusiness.ZipArchive, out contentIOBusiness.Output) error {
	var pages []string
	for _, n := range z.Names() {
		if strings.HasSuffix(n, ".html") && path.Base(n) != "index.html" && !strings.Contains(n, "/attachments/") && !strings.HasPrefix(n, "attachments/") {
			pages = append(pages, n)
		}
	}
	assets := z.Assets()
	for i, name := range pages {
		if err := ctx.Err(); err != nil {
			return err
		}
		text, err := z.ReadText(name)
		if err != nil {
			out.Skip(name, err.Error())
			continue
		}
		pg, err := page(name, text, assets)
		if err != nil {
			out.Skip(name, "it could not be read as a Confluence page")
			continue
		}
		if err := out.Page(pg); err != nil {
			return err
		}
		out.Progress(i+1, len(pages))
	}
	return nil
}

// page reads one exported page: its title, its parent from the breadcrumbs,
// and #main-content as its body.
func page(key, text string, assets contentIOBusiness.AssetOpener) (contentIOBusiness.Page, error) {
	doc, err := html.Parse(strings.NewReader(text))
	if err != nil {
		return contentIOBusiness.Page{}, err
	}
	dir := path.Dir(key)
	spaceName, parent := "", ""
	if crumbs := htmldom.ByID(doc, "breadcrumbs"); crumbs != nil {
		for li := crumbs.FirstChild; li != nil; li = li.NextSibling {
			if li.DataAtom != atom.Li {
				continue
			}
			a := htmldom.First(li, htmldom.Tag(atom.A))
			if htmldom.HasClass(li, "first") {
				spaceName = htmldom.Text(li)
				continue
			}
			if a != nil {
				if href := htmldom.Attr(a, "href"); href != "" && href != "index.html" && !strings.Contains(href, "://") {
					parent = path.Join(dir, href)
				}
			}
		}
	}
	title := ""
	if t := htmldom.First(doc, htmldom.Tag(atom.Title)); t != nil {
		title = htmldom.Text(t)
	}
	if spaceName != "" {
		title = strings.TrimPrefix(title, spaceName+" : ")
	}
	main := htmldom.ByID(doc, "main-content")
	var body *contentcodec.Node
	if main != nil {
		macros(main)
		inner, err := htmldom.Inner(main)
		if err != nil {
			return contentIOBusiness.Page{}, err
		}
		body, err = contentcodec.FromHTML(inner, contentcodec.HTMLOptions{Foreign: true})
		if err != nil {
			return contentIOBusiness.Page{}, err
		}
	} else {
		body = contentcodec.Normalize(nil, nil)
	}
	return contentIOBusiness.Page{Key: key, ParentKey: parent, Title: title, Body: body, Assets: assets}, nil
}

var brush = regexp.MustCompile(`brush:\s*([a-zA-Z0-9+#-]+)`)

// macros turns Confluence's macros into the HTML a doc's blocks read.
func macros(root *html.Node) {
	for _, panel := range htmldom.Nested(root, htmldom.Classed("confluence-information-macro")) {
		// A panel: its body, as a callout.
		body := htmldom.First(panel, htmldom.Classed("confluence-information-macro-body"))
		htmldom.Replace(panel, htmldom.Element(atom.Aside, nil, body))
	}
	for _, block := range htmldom.All(root, func(n *html.Node) bool { return htmldom.HasClass(n, "code") && htmldom.HasClass(n, "panel") }) {
		pre := htmldom.First(block, htmldom.Tag(atom.Pre))
		if pre == nil {
			continue
		}
		code := htmldom.Element(atom.Code, []*html.Node{htmldom.TextNode(htmldom.RawText(pre))}, nil)
		if m := brush.FindStringSubmatch(htmldom.Attr(pre, "data-syntaxhighlighter-params")); m != nil {
			code.Attr = []html.Attribute{{Key: "class", Val: "language-" + strings.ToLower(m[1])}}
		}
		htmldom.Replace(block, htmldom.Element(atom.Pre, []*html.Node{code}, nil))
	}
	for _, expand := range htmldom.Nested(root, htmldom.Classed("expand-container")) {
		summary := htmldom.Element(atom.Summary, nil, nil)
		if label := htmldom.First(expand, htmldom.Classed("expand-control-text")); label != nil {
			summary.AppendChild(htmldom.TextNode(htmldom.Text(label)))
		}
		content := htmldom.First(expand, htmldom.Classed("expand-content"))
		htmldom.Replace(expand, htmldom.Element(atom.Details, []*html.Node{summary}, content))
	}
	for _, list := range htmldom.Nested(root, htmldom.Classed("inline-task-list")) {
		for li := list.FirstChild; li != nil; li = li.NextSibling {
			if li.DataAtom == atom.Li {
				li.InsertBefore(htmldom.Checkbox(htmldom.HasClass(li, "checked")), li.FirstChild)
			}
		}
	}
}
