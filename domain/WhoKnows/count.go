// Package whoKnowsDomain is "Who knows" (DG9) where it touches the stores:
// the one DQL query that counts who wrote, is assigned to and commented on a
// set of items, and the OpenSearch query that finds the items.
//
// WHAT IT IS NOT. It decides nothing about who may see what. The items it
// counts are the ones business/WhoKnows has already decided the asker can
// read (business/EntityLink ReadMany, the chips' and the work graph's rule),
// and the people it names are filtered there again (opt-outs, the demo's
// list). It takes uuids only, written into the query by dgraphquery.UUIDList,
// never a graph uid and never the asker's words.
package whoKnowsDomain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
)

// Item kinds, as business/EntityLink names them.
const (
	KindDoc     = "doc"
	KindTask    = "task"
	KindMessage = "message" // a channel post
)

// Roles a person has on an item.
const (
	RoleWrote     = "wrote"
	RoleAssigned  = "assigned"
	RoleCommented = "commented"
)

// CommentsPerItem is how many of an item's live comments are read: the
// first, by uid. Counts and evidence read the same ones.
const CommentsPerItem = 50

// MaxPeople bounds the people one read names. The real bound is the items
// (200) times who can be on one (an author, a creator, an assignee and
// CommentsPerItem commenters); this is a safety net, not a ranking.
const MaxPeople = 1000

// Items are the items to count, by kind: uuids.
type Items struct {
	Docs     []string
	Tasks    []string
	Messages []string
}

// Empty is whether there is nothing to count.
func (s Items) Empty() bool {
	return len(dgraphquery.QuotedUUIDs(s.Docs)) == 0 && len(dgraphquery.QuotedUUIDs(s.Tasks)) == 0 && len(dgraphquery.QuotedUUIDs(s.Messages)) == 0
}

// Person is one person the read named, with their counts over the items.
// Email is for the demo's list filter only; it never leaves the server.
type Person struct {
	UUID      string
	Email     string
	Wrote     int // docs they wrote
	Created   int // tasks they created
	Posted    int // channel posts they posted
	Assigned  int
	Commented int
}

// Contribution is one person's role on one item.
type Contribution struct {
	Kind   string
	Item   string
	Person string
	Role   string
}

// Counted is a read's answer: the people (live, no bots, no external
// accounts) and what each did on which item.
type Counted struct {
	People        []Person
	Contributions []Contribution
}

// kindSpec is how the graph names one item kind.
type kindSpec struct {
	kind, root, uuid, deleted, comments string
	authors                             []string // predicates naming who wrote it
	assignee                            string   // "" for none
	// only keeps the items of the kind that can count, "" for every one: of
	// the docs, the pages (docs F1). Making a folder, uploading a file or
	// adding a link isn't writing about anything, and a comment on one isn't
	// either.
	only string
}

var kinds = []kindSpec{
	{kind: KindDoc, root: "D", uuid: "doc_uuid", deleted: "doc_deleted_at", comments: "doc_comments", authors: []string{"doc_created_by"}, only: docDomain.PageOnly},
	{kind: KindTask, root: "T", uuid: "task_uuid", deleted: "task_deleted_at", comments: "task_comments", authors: []string{"task_created_by"}, assignee: "task_assignee"},
	{kind: KindMessage, root: "P", uuid: "post_uuid", deleted: "post_deleted_at", comments: "post_comments", authors: []string{"post_by"}},
}

// keep is the filter on the kind's items: the live ones that can count.
func (k kindSpec) keep() string {
	if k.only == "" {
		return dgraphquery.Live(k.deleted)
	}
	return dgraphquery.Live(k.deleted) + " AND " + k.only
}

func (s Items) of(kind string) []string {
	switch kind {
	case KindDoc:
		return s.Docs
	case KindTask:
		return s.Tasks
	case KindMessage:
		return s.Messages
	}
	return nil
}

// PersonFilter keeps people who may be named: live, not a bot and not an
// external account (bots are external by class; both flags are checked).
var PersonFilter = "has(user_uuid) AND " + dgraphquery.Live("user_deleted_at") + " AND NOT eq(is_bot, true) AND NOT eq(is_external, true)"

// CountQuery is the read for items: "" when there is nothing to read. One
// query: each kind's live items by uuid, their live comments, a @groupby per
// role into value variables keyed by the person, one block reading those
// values for the people who may be named, and evidence blocks listing each
// item's people over the same edges.
func CountQuery(s Items) string {
	var b strings.Builder
	var vars, comments []string
	b.WriteString("{\n")
	for _, k := range kinds {
		list := dgraphquery.UUIDList(s.of(k.kind))
		if list == "" {
			continue
		}
		fmt.Fprintf(&b, "\t%s as var(func: eq(%s, [%s])) @filter(%s)\n", k.root, k.uuid, list, k.keep())
		fmt.Fprintf(&b, "\tvar(func: uid(%s)) { %sC as %s @filter(%s) (first: %d) }\n", k.root, k.root, k.comments, dgraphquery.Live("comment_deleted_at"), CommentsPerItem)
		comments = append(comments, k.root+"C")
		for i, a := range k.authors {
			v := fmt.Sprintf("w%s%d", k.root, i)
			fmt.Fprintf(&b, "\tvar(func: uid(%s)) @groupby(%s) { %s as count(uid) }\n", k.root, a, v)
			vars = append(vars, v)
		}
		if k.assignee != "" {
			v := "a" + k.root
			fmt.Fprintf(&b, "\tvar(func: uid(%s)) @groupby(%s) { %s as count(uid) }\n", k.root, k.assignee, v)
			vars = append(vars, v)
		}
	}
	if len(vars) == 0 {
		return ""
	}
	fmt.Fprintf(&b, "\tvar(func: uid(%s)) @groupby(comment_by) { cm as count(uid) }\n", strings.Join(comments, ", "))
	vars = append(vars, "cm")
	fmt.Fprintf(&b, "\twho(func: uid(%s), first: %d) @filter(%s) {\n\t\tuser_uuid\n\t\tuser_email_id\n", strings.Join(vars, ", "), MaxPeople, PersonFilter)
	for _, v := range vars {
		fmt.Fprintf(&b, "\t\t%s: val(%s)\n", v, v)
	}
	b.WriteString("\t}\n")
	for _, k := range kinds {
		if dgraphquery.UUIDList(s.of(k.kind)) == "" {
			continue
		}
		fmt.Fprintf(&b, "\tev_%s(func: uid(%s)) {\n\t\t%s\n", k.kind, k.root, k.uuid)
		for _, a := range k.authors {
			fmt.Fprintf(&b, "\t\t%s { user_uuid }\n", a)
		}
		if k.assignee != "" {
			fmt.Fprintf(&b, "\t\t%s { user_uuid }\n", k.assignee)
		}
		fmt.Fprintf(&b, "\t\t%s @filter(%s) (first: %d) { comment_by { user_uuid } }\n\t}\n", k.comments, dgraphquery.Live("comment_deleted_at"), CommentsPerItem)
	}
	b.WriteString("}")
	return b.String()
}

// read runs a query (a seam for tests).
var read = func(ctx context.Context, q string) (map[string]json.RawMessage, error) {
	return dgexec.Read[map[string]json.RawMessage](ctx, q, nil)
}

// Count is who did what on items: one query. Nothing to count is no query.
func Count(ctx context.Context, s Items) (*Counted, error) {
	q := CountQuery(s)
	if q == "" {
		return &Counted{}, nil
	}
	raw, err := read(ctx, q)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/WhoKnows Count: %+v", err)
		return nil, err
	}
	return parseCounted(raw)
}

func parseCounted(raw map[string]json.RawMessage) (*Counted, error) {
	var who []map[string]any
	if b := raw["who"]; len(b) > 0 {
		if err := json.Unmarshal(b, &who); err != nil {
			return nil, fmt.Errorf("who knows: who: %w", err)
		}
	}
	out := &Counted{}
	named := map[string]bool{}
	for _, w := range who {
		id, _ := w["user_uuid"].(string)
		if id == "" {
			continue
		}
		p := Person{UUID: strings.ToLower(id)}
		p.Email, _ = w["user_email_id"].(string)
		for key, v := range w {
			n, ok := v.(float64)
			if !ok {
				continue
			}
			switch {
			case key == "cm":
				p.Commented += int(n)
			case strings.HasPrefix(key, "wD"):
				p.Wrote += int(n)
			case strings.HasPrefix(key, "wT"):
				p.Created += int(n)
			case strings.HasPrefix(key, "wP"):
				p.Posted += int(n)
			case strings.HasPrefix(key, "a"):
				p.Assigned += int(n)
			}
		}
		named[p.UUID] = true
		out.People = append(out.People, p)
	}
	for _, k := range kinds {
		b := raw["ev_"+k.kind]
		if len(b) == 0 {
			continue
		}
		var items []map[string]any
		if err := json.Unmarshal(b, &items); err != nil {
			return nil, fmt.Errorf("who knows: ev_%s: %w", k.kind, err)
		}
		for _, it := range items {
			item, _ := it[k.uuid].(string)
			if item == "" {
				continue
			}
			item = strings.ToLower(item)
			add := func(person any, role string) {
				for _, n := range asNodes(person) {
					id, _ := n["user_uuid"].(string)
					if id = strings.ToLower(id); id != "" && named[id] {
						out.Contributions = append(out.Contributions, Contribution{Kind: k.kind, Item: item, Person: id, Role: role})
					}
				}
			}
			for _, a := range k.authors {
				add(it[a], RoleWrote)
			}
			if k.assignee != "" {
				add(it[k.assignee], RoleAssigned)
			}
			for _, c := range asNodes(it[k.comments]) {
				add(c["comment_by"], RoleCommented)
			}
		}
	}
	return out, nil
}

// asNodes is a block's nodes: a list, or one node for a single uid edge.
func asNodes(v any) []map[string]any {
	switch t := v.(type) {
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, x := range t {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{t}
	}
	return nil
}
