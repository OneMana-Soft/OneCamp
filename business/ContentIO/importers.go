package business

import (
	"context"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// The single-file importers: Markdown, plain text, HTML. Word files are in
// docx.go, zips (of any of them, with their images) in zip.go.

func init() {
	Pipeline.RegisterImporter(markdownImporter{})
	Pipeline.RegisterImporter(textImporter{})
	Pipeline.RegisterImporter(htmlImporter{})
}

// maxTextBytes bounds one Markdown, text or HTML file: far beyond any doc a
// person writes, short of what would make reading it a burden on the server.
const maxTextBytes = 10 << 20

func hasExt(filename string, exts ...string) bool {
	ext := strings.ToLower(path.Ext(filename))
	for _, e := range exts {
		if ext == e {
			return true
		}
	}
	return false
}

func baseMediaType(mediaType string) string {
	t, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(mediaType)), ";")
	return strings.TrimSpace(t)
}

// readText reads a text file whole, as text (DecodeText).
func readText(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxTextBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxTextBytes {
		return "", readError{"A single text file can be at most 10 MB."}
	}
	return DecodeText(data), nil
}

// DecodeText is a file's bytes as text, whatever it was saved in: by its
// byte-order mark (UTF-8 or UTF-16, as Windows' Notepad saves "Unicode"), as
// UTF-8 when it is, by the character set an HTML page declares, and otherwise
// as Windows-1252, the usual other encoding of a text file.
func DecodeText(data []byte) string {
	e, _, certain := charset.DetermineEncoding(data, "")
	if !certain && utf8.Valid(data) {
		return strings.TrimPrefix(string(data), "\uFEFF")
	}
	if !certain {
		// No byte-order mark, and not UTF-8: the charset a page declares, or
		// Windows-1252.
		e, _, _ = charset.DetermineEncoding(data, "text/html")
	}
	text, err := e.NewDecoder().Bytes(data)
	if err != nil {
		return strings.ToValidUTF8(string(data), "\uFFFD")
	}
	return strings.TrimPrefix(string(text), "\uFEFF")
}

// fileTitle is a title from a file's name: its base name without the
// extension.
func fileTitle(filename string) string {
	base := path.Base(strings.ReplaceAll(filename, "\\", "/"))
	return strings.TrimSuffix(base, path.Ext(base))
}

// titleFromBody takes a doc's title from a heading 1 it starts with (an
// exported doc's first line is its title), leaving the rest as the body.
func titleFromBody(body *contentcodec.Node, fallback string) (string, *contentcodec.Node) {
	if body != nil && len(body.Content) > 1 {
		first := body.Content[0]
		if first.Type == "heading" {
			if level, _ := first.Attrs["level"].(float64); level == 1 {
				if t := strings.TrimSpace(contentcodec.PlainText(first)); t != "" {
					rest := &contentcodec.Node{Type: "doc", Content: body.Content[1:]}
					return t, contentcodec.Normalize(rest, nil)
				}
			}
		}
	}
	return fallback, body
}

type markdownImporter struct{}

func (markdownImporter) Format() string { return "md" }

func (markdownImporter) Extensions() []string { return []string{".md", ".markdown"} }

func (markdownImporter) Accepts(filename, mediaType string) bool {
	return hasExt(filename, ".md", ".markdown", ".mdown", ".mkd") || baseMediaType(mediaType) == "text/markdown"
}

func (markdownImporter) Import(_ context.Context, in Input, out Output) error {
	text, err := readText(in.Reader())
	if err != nil {
		return err
	}
	return out.Page(markdownPage(in.Filename, text, nil))
}

// markdownPage is a Markdown file as a page. One the reader won't read
// (contentcodec.ErrTooCostly: nested too deep, or too slow to read) comes in
// as plain text, every word kept.
func markdownPage(key, text string, assets AssetOpener) Page {
	body, err := contentcodec.FromMarkdown(text, nil)
	if err != nil || body == nil {
		return Page{Key: key, Title: fileTitle(key), Body: contentcodec.FromText(text), Assets: assets}
	}
	title, body := titleFromBody(body, fileTitle(key))
	return Page{Key: key, Title: title, Body: body, Assets: assets}
}

type textImporter struct{}

func (textImporter) Format() string { return "txt" }

func (textImporter) Extensions() []string { return []string{".txt"} }

func (textImporter) Accepts(filename, mediaType string) bool {
	return hasExt(filename, ".txt", ".text") || baseMediaType(mediaType) == "text/plain" && path.Ext(filename) == ""
}

func (textImporter) Import(_ context.Context, in Input, out Output) error {
	text, err := readText(in.Reader())
	if err != nil {
		return err
	}
	return out.Page(textPage(in.Filename, text))
}

func textPage(key, text string) Page {
	return Page{Key: key, Title: fileTitle(key), Body: contentcodec.FromText(text)}
}

type htmlImporter struct{}

func (htmlImporter) Format() string { return "html" }

func (htmlImporter) Extensions() []string { return []string{".html", ".htm"} }

func (htmlImporter) Accepts(filename, mediaType string) bool {
	return hasExt(filename, ".html", ".htm", ".xhtml") || baseMediaType(mediaType) == "text/html"
}

func (htmlImporter) Import(_ context.Context, in Input, out Output) error {
	text, err := readText(in.Reader())
	if err != nil {
		return err
	}
	return out.Page(htmlPage(in.Filename, text, nil))
}

// htmlPage reads an HTML file as a page: its <title>, or the heading 1 it
// starts with, as the title; its body as foreign HTML.
func htmlPage(key, text string, assets AssetOpener) Page {
	body, _ := contentcodec.FromHTML(text, contentcodec.HTMLOptions{Foreign: true})
	title, body := titleFromBody(body, "")
	if title == "" {
		title = htmlTitle(text)
	}
	if title == "" {
		title = fileTitle(key)
	}
	return Page{Key: key, Title: title, Body: body, Assets: assets}
}

// htmlTitle is the text of a document's <title>, if it has one.
func htmlTitle(src string) string {
	z := html.NewTokenizer(strings.NewReader(src))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken:
			name, _ := z.TagName()
			if string(name) == "title" {
				if z.Next() == html.TextToken {
					return strings.Join(strings.Fields(string(z.Text())), " ")
				}
				return ""
			}
			if string(name) == "body" {
				return ""
			}
		}
	}
}
