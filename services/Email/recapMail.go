package email

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
)

// RecapMail is a call's recap as it is emailed to a meeting's guests
// (business/Calendar): the email's shell (TemplateData, whose Body it never
// renders), the recap's words a line each, and its action items. Each of them
// is a plain string as the model wrote it, which anyone on the call can steer,
// for guests outside the workspace: the template alone writes them, and
// html/template escapes every one (RenderRecap). No caller hands it HTML.
type RecapMail struct {
	TemplateData
	Lines []RecapLine
	Items []RecapMailItem
}

// RecapLine is one line of a recap's words: a section's heading, or a line.
type RecapLine struct {
	Text    string
	Heading bool
}

// RecapMailItem is one of a recap's action items: what, who when it names
// them, and by when when it names a date, as the recap says them.
type RecapMailItem struct {
	Text, Owner, Due string
}

// About is who and by when, as the email says them after the item's words:
// "Maya, due 2026-11-14"; "" for an item that names neither. Pure.
func (it RecapMailItem) About() string {
	var about []string
	if it.Owner != "" {
		about = append(about, it.Owner)
	}
	if it.Due != "" {
		about = append(about, "due "+it.Due)
	}
	return strings.Join(about, ", ")
}

// recapBody is the recap's part of its email, the shell's "body": written
// from the fields alone, with no function that trusts a string as HTML, so
// html/template escapes each in its place.
const recapBody = `{{define "body"}}` +
	`{{range .Lines}}{{if .Heading}}<div style="margin-top:12px;"><strong>{{.Text}}</strong></div>{{else}}<div>{{.Text}}</div>{{end}}{{end}}` +
	`{{if .Items}}<div style="margin-top:12px;"><strong>Action items</strong></div>` +
	`{{range .Items}}<div>• {{.Text}}{{with .About}} ({{.}}){{end}}</div>{{end}}{{end}}` +
	`{{end}}`

// recapTpl is the shell with the recap's body in it.
var recapTpl = template.Must(template.Must(baseTpl.Clone()).Parse(recapBody))

// RenderRecap is Render for a recap's email: its HTML, every line and item
// escaped by the template, and its plain text, saying them as written.
func RenderRecap(d RecapMail) (string, string, error) {
	d.TemplateData = d.withDefaults()
	return renderInShell(recapTpl, d, d.TemplateData, d.plainBody(), "recap")
}

// renderInShell is Render for an email with a body of its own: tpl (the
// shell with its "body") run over d, and td's plain text with plainBody in
// the body's place.
func renderInShell(tpl *template.Template, d any, td TemplateData, plainBody, what string) (string, string, error) {
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, d); err != nil {
		return "", "", fmt.Errorf("render %s html: %w", what, err)
	}
	td.Body, td.BodyHTML = plainBody, ""
	return buf.String(), renderPlainText(td), nil
}

// plainBody is the recap as its plain-text part says it, in the body's place:
// its lines, a blank line before each heading, then its action items, each as
// written; "" for none. Pure.
func (d RecapMail) plainBody() string {
	var b strings.Builder
	for _, l := range d.Lines {
		if l.Heading && b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(l.Text + "\n")
	}
	if len(d.Items) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Action items\n")
		for _, it := range d.Items {
			b.WriteString("• " + it.Text)
			if about := it.About(); about != "" {
				b.WriteString(" (" + about + ")")
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}
