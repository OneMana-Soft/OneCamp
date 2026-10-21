// Package bodyrefs reads what a body links to: the OneCamp docs, tasks and
// projects a message, a comment or a doc names, and the people it @mentions.
// It is the one parser of the formats the web app writes, so a doc's
// backlinks (doc_mentions, business/Doc docMentions.go) and the work graph's
// references (refers_to, business/Refs) read a body the same way.
//
// WHAT COUNTS AS A LINK.
//   - A "+" chip: <span data-type="entityMention" data-id="<kind>:<uuid>">
//     (components/minimal-tiptap/extensions/reference-mention on the web), for
//     the kinds doc, task and project.
//   - A link: <a href=".../app/doc/<uuid>"> (and /app/task/, /app/project/),
//     on this app's host or any (a pasted link keeps its host), a link chip
//     (<a data-type="link-mention">) included.
//   - A link written out in text: ".../app/doc/<uuid>" in a message written as
//     plain text or Markdown (the API, an agent, an import). Marked FromText,
//     so a caller that reads only what the editor makes can leave it out.
//
// WHAT DOESN'T. A to-do's data-task-id: a doc holds a to-do for a task only
// when doc_task_links says so (migration 340), since a pasted line could carry
// anyone's task id. A channel, a board or a person chip: not a reference.
//
// Pure: no store is read, and nothing it returns has been checked to exist or
// to be visible to anyone.
package bodyrefs

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/net/html"
)

// The kinds a body links to.
const (
	KindDoc     = "doc"
	KindTask    = "task"
	KindProject = "project"
)

// Link is one thing a body links to: its kind and uuid (lower case).
type Link struct {
	Kind string
	UUID string
	// FromText is a link written out in the text, not a chip or an <a>.
	FromText bool
}

// Refs is what a body names, each once, in the order it first appears.
type Refs struct {
	// People are the user uuids it @mentions.
	People []string
	// Links are the docs, tasks and projects it links to.
	Links []Link
}

// Of kind is the uuids of Links of that kind; with markup only when
// markupOnly (a chip or an <a>, not text).
func (r Refs) Of(kind string, markupOnly bool) []string {
	var out []string
	for _, l := range r.Links {
		if l.Kind == kind && (!markupOnly || !l.FromText) {
			out = append(out, l.UUID)
		}
	}
	return out
}

// pathLink is a link to a doc, a task or a project, in an href or in text.
var pathLink = regexp.MustCompile(`/app/(doc|task|project)/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)

// Scan is what body names. body is the HTML the editor writes, or plain text
// or Markdown, which reads as one text token.
func Scan(body string) Refs {
	var r Refs
	seenPerson := map[string]bool{}
	seenLink := map[string]int{} // kind:uuid -> index in r.Links
	add := func(kind, id string, fromText bool) {
		u, err := uuid.Parse(id)
		if err != nil {
			return
		}
		key := kind + ":" + u.String()
		if i, ok := seenLink[key]; ok {
			// Markup wins over text: it is what the editor made.
			if !fromText {
				r.Links[i].FromText = false
			}
			return
		}
		seenLink[key] = len(r.Links)
		r.Links = append(r.Links, Link{Kind: kind, UUID: u.String(), FromText: fromText})
	}
	z := html.NewTokenizer(strings.NewReader(body))
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return r
		case html.TextToken:
			for _, m := range pathLink.FindAllStringSubmatch(string(z.Text()), -1) {
				add(m[1], m[2], true)
			}
			continue
		case html.StartTagToken, html.SelfClosingTagToken:
		default:
			continue
		}
		name, hasAttr := z.TagName()
		if !hasAttr {
			continue
		}
		attrs := map[string]string{}
		for {
			k, v, more := z.TagAttr()
			attrs[string(k)] = string(v)
			if !more {
				break
			}
		}
		switch string(name) {
		case "span":
			switch attrs["data-type"] {
			case "mention":
				// "<user uuid>@<graph uid>"; older chips carry the uuid alone.
				id, _, _ := strings.Cut(attrs["data-id"], "@")
				if u, err := uuid.Parse(id); err == nil && !seenPerson[u.String()] {
					seenPerson[u.String()] = true
					r.People = append(r.People, u.String())
				}
			case "entityMention":
				kind, id, ok := strings.Cut(attrs["data-id"], ":")
				if ok && (kind == KindDoc || kind == KindTask || kind == KindProject) {
					add(kind, id, false)
				}
			}
		case "a":
			if kind, id, ok := AppLink(attrs["href"]); ok {
				add(kind, id, false)
			}
		}
	}
}

// AppLink is the doc, task or project a link opens, for a link to
// /app/<kind>/<uuid> on this app or any. Pure.
func AppLink(href string) (kind, id string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return "", "", false
	}
	for _, k := range []string{KindDoc, KindTask, KindProject} {
		rest, found := strings.CutPrefix(u.Path, "/app/"+k+"/")
		if !found {
			continue
		}
		id, _, _ = strings.Cut(rest, "/")
		if _, err := uuid.Parse(id); err != nil {
			return "", "", false
		}
		return k, id, true
	}
	return "", "", false
}
