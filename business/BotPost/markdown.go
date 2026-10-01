package botpost

// Markdown for bot replies.
//
// Models answer in markdown: bold for the thing that matters, numbered steps,
// inline code. Bot replies in channels and threads went through a renderer
// that escaped the text into plain paragraphs, so every **bold**, "1." and
// `code` reached people as literal asterisks, digits and backticks. Seen live
// on the demo agent's first answer; true of every agent and OneCamp AI reply
// that used any formatting.
//
// CommonMark via goldmark, narrowed to what a chat message holds:
//   - raw HTML in the reply is shown as text, never interpreted ("use the <div>
//     tag" must read as written);
//   - headings become a bold paragraph, because a chat reply with an <h1> in
//     it shouts over the conversation around it;
//   - images become links, so a reply cannot make every reader's browser fetch
//     a remote URL just by being displayed;
//   - a single newline is a line break, as it was, because that is how people
//     and models both write in chat.
// The result is sanitised with the same policy as any other bot HTML.

import (
	"bytes"
	"html"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"
)

var chatMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.Strikethrough, extension.Linkify),
	goldmark.WithRendererOptions(gmhtml.WithHardWraps(), gmhtml.WithXHTML()),
	goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(chatNodes{}, 100))),
)

// chatNodes overrides how a few node kinds render for chat. See the file comment.
type chatNodes struct{}

func (chatNodes) RegisterFuncs(r renderer.NodeRendererFuncRegisterer) {
	r.Register(ast.KindRawHTML, renderRawAsText)
	r.Register(ast.KindHTMLBlock, renderRawAsText)
	r.Register(ast.KindHeading, renderHeading)
	r.Register(ast.KindImage, renderImageAsLink)
}

func renderRawAsText(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	var raw bytes.Buffer
	switch v := n.(type) {
	case *ast.RawHTML:
		for i := 0; i < v.Segments.Len(); i++ {
			seg := v.Segments.At(i)
			raw.Write(seg.Value(src))
		}
	case *ast.HTMLBlock:
		for i := 0; i < v.Lines().Len(); i++ {
			seg := v.Lines().At(i)
			raw.Write(seg.Value(src))
		}
		if v.HasClosure() {
			raw.Write(v.ClosureLine.Value(src))
		}
		text := strings.TrimRight(raw.String(), "\n")
		_, _ = w.WriteString("<p>" + strings.ReplaceAll(html.EscapeString(text), "\n", "<br/>") + "</p>")
		return ast.WalkSkipChildren, nil
	}
	_, _ = w.WriteString(html.EscapeString(raw.String()))
	return ast.WalkSkipChildren, nil
}

func renderHeading(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		_, _ = w.WriteString("<p><strong>")
	} else {
		_, _ = w.WriteString("</strong></p>")
	}
	return ast.WalkContinue, nil
}

func renderImageAsLink(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	img := n.(*ast.Image)
	label := strings.TrimSpace(altText(img, src))
	if label == "" {
		label = string(img.Destination)
	}
	_, _ = w.WriteString(`<a href="` + html.EscapeString(string(img.Destination)) + `">` + html.EscapeString(label) + `</a>`)
	return ast.WalkSkipChildren, nil
}

var blockNewline = regexp.MustCompile(`(<br/>|</p>|</li>|</ul>|</ol>|</blockquote>|</pre>|<ul>|<ol>|<li>|<blockquote>)\n`)

// renderMarkdown turns a model's markdown into chat HTML, with the typing
// cursor at the end of the last block while a reply is still streaming.
func renderMarkdown(text string, typing bool) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	var buf bytes.Buffer
	if err := chatMarkdown.Convert([]byte(text), &buf); err != nil {
		return renderEscapedParagraphs(text, typing)
	}
	out := strings.TrimSpace(botHTMLPolicy.Sanitize(buf.String()))
	out = strings.NewReplacer("<br />", "<br/>", "<br>", "<br/>").Replace(out)
	// goldmark separates blocks and wrapped lines with newlines; in chat HTML
	// they are noise, and the editor that displays it keeps them as text. Code
	// blocks keep theirs: the tags matched here never occur inside <pre>.
	out = blockNewline.ReplaceAllString(out, "$1")
	if typing {
		out = withCursor(out)
	}
	return out
}

// withCursor puts the typing cursor inside the last text block, so it trails
// the words rather than sitting on a line of its own after a list.
func withCursor(out string) string {
	at := -1
	for _, closing := range []string{"</p>", "</li>", "</code></pre>", "</blockquote>"} {
		if i := strings.LastIndex(out, closing); i > at {
			at = i
		}
	}
	if at < 0 {
		return out + streamCursor
	}
	return out[:at] + streamCursor + out[at:]
}

// altText is an image's alt text: the text of its children.
func altText(n ast.Node, src []byte) string {
	var b strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if t, ok := c.(*ast.Text); ok {
			b.Write(t.Segment.Value(src))
		} else {
			b.WriteString(altText(c, src))
		}
	}
	return b.String()
}
