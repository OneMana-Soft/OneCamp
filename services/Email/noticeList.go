package email

import (
	"fmt"
	"html/template"
	"strings"
)

// NoticeList is one email standing for several notices to one person: each
// as a line, newest first, and how many more it stands for than it lists.
// Every field is text (RenderNoticeList escapes each); no caller hands it HTML.
type NoticeList struct {
	TemplateData
	Items []NoticeItem
	More  int
}

// NoticeItem is one notice in a NoticeList: what happened, where, the words
// it quotes and the link to it.
type NoticeItem struct {
	Title, Place, Snippet, Link string
}

// noticeListBody is the list's part of its email, the shell's "body": written
// from the fields alone, so html/template escapes each in its place (a link
// that isn't a web address is written as "#ZgotmplZ").
const noticeListBody = `{{define "body"}}` +
	`{{range .Items}}<div style="margin-top:12px;">` +
	`{{if .Link}}<a href="{{.Link}}" style="color:inherit;"><strong>{{.Title}}</strong></a>{{else}}<strong>{{.Title}}</strong>{{end}}` +
	`{{with .Place}} · {{.}}{{end}}{{with .Snippet}}<div>{{.}}</div>{{end}}</div>{{end}}` +
	`{{if .More}}<div style="margin-top:12px;">and {{.More}} more</div>{{end}}` +
	`{{end}}`

// noticeListTpl is the shell with the list's body in it.
var noticeListTpl = template.Must(template.Must(baseTpl.Clone()).Parse(noticeListBody))

// RenderNoticeList is Render for a NoticeList: its HTML, every field escaped
// by the template, and its plain text, saying them as written.
func RenderNoticeList(d NoticeList) (string, string, error) {
	d.TemplateData = d.withDefaults()
	return renderInShell(noticeListTpl, d, d.TemplateData, d.plainBody(), "notice list")
}

// plainBody is the list as its plain-text part says it, in the body's place:
// each notice's title and place, its words and its link, a blank line
// between notices, then how many more. Pure.
func (d NoticeList) plainBody() string {
	var b strings.Builder
	for i, it := range d.Items {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(it.Title)
		if it.Place != "" {
			b.WriteString(" · " + it.Place)
		}
		b.WriteString("\n")
		for _, l := range []string{it.Snippet, it.Link} {
			if l != "" {
				b.WriteString(l + "\n")
			}
		}
	}
	if d.More > 0 {
		fmt.Fprintf(&b, "\nand %d more\n", d.More)
	}
	return strings.TrimSuffix(b.String(), "\n")
}
