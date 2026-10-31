package linear

import (
	"html"
	"strings"
)

// Duplicate and related links (L-2). Linear keeps each link once, as a
// relation from one issue to another, and an issue reads the links that point
// at it (inverseRelations): so each link is seen once, and both ends get its
// line. A "blocks" link is a dependency (BlockedBy); "similar" is Linear's own
// guess, and isn't brought.

// relatedIssue is the other issue of a relation, as the issue query reads it.
type relatedIssue struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	URL        string `json:"url"`
}

// linearRelation is a duplicate or related link that points at an issue.
type linearRelation struct {
	Type  string
	Other relatedIssue
}

// issueRef is an issue a line links to: by its key here when it came too
// (the key finds its task), else by its address in Linear.
type issueRef struct {
	Key, URL string
	Here     bool
}

// linkRelations gives each end of every duplicate and related link its line:
// the duplicate says what it duplicates, the original lists its duplicates,
// and related issues list each other.
func linkRelations(issues []linearIssue, byID map[string]*linearIssue) {
	for i := range issues {
		x := &issues[i]
		here := issueRef{Key: x.Identifier, URL: x.URL, Here: true}
		for _, r := range x.Relations {
			o := byID[r.Other.ID]
			ref := issueRef{Key: r.Other.Identifier, URL: r.Other.URL, Here: o != nil}
			switch r.Type {
			case "duplicate": // the other issue duplicates this one
				x.Duplicates = append(x.Duplicates, ref)
				if o != nil {
					o.DuplicateOf = append(o.DuplicateOf, here)
				}
			case "related":
				x.Related = append(x.Related, ref)
				if o != nil {
					o.Related = append(o.Related, here)
				}
			}
		}
	}
}

// relationLines are an issue's links as lines at the end of its description.
func relationLines(iss linearIssue) string {
	var b strings.Builder
	line := func(label string, refs []issueRef) {
		if len(refs) == 0 {
			return
		}
		b.WriteString("<p><strong>" + label + "</strong> ")
		for i, r := range refs {
			if i > 0 {
				b.WriteString(", ")
			}
			// The description isn't sanitised on import, so a source's
			// address is kept only when it is https.
			href := ""
			if r.Here && r.Key != "" {
				href = "/app/task/" + r.Key
			} else if strings.HasPrefix(strings.ToLower(r.URL), "https://") {
				href = r.URL
			}
			if href == "" {
				b.WriteString(html.EscapeString(r.Key))
				continue
			}
			b.WriteString(`<a href="` + html.EscapeString(href) + `">` + html.EscapeString(r.Key) + "</a>")
		}
		b.WriteString("</p>")
	}
	line("Duplicate of", iss.DuplicateOf)
	line("Duplicates", iss.Duplicates)
	line("Related", iss.Related)
	return b.String()
}
