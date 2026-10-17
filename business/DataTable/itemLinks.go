package business

// Relation fields that link rows to OneCamp items: docs, tasks, boards,
// projects and people (relation_target "doc", "task", "board", "project",
// "user", or "any" for all of them).
//
// A link was stored as {id, label, type}, with the label copied when the link
// was made, and every reader of the table saw that label, and so did anyone
// with the table's guest link, whatever they could open: a private doc's
// title, to the whole workspace (TAB-BUG-5). A title edited later was never
// shown, and a member writing through the API could put any label beside any
// id.
//
// Now a row keeps [{id, type}] only, and each read names the items for the
// person reading, a batch of each kind at a time, by that kind's own rule:
//   - a doc by the doc access rule (business/Doc CanRead, the Access
//     Resolver's rule where there is one);
//   - a board by business/Board CanRead;
//   - a task by business/Task CanViewTask, unless it or its project is
//     deleted;
//   - a project by being in it, unless it's deleted;
//   - a person by being in the workspace.
//
// An item the reader can't open, that's gone, or that's past what one read
// names comes back {id, type, hidden: true}, so the web keeps it when it
// writes the cell back. A guest's read names people, without their ids,
// which are members', and leaves every other item out.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	"github.com/akashc777/OneCamp/business/DataTable/formula"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// The kinds of item a relation field can link to; itemAny is all of them.
const (
	itemDoc     = "doc"
	itemTask    = "task"
	itemBoard   = "board"
	itemProject = "project"
	itemUser    = "user"
	itemAny     = "any"
)

// itemKinds is each kind of item, as a refusal names many of them.
var itemKinds = map[string]string{
	itemDoc: "docs", itemTask: "tasks", itemBoard: "boards", itemProject: "projects", itemUser: "people",
}

// untitled is what an item with no title of its own is called.
var untitled = map[string]string{
	itemDoc: "Untitled doc", itemTask: "Untitled task", itemBoard: "Untitled board", itemProject: "Untitled project", itemUser: "Someone",
}

const (
	// maxReadItems is how many different items one read names; those past
	// it come back hidden.
	maxReadItems = 10000
	// itemBatch is how many items of a kind one lookup names.
	itemBatch = 500
	// maxQuoted is how much of a refused link a refusal quotes: a cell may
	// be 100 KiB.
	maxQuoted = 80
)

// itemRef is a link to an item: {id, type} as stored, and as read with the
// item's title, or hidden when the reader can't open it.
type itemRef struct {
	ID     string `json:"id,omitempty"`
	Type   string `json:"type"`
	Label  string `json:"label,omitempty"`
	Hidden bool   `json:"hidden,omitempty"`
}

// itemsOf is what a relation field links to when it links to OneCamp items:
// one kind, or itemAny; ok is false for any other field.
func itemsOf(f *model.Field) (target string, ok bool) {
	if f == nil || f.Type != model.FieldRelation {
		return "", false
	}
	var c relationConfig
	_ = json.Unmarshal([]byte(f.Config), &c)
	switch {
	case c.Target == linksTable:
		return "", false
	case itemKinds[c.Target] != "":
		return c.Target, true
	}
	return itemAny, true
}

// itemField is a relation field linking to items, and the kind it links to.
type itemField struct {
	field  *model.Field
	target string
}

// readsLinks is whether a field's cells are worked out for each reader from
// what they link to: a table's rows, or OneCamp items. Such a cell never
// names a row, and no rollup adds it up: a read of another table works out
// neither for its reader.
func readsLinks(f *model.Field) bool {
	if _, isLink := linkOf(f); isLink {
		return true
	}
	_, isItems := itemsOf(f)
	return isItems
}

// parseItems reads the links in a cell, as stored or as written: each item's
// {id, type} once, in order, and nothing else stored with it (a label copied
// when it was linked is never read). A type left out is the field's kind; a
// read's count of links past those shown ({"type": "more"}) is passed over.
// bad is the first item that isn't a link to an item the field links to, as
// it was given.
func parseItems(raw interface{}, target string) (refs []itemRef, bad string) {
	list, isList := raw.([]interface{})
	if !isList {
		if raw == nil {
			return nil, ""
		}
		list = []interface{}{raw}
	}
	refs = make([]itemRef, 0, len(list))
	seen := map[string]bool{}
	for _, it := range list {
		var id, typ string
		switch v := it.(type) {
		case map[string]interface{}:
			if v["type"] == "more" {
				continue
			}
			id, _ = v["id"].(string)
			typ, _ = v["type"].(string)
		case string:
			id = v
		}
		if typ == "" && target != itemAny {
			typ = target
		}
		parsed, err := uuid.Parse(strings.TrimSpace(id))
		if err != nil || itemKinds[typ] == "" || (target != itemAny && typ != target) {
			if bad == "" {
				b, _ := json.Marshal(it)
				bad = string(b)
			}
			continue
		}
		key := typ + "/" + parsed.String()
		if !seen[key] {
			seen[key] = true
			refs = append(refs, itemRef{ID: parsed.String(), Type: typ})
		}
	}
	return refs, bad
}

// keepItemIDs reduces each cell written to a field linking to items to its
// links' {id, type}, so no label written with one is ever stored or shown.
// A cell holding anything that isn't such a link is refused; an empty one
// clears.
func keepItemIDs(fields []*model.Field, values map[string]interface{}) error {
	for _, f := range fields {
		target, ok := itemsOf(f)
		raw, present := values[f.Id.String()]
		if !ok || !present || raw == nil || raw == "" {
			continue
		}
		refs, bad := parseItems(raw, target)
		if bad != "" {
			kinds := itemKinds[target]
			if target == itemAny {
				kinds = "docs, tasks, boards, projects or people"
			}
			if r := []rune(bad); len(r) > maxQuoted {
				bad = string(r[:maxQuoted]) + "…"
			}
			return fmt.Errorf("%q links to %s by id and type: %s isn't one", f.Name, kinds, bad)
		}
		values[f.Id.String()] = refs
	}
	return nil
}

// itemReader is who a read names items for, by uuid, with their graph uid
// once a lookup needs it.
type itemReader struct {
	uuid   string
	uid    string
	looked bool
}

// graphUID is the reader's graph uid ("" for no one active), looked up once.
func (r *itemReader) graphUID(ctx context.Context) (string, error) {
	if !r.looked {
		u, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, r.uuid)
		if err != nil {
			return "", err
		}
		r.looked = true
		if u != nil {
			r.uid = u.Uid
		}
	}
	return r.uid, nil
}

// itemNamers name the items of each kind that a reader may open now: each
// one's title, by lower-case id. A seam for tests.
var itemNamers = map[string]func(ctx context.Context, ids []string, who *itemReader) (map[string]string, error){
	itemDoc: func(ctx context.Context, ids []string, who *itemReader) (map[string]string, error) {
		return docBusiness.ReadableDocTitles(ctx, ids, who.uuid)
	},
	itemBoard: func(ctx context.Context, ids []string, who *itemReader) (map[string]string, error) {
		return boardBusiness.ReadableBoardTitles(ctx, ids, who.uuid)
	},
	itemTask: func(ctx context.Context, ids []string, who *itemReader) (map[string]string, error) {
		uid, err := who.graphUID(ctx)
		if err != nil {
			return nil, err
		}
		return taskBusiness.ReadableTaskTitles(ctx, ids, uid)
	},
	itemProject: projectTitles,
	itemUser:    peopleNames,
}

// projectTitles names the projects a reader is in, by projectDomain's
// progress read, which says whether they're in each and whether it's
// archived.
func projectTitles(ctx context.Context, ids []string, who *itemReader) (map[string]string, error) {
	uid, err := who.graphUID(ctx)
	if err != nil || uid == "" {
		return map[string]string{}, err
	}
	parsed := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if p, err := uuid.Parse(id); err == nil {
			parsed = append(parsed, p)
		}
	}
	got, err := projectDomain.GetDgraphProjectsProgress(ctx, parsed, uid, time.Now())
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(got))
	for id, p := range got {
		if p.Visible() && !p.Archived() {
			out[strings.ToLower(id)] = p.Name
		}
	}
	return out, nil
}

// peopleNames names the people in the workspace, whoever reads.
func peopleNames(ctx context.Context, ids []string, _ *itemReader) (map[string]string, error) {
	got, err := userDomain.ResolveUserDisplays(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(got))
	for id, u := range got {
		out[strings.ToLower(id)] = u.Display
	}
	return out, nil
}

// itemNames is how one read names the items its cells link to.
type itemNames struct {
	// titles is the items the reader may open, by kind, then lower-case id.
	titles map[string]map[string]string
	// guest is a read for no member, which names people only.
	guest bool
}

// nameItems names the items a read's cells link to, wanted, for its reader:
// the first maxReadItems different ones, a kind at a time, a batch of
// itemBatch at a time. An item a lookup couldn't name stays hidden.
func nameItems(ctx context.Context, wanted []itemRef) itemNames {
	a, member := ctx.Value(viewerKey{}).(Actor)
	n := itemNames{titles: map[string]map[string]string{}, guest: !member}
	byKind := map[string][]string{}
	asked := map[string]bool{}
	for _, r := range wanted {
		key := r.Type + "/" + r.ID
		if asked[key] || len(asked) == maxReadItems || (n.guest && r.Type != itemUser) {
			continue
		}
		asked[key] = true
		byKind[r.Type] = append(byKind[r.Type], r.ID)
	}
	readers := []*itemReader{{uuid: a.UserID.String()}}
	for kind, ids := range byKind {
		askers := readers
		if kind == itemUser {
			askers = readers[:1] // people are named alike for everyone
		}
		var titles map[string]string
		for i, who := range askers {
			got, err := namesFor(ctx, kind, ids, who)
			if err != nil {
				helpers.LogErrorWithContext(ctx, "business/DataTable could not name the %s a row links to, leaving them hidden: %v", itemKinds[kind], err)
				titles = nil
				break
			}
			if i == 0 {
				titles = got
				continue
			}
			for id := range titles {
				if _, both := got[id]; !both {
					delete(titles, id)
				}
			}
		}
		n.titles[kind] = titles
	}
	return n
}

// namesFor names items of one kind for a reader, a batch at a time.
func namesFor(ctx context.Context, kind string, ids []string, who *itemReader) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	for from := 0; from < len(ids); from += itemBatch {
		got, err := itemNamers[kind](ctx, ids[from:min(from+itemBatch, len(ids))], who)
		if err != nil {
			return nil, err
		}
		for id, title := range got {
			out[id] = title
		}
	}
	return out, nil
}

// cell is a cell's links as its reader gets them, and the value formulas
// read for it: the titles of the items the reader may open, in order. A
// guest's names people alone, without their ids, which are members': it
// leaves out every other item, and anyone it can't name.
func (n itemNames) cell(refs []itemRef) ([]itemRef, formula.Value) {
	out := make([]itemRef, 0, len(refs))
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		if n.guest && r.Type != itemUser {
			continue
		}
		title, ok := n.titles[r.Type][strings.ToLower(r.ID)]
		if !ok {
			if !n.guest {
				out = append(out, itemRef{ID: r.ID, Type: r.Type, Hidden: true})
			}
			continue
		}
		if strings.TrimSpace(title) == "" {
			title = untitled[r.Type]
		}
		named := itemRef{ID: r.ID, Type: r.Type, Label: title}
		if n.guest {
			named.ID = ""
		}
		out = append(out, named)
		names = append(names, title)
	}
	return out, formula.Text(strings.Join(names, ", "))
}

// itemLinkRepairKey records in the system config that every link to an item
// stored in a row has been reduced to its id and kind (RepairItemLinks).
const itemLinkRepairKey = "tables_item_links_ids_only"

// itemLinkRepairRows is how many rows one statement of the repair reads.
const itemLinkRepairRows = 500

// RepairItemLinks reduces every link to an item stored in any row to its
// {id, type}, once per install, and answers how many rows it changed. Links
// were stored with the item's title as it was when linked (TAB-BUG-5). Each
// read now names them for its reader, but a cell no read names still gave
// the title out as it was stored: a deleted field's, a field's changed to
// another type before that was refused, and any a filter's search matches.
func RepairItemLinks(ctx context.Context) (int, error) {
	if done, err := configModel.IsDone(itemLinkRepairKey); err != nil || done {
		return 0, err
	}
	kinds := make([]string, 0, len(itemKinds))
	for kind := range itemKinds {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	changed := 0
	for after := uuid.Nil; ; {
		last, n, err := model.StripItemLinks(ctx, kinds, after, itemLinkRepairRows)
		if err != nil {
			return changed, err
		}
		changed += n
		if last == uuid.Nil {
			break
		}
		after = last
	}
	return changed, configModel.UpsertConfig(itemLinkRepairKey, fmt.Sprintf("done: %d rows", changed))
}

func init() {
	// Hourly, claimed by one worker at a time, until it's done once; a pass
	// that fails is tried again the next hour.
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "tables.item-link-repair", Every: time.Hour, Run: func(ctx context.Context) error {
		changed, err := RepairItemLinks(ctx)
		if changed > 0 {
			helpers.MessageLogs.InfoLog.Printf("tables: links to items in %d rows reduced to their ids", changed)
		}
		return err
	}})
}
