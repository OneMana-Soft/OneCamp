package contentcodec

// The codec's rules for connected work (docs programme C1): a page in a doc
// (subpage), a date in a line (dateMention), a project's tasks or board
// (taskListEmbed, boardEmbed), and a to-do's task (taskId on taskItem). The
// same rules, in the same order, as the web's lib/codec/connected.ts; the
// golden fixture testdata/codec/connected.* holds the two to the same bytes.
//
// In HTML each is what the editor renders: ids and the date as attributes,
// and a plain link or the date's words inside, for a reader that knows
// nothing of them. In Markdown each is that link, or the date's words.

import (
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"golang.org/x/net/html"
)

var (
	taskListShows = map[string]bool{"open": true, "mine": true, "all": true}
	// connectedDataTypes is each connected block's data-type in HTML, by node.
	connectedDataTypes = map[string]string{"subpage": "subpage", "taskListEmbed": "task-list-embed", "boardEmbed": "board-embed"}
	// embedNodes is each project embed's node, by its data-type.
	embedNodes = map[string]string{"task-list-embed": "taskListEmbed", "board-embed": "boardEmbed"}
)

// connectedID is v as an id: a uuid in lower case, or "".
func connectedID(v any) string {
	s, _ := v.(string)
	if !helpers.IsCanonicalUUID(s) {
		return ""
	}
	return strings.ToLower(s)
}

// dayOf is v as a day on the calendar, written "2026-10-12": "2026-02-30" is
// none, and nor is anything else.
func dayOf(v any) (time.Time, bool) {
	s, _ := v.(string)
	t, err := time.Parse(time.DateOnly, s)
	return t, err == nil
}

// isoDate is v as a day, "2026-10-12", or "".
func isoDate(v any) string {
	if t, ok := dayOf(v); ok {
		return t.Format(time.DateOnly)
	}
	return ""
}

// dateWords is a day as the body writes it, whatever the reader's language:
// "12 Oct 2026".
func dateWords(v any) string {
	if t, ok := dayOf(v); ok {
		return t.Format("2 Jan 2006")
	}
	return ""
}

func taskListShow(v any) string {
	if s, _ := v.(string); taskListShows[s] {
		return s
	}
	return "open"
}

func init() {
	nodeSpecs["subpage"] = &nodeSpec{name: "subpage", attrs: []attrSpec{{"docId", nil}, {"title", nil}}}
	nodeSpecs["dateMention"] = &nodeSpec{name: "dateMention", inline: true, attrs: []attrSpec{{"date", nil}, {"reminder", nil}}}
	nodeSpecs["taskListEmbed"] = &nodeSpec{name: "taskListEmbed", attrs: []attrSpec{{"projectId", nil}, {"show", "open"}}}
	nodeSpecs["boardEmbed"] = &nodeSpec{name: "boardEmbed", attrs: []attrSpec{{"projectId", nil}}}
}

// connectedAttrs is the to-do's task, on the to-do.
var connectedAttrs = []globalAttr{{
	name: "taskId", html: "data-task-id",
	on:    []string{"taskItem"},
	typed: func(v any) any { return orNil(connectedID(v)) },
	read:  func(raw string, _ bool) any { return orNil(connectedID(raw)) },
	write: func(v any) string { return connectedID(v) },
}}

// typedConnectedAttr is a connected node's attribute as the editor's schema
// types it.
func typedConnectedAttr(typ, name string, v any) (any, bool) {
	switch {
	case typ == "subpage" && name == "docId", (typ == "taskListEmbed" || typ == "boardEmbed") && name == "projectId":
		return orNil(connectedID(v)), true
	case typ == "dateMention" && name == "date":
		return orNil(isoDate(v)), true
	case typ == "taskListEmbed" && name == "show":
		return taskListShow(v), true
	}
	return nil, false
}

// connectedLink is the app's address and the words a block stands for.
func connectedLink(n *Node) (path, words string) {
	switch n.Type {
	case "subpage":
		if id := connectedID(n.attr("docId")); id != "" {
			return "/app/doc/" + id, helpers.FirstNonEmpty(n.attrString("title"), "Page")
		}
		return "", helpers.FirstNonEmpty(n.attrString("title"), "Page")
	case "taskListEmbed":
		if id := connectedID(n.attr("projectId")); id != "" {
			return "/app/project/" + id, "Tasks"
		}
		return "", "Tasks"
	case "boardEmbed":
		if id := connectedID(n.attr("projectId")); id != "" {
			return "/app/project/" + id, "Board"
		}
		return "", "Board"
	}
	return "", ""
}

// writeConnectedBlock writes a connected block's HTML, or answers false for
// any other node.
func writeConnectedBlock(b *strings.Builder, n *Node) bool {
	dataType := connectedDataTypes[n.Type]
	if dataType == "" {
		return false
	}
	b.WriteString(`<div data-type="` + dataType + `"`)
	switch n.Type {
	case "subpage":
		if id := connectedID(n.attr("docId")); id != "" {
			writeAttr(b, "data-doc-id", id)
		}
		if t := n.attrString("title"); t != "" {
			writeAttr(b, "data-title", t)
		}
	default:
		if id := connectedID(n.attr("projectId")); id != "" {
			writeAttr(b, "data-project-id", id)
		}
		if n.Type == "taskListEmbed" {
			writeAttr(b, "data-show", taskListShow(n.attr("show")))
		}
	}
	b.WriteString(">")
	if path, words := connectedLink(n); path != "" {
		b.WriteString(`<a href="` + escapeAttr(path) + `">` + escapeText(words) + `</a>`)
	}
	b.WriteString("</div>")
	return true
}

// writeDateMention writes a date in a line.
func writeDateMention(b *strings.Builder, n *Node) {
	b.WriteString(`<time data-type="date-mention"`)
	if d := isoDate(n.attr("date")); d != "" {
		writeAttr(b, "datetime", d)
	}
	if r := n.attrString("reminder"); r != "" {
		writeAttr(b, "data-reminder", r)
	}
	b.WriteString(">" + escapeText(dateWords(n.attr("date"))) + "</time>")
}

// readConnectedBlock is a connected block read from its div, or nil for any
// other div.
func readConnectedBlock(dataType string, n *html.Node) *Node {
	switch dataType {
	case "subpage":
		m := newNode("subpage")
		if id := connectedID(attrVal(n, "data-doc-id")); id != "" {
			m.setAttr("docId", id)
		}
		if t := attrVal(n, "data-title"); t != "" {
			m.setAttr("title", t)
		}
		return m
	case "task-list-embed", "board-embed":
		m := newNode(embedNodes[dataType])
		if id := connectedID(attrVal(n, "data-project-id")); id != "" {
			m.setAttr("projectId", id)
		}
		if m.Type == "taskListEmbed" {
			m.setAttr("show", taskListShow(attrVal(n, "data-show")))
		}
		return m
	}
	return nil
}

// readDateMention is a date in a line, read from its <time>.
func readDateMention(n *html.Node) *Node {
	m := newNode("dateMention")
	if d := isoDate(attrVal(n, "datetime")); d != "" {
		m.setAttr("date", d)
	}
	if r := attrVal(n, "data-reminder"); r != "" {
		m.setAttr("reminder", r)
	}
	return m
}

// connectedMarkdown is a connected block in Markdown: its link, or its words.
func (w *mdWriter) connectedMarkdown(n *Node) []string {
	path, words := connectedLink(n)
	if path == "" {
		return []string{escapeMarkdownText(words, true)}
	}
	return []string{"[" + escapeLinkText(words) + "](" + linkDest(w.appURL(path)) + ")"}
}
