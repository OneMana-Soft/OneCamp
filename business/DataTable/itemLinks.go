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
// writes the cell back. An agent asked by someone else names an item only
// when both of them can open it (AlsoFor). A guest's read names people,
// without their ids, which are members', and leaves every other item out.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	formula "github.com/akashc777/OneCamp/business/Formula"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// untitled is what an item with no title of its own is called.
var untitled = map[string]string{
	fieldTypes.ItemDoc: "Untitled doc", fieldTypes.ItemTask: "Untitled task", fieldTypes.ItemBoard: "Untitled board", fieldTypes.ItemProject: "Untitled project", fieldTypes.ItemUser: "Someone",
}

const (
	// maxReadItems is how many different items one read names; those past
	// it come back hidden.
	maxReadItems = 10000
	// itemBatch is how many items of a kind one lookup names.
	itemBatch = 500
)

// itemsOf is what a relation field links to when it links to OneCamp items:
// one kind, or fieldTypes.ItemAny; ok is false for any other field.
func itemsOf(f *model.Field) (target string, ok bool) {
	if f != nil && f.Type == model.FieldPeople {
		return fieldTypes.ItemUser, true // members, kept as a link to people is
	}
	if f == nil || f.Type != model.FieldRelation {
		return "", false
	}
	if target = fieldTypes.RelationTarget(f); target == fieldTypes.TargetTable {
		return "", false
	}
	return target, true
}

// namedPerReader is a field whose cells read as what their reader may open:
// a link to docs, tasks, boards or projects, which nameItems names by the
// reader. No one stored value serves every reader, so a value over one is
// never kept (CR-92 M1). People are named alike for everyone (peopleNames).
func namedPerReader(f *model.Field) bool {
	target, items := itemsOf(f)
	return items && target != fieldTypes.ItemUser
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
	fieldTypes.ItemDoc: func(ctx context.Context, ids []string, who *itemReader) (map[string]string, error) {
		return docBusiness.ReadableDocTitles(ctx, ids, who.uuid)
	},
	fieldTypes.ItemBoard: func(ctx context.Context, ids []string, who *itemReader) (map[string]string, error) {
		return boardBusiness.ReadableBoardTitles(ctx, ids, who.uuid)
	},
	fieldTypes.ItemTask: func(ctx context.Context, ids []string, who *itemReader) (map[string]string, error) {
		uid, err := who.graphUID(ctx)
		if err != nil {
			return nil, err
		}
		return taskBusiness.ReadableTaskTitles(ctx, ids, uid)
	},
	fieldTypes.ItemProject: projectTitles,
	fieldTypes.ItemUser:    peopleNames,
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
func nameItems(ctx context.Context, wanted []fieldTypes.ItemRef) itemNames {
	a, member := ctx.Value(viewerKey{}).(Actor)
	n := itemNames{titles: map[string]map[string]string{}, guest: !member}
	byKind := map[string][]string{}
	asked := map[string]bool{}
	for _, r := range wanted {
		key := r.Type + "/" + r.ID
		if asked[key] || len(asked) == maxReadItems || (n.guest && r.Type != fieldTypes.ItemUser) {
			continue
		}
		asked[key] = true
		byKind[r.Type] = append(byKind[r.Type], r.ID)
	}
	readers := []*itemReader{{uuid: a.UserID.String()}}
	if also, ok := ctx.Value(alsoKey{}).(Actor); ok {
		readers = append(readers, &itemReader{uuid: also.UserID.String()})
	}
	for kind, ids := range byKind {
		askers := readers
		if kind == fieldTypes.ItemUser {
			askers = readers[:1] // people are named alike for everyone
		}
		var titles map[string]string
		for i, who := range askers {
			got, err := namesFor(ctx, kind, ids, who)
			if err != nil {
				helpers.LogErrorWithContext(ctx, "business/DataTable could not name the %s a row links to, leaving them hidden: %v", fieldTypes.ItemKinds[kind], err)
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
func (n itemNames) cell(refs []fieldTypes.ItemRef) ([]fieldTypes.ItemRef, formula.Value) {
	out := make([]fieldTypes.ItemRef, 0, len(refs))
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		if n.guest && r.Type != fieldTypes.ItemUser {
			continue
		}
		title, ok := n.titles[r.Type][strings.ToLower(r.ID)]
		if !ok {
			if !n.guest {
				out = append(out, fieldTypes.ItemRef{ID: r.ID, Type: r.Type, Hidden: true})
			}
			continue
		}
		if strings.TrimSpace(title) == "" {
			title = untitled[r.Type]
		}
		named := fieldTypes.ItemRef{ID: r.ID, Type: r.Type, Label: title}
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
	kinds := make([]string, 0, len(fieldTypes.ItemKinds))
	for kind := range fieldTypes.ItemKinds {
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
