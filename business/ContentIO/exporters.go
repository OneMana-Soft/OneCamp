package business

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"io"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/akashc777/OneCamp/services/collab"
)

// The server's doc exports, for the API and an admin's export: Markdown, a
// self-contained HTML page, and the editor's JSON. They read the doc as last
// saved; the web app exports the live doc itself (lib/export), and makes PDF
// (the browser's print) and Word files there, so the server runs no renderer.

func init() {
	for _, f := range []string{"md", "html", "json"} {
		Pipeline.RegisterExporter(docExporter{format: f})
	}
}

type docExporter struct{ format string }

func (docExporter) Kind() string { return accessBusiness.KindDoc }

func (e docExporter) Format() string { return e.format }

// loadDoc reads a doc's title and body as who sees it: the body as the live
// copy holds it when the collaboration service has one (A4), which is newer
// than the last save while someone is typing, else as last saved.
var loadDoc = func(ctx context.Context, who accessBusiness.Subject, docUUID string) (string, string, error) {
	doc, err := docBusiness.GetDocByDocUUID(ctx, docUUID, who.UserUID)
	if err != nil {
		return "", "", err
	}
	if doc == nil {
		return "", "", ErrNotFound
	}
	if cur, err := collab.Read(ctx, docUUID); err == nil {
		return doc.Title, cur.HTML, nil
	}
	return doc.Title, doc.Body, nil
}

func (e docExporter) Export(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource, to ExportTarget) error {
	title, stored, err := loadDoc(ctx, who, res.UUID)
	if err != nil {
		return err
	}
	body, err := contentcodec.FromHTML(stored, contentcodec.HTMLOptions{})
	if err != nil {
		return err
	}
	name := exportFilename(title)
	switch e.format {
	case "md":
		_, err = io.WriteString(to.Start(name+".md", "text/markdown; charset=utf-8"), docMarkdown(title, body, authService.FrontendBaseURL()))
	case "html":
		_, err = io.WriteString(to.Start(name+".html", "text/html; charset=utf-8"), docPage(title, body))
	case "json":
		var b []byte
		b, err = docJSON(title, body)
		if err == nil {
			_, err = to.Start(name+".json", "application/json").Write(b)
		}
	default:
		return ErrUnsupported
	}
	return err
}

// docMarkdown is a doc as Markdown, its title as the first heading (which an
// import reads back as the title). The web app writes the same file
// (lib/export/docToMarkdown.ts).
func docMarkdown(title string, body *contentcodec.Node, appURL string) string {
	doc := &contentcodec.Node{Type: "doc"}
	if strings.TrimSpace(title) != "" {
		doc.Content = append(doc.Content, &contentcodec.Node{
			Type: "heading", Attrs: map[string]any{"level": float64(1)},
			Content: []*contentcodec.Node{{Type: "text", Text: title}},
		})
	}
	doc.Content = append(doc.Content, body.Content...)
	return contentcodec.ToMarkdown(doc, contentcodec.MarkdownOptions{AppURL: appURL})
}

func docJSON(title string, body *contentcodec.Node) ([]byte, error) {
	canon, err := contentcodec.CanonicalJSON(body)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString(`{"title":`)
	t, _ := json.Marshal(title)
	b.Write(t)
	b.WriteString(`,"doc":`)
	b.Write(bytes.TrimSpace(canon))
	b.WriteString("}\n")
	return b.Bytes(), nil
}

// exportFilename is a file name from a doc's title.
func exportFilename(title string) string {
	name := uploadsafe.SanitiseFileName(strings.TrimSpace(title), 80)
	if name == "" {
		return "doc"
	}
	return name
}

// docPage is a doc as a page of its own: readable offline, printable, with
// nothing loaded from anywhere. The web app writes the same page
// (lib/export/docToHtml.ts).
func docPage(title string, body *contentcodec.Node) string {
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	b.WriteString("<title>" + html.EscapeString(title) + "</title>\n<style>\n" + pageStyle + "</style>\n</head>\n<body>\n<main>\n")
	if strings.TrimSpace(title) != "" {
		b.WriteString("<h1 class=\"doc-title\">" + html.EscapeString(title) + "</h1>\n")
	}
	b.WriteString(contentcodec.ToHTML(body))
	b.WriteString("\n</main>\n</body>\n</html>\n")
	return b.String()
}

// pageStyle is the exported page's look: the doc's measure and type scale,
// light and dark, and clean print.
const pageStyle = `:root { color-scheme: light dark; --ink: #1f2328; --ink-2: #59636e; --line: #d8dee4; --tint: #f6f8fa; --link: #0b62c4; }
@media (prefers-color-scheme: dark) { :root { --ink: #e6edf3; --ink-2: #9198a1; --line: #3d444d; --tint: #151b23; --link: #4493f8; } }
body { margin: 0; background: Canvas; color: var(--ink); font: 16px/26px system-ui, -apple-system, "Segoe UI", sans-serif; }
main { max-width: 720px; margin: 0 auto; padding: 48px 24px 96px; }
.doc-title { font-size: 40px; line-height: 48px; font-weight: 650; margin: 0 0 24px; }
h1 { font-size: 30px; line-height: 38px; } h2 { font-size: 24px; line-height: 32px; } h3 { font-size: 20px; line-height: 28px; }
h4, h5, h6 { font-size: 16px; line-height: 24px; color: var(--ink-2); }
h1, h2, h3, h4, h5, h6 { font-weight: 650; margin: 32px 0 8px; }
p { margin: 4px 0; } a { color: var(--link); }
blockquote { margin: 8px 0; padding-left: 16px; border-left: 3px solid var(--line); color: var(--ink-2); }
code { font: 14px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace; background: var(--tint); padding: 1px 4px; border-radius: 4px; }
pre { background: var(--tint); padding: 12px 16px; border-radius: 8px; overflow-x: auto; }
pre code { background: none; padding: 0; }
img { max-width: 100%; height: auto; }
hr { border: 0; border-top: 1px solid var(--line); margin: 24px 0; }
table { border-collapse: collapse; margin: 12px 0; } th, td { border: 1px solid var(--line); padding: 6px 10px; text-align: left; vertical-align: top; }
ul[data-type="taskList"] { list-style: none; padding-left: 4px; }
li[data-type="taskItem"] { display: flex; gap: 8px; } li[data-type="taskItem"] > label { flex: none; }
div[data-type="callout"] { background: var(--tint); border-radius: 10px; padding: 12px 16px; margin: 12px 0; }
div[data-type="collapsible"] { border-left: 2px solid var(--line); padding-left: 12px; margin: 8px 0; }
div[data-type="collapsible"] > :first-child { font-weight: 600; }
blockquote[data-quote-size="large"] { font-size: 20px; line-height: 30px; color: var(--ink); }
:root { --gray: #5b5d65; --gray-bg: rgba(20, 22, 30, 0.07); --sun: #7a5200; --sun-bg: #fff6d6; --moss: #1d6a3c; --moss-bg: #e6f6ec; --lake: #0b5c65; --lake-bg: #e1f5f6; --sky: #1e4e9a; --sky-bg: #e7effc; --dusk: #4b3699; --dusk-bg: #efebfc; --berry: #962556; --berry-bg: #fce8ef; }
@media (prefers-color-scheme: dark) { :root { --gray: #a8abb1; --gray-bg: rgba(255, 255, 255, 0.07); --sun: #fbe29c; --sun-bg: #2e2712; --moss: #a6e7c4; --moss-bg: #14291f; --lake: #a3e6ec; --lake-bg: #10292f; --sky: #b9d3fb; --sky-bg: #152238; --dusk: #d4cbfb; --dusk-bg: #211b38; --berry: #f9c2d6; --berry-bg: #33161f; } }
mark { color: inherit; background: var(--sun-bg); border-radius: 3px; padding: 0 1px; }
[data-block-color$="_background"] { border-radius: 6px; padding: 2px 8px; }
span[data-color="gray"], [data-block-color="gray"] { color: var(--gray); } mark[data-color="gray"], [data-block-color="gray_background"] { background: var(--gray-bg); }
span[data-color="sun"], [data-block-color="sun"] { color: var(--sun); } mark[data-color="sun"], [data-block-color="sun_background"] { background: var(--sun-bg); }
span[data-color="moss"], [data-block-color="moss"] { color: var(--moss); } mark[data-color="moss"], [data-block-color="moss_background"] { background: var(--moss-bg); }
span[data-color="lake"], [data-block-color="lake"] { color: var(--lake); } mark[data-color="lake"], [data-block-color="lake_background"] { background: var(--lake-bg); }
span[data-color="sky"], [data-block-color="sky"] { color: var(--sky); } mark[data-color="sky"], [data-block-color="sky_background"] { background: var(--sky-bg); }
span[data-color="dusk"], [data-block-color="dusk"] { color: var(--dusk); } mark[data-color="dusk"], [data-block-color="dusk_background"] { background: var(--dusk-bg); }
span[data-color="berry"], [data-block-color="berry"] { color: var(--berry); } mark[data-color="berry"], [data-block-color="berry_background"] { background: var(--berry-bg); }
div[data-type="table-embed"], div[data-type="poll"], div[data-type="chart"], div[data-type="diff"], div[data-type="recording-embed"] { border: 1px dashed var(--line); border-radius: 8px; padding: 8px 12px; margin: 12px 0; color: var(--ink-2); font-size: 14px; }
div[data-type="table-embed"]::before { content: "A table: open the doc in OneCamp to see it"; }
div[data-type="poll"]::before { content: "A poll: open the doc in OneCamp to vote"; }
div[data-type="chart"]::before { content: "A chart: open the doc in OneCamp to see it"; }
div[data-type="diff"]::before { content: "A change set: open the doc in OneCamp to see it"; }
div[data-type="recording-embed"]::before { content: "A call recording: open the doc in OneCamp to play it"; }
@media print { body { background: #fff; color: #000; } main { max-width: none; padding: 0; } a { color: inherit; } pre, blockquote, img, table { break-inside: avoid; } }
`
