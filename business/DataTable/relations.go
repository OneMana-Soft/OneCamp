package business

// Relations between tables, and rollups.
//
// A relation field can link a row to rows of a table, its own included:
// config {"relation_target": "table", "table_id": "<table>"}. Its links live
// in data_table_links (migration 197), not in the row's values, so saving a
// row never touches them. They're added and removed a few at a time
// (ChangeLinks), and a new row's link cells link it as it's made (takeLinks).
// Each read works out the linked rows' names from their first field, so a
// renamed row reads right everywhere.
//
// The field showing those links from the other table ({"inverse_of":
// "<field>"}) has no links of its own: its cells are the rows linking to each
// row, found by index on each read, so the two sides can't disagree.
//
// A rollup field ({"relation": "<relation field>", "field": "<field of the
// linked table>", "aggregate": "<how>"}) adds up a field of the linked rows
// on each read (computed.go), and formulas can read it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	formula "github.com/akashc777/OneCamp/business/Formula"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

const (
	// linksTable is the relation target that links to a table's rows.
	linksTable = "table"
	// maxLinks is how many rows one row can link to through a field, and
	// how many links one change makes.
	maxLinks = 1000
	// maxShownLinks is how many links a cell shows; the rest are counted.
	maxShownLinks = 100
	// privateRow is how a link to a row of a table the reader can't open
	// shows.
	privateRow = "Private row"
)

type viewerKey struct{}

// guest is the viewer of a read made for no member: a guest link's, or an AI
// column's prompt, whose answer every reader of the table sees.
type guest struct{}

// asViewer has a read made for a member, who sees links and rollups across
// every table they can open.
func asViewer(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, viewerKey{}, a)
}

// asGuest has a read made for no member: it sees links and rollups only
// within the table it reads. A read with no viewer at all is one too.
func asGuest(ctx context.Context) context.Context {
	return context.WithValue(ctx, viewerKey{}, guest{})
}

// mayOpen is whether a read's viewer can open table t; own is the table being
// read.
func mayOpen(ctx context.Context, t *model.DataTable, own uuid.UUID) bool {
	if t == nil {
		return false
	}
	if a, ok := ctx.Value(viewerKey{}).(Actor); ok {
		return canView(t, a)
	}
	return t.Id == own
}

// relationConfig is a relation field's config.
type relationConfig struct {
	Target string `json:"relation_target"`
	Table  string `json:"table_id,omitempty"`
	// InverseOf is set on a field showing another field's links: that
	// field's id. Inverse is set on a field whose links another shows.
	InverseOf string `json:"inverse_of,omitempty"`
	Inverse   string `json:"inverse,omitempty"`
}

// tableLink is a relation field that links rows to a table's rows.
type tableLink struct {
	field     *model.Field
	table     uuid.UUID
	inverseOf string
	inverse   string
}

func linkOf(f *model.Field) (tableLink, bool) {
	if f == nil || f.Type != model.FieldRelation {
		return tableLink{}, false
	}
	var c relationConfig
	if json.Unmarshal([]byte(f.Config), &c) != nil || c.Target != linksTable {
		return tableLink{}, false
	}
	id, err := uuid.Parse(c.Table)
	if err != nil {
		return tableLink{}, false
	}
	return tableLink{field: f, table: id, inverseOf: c.InverseOf, inverse: c.Inverse}, true
}

// linkField is the field whose links a table link's cells show: its own, or,
// on the side showing another's, that one's. ok is false when that's gone.
func (l tableLink) linkField() (uuid.UUID, bool) {
	if l.inverseOf == "" {
		return l.field.Id, true
	}
	id, err := uuid.Parse(l.inverseOf)
	return id, err == nil
}

// link is a link a table link's cell makes, between its row and other.
func (l tableLink) link(row, other uuid.UUID) model.Link {
	if l.inverseOf == "" {
		return model.Link{From: row, To: other}
	}
	return model.Link{From: other, To: row}
}

// rollupConfig is a rollup field's config.
type rollupConfig struct {
	Relation  string `json:"relation"`
	Field     string `json:"field"`
	Aggregate string `json:"aggregate"`
	// Formula adds up values when Aggregate is "formula" (rollupMore.go).
	Formula string `json:"formula,omitempty"`
	// Where is the conditions a linked row meets to be added up.
	Where []Filter `json:"where,omitempty"`
	// Sort is the field of the linked rows, by id, that orders them before
	// they're added up (a list follows it), Desc largest or latest first, and
	// Limit how many of the first are added up: Airtable's sorted rollups and
	// first N. Without Sort, the first in link order.
	Sort  string `json:"sort,omitempty"`
	Desc  bool   `json:"desc,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

func rollupOf(f *model.Field) (rollupConfig, bool) {
	if f == nil || f.Type != model.FieldRollup {
		return rollupConfig{}, false
	}
	var c rollupConfig
	_ = json.Unmarshal([]byte(f.Config), &c)
	return c, true
}

// rollupGives is each way a rollup adds up, and what it gives.
var rollupGives = map[string]formula.Kind{
	"count":        formula.KindNumber,
	"count_values": formula.KindNumber,
	"unique_count": formula.KindNumber,
	"sum":          formula.KindNumber,
	"average":      formula.KindNumber,
	"min":          formula.KindNumber,
	"max":          formula.KindNumber,
	"checked":      formula.KindNumber,
	"earliest":     formula.KindDate,
	"latest":       formula.KindDate,
	"list":         formula.KindText,
	"unique":       formula.KindText,
	// A formula gives what it gives (rollupKind).
	rollupByFormula: formula.KindText,
}

// rollupFits is whether a field whose cells are of kind k can be added up
// that way.
func rollupFits(how string, k formula.Kind) bool {
	switch how {
	case "count", "count_values", "unique_count", "list", "unique", rollupByFormula:
		return true
	case "sum", "average", "min", "max":
		return k == formula.KindNumber
	case "earliest", "latest":
		return k == formula.KindDate
	case "checked":
		return k == formula.KindBool
	}
	return false
}

// linkItems reads the rows a relation cell written links to: ids, or refs
// ({"id": …}) as a read gives them, in order, each once. A read's count of
// the links past those it shows ({"type": "more"}) is passed over. bad is
// the first item that isn't a row's id, as it was given.
func linkItems(raw interface{}) (ids []uuid.UUID, bad string) {
	items, isList := raw.([]interface{})
	if strs, ok := raw.([]string); ok { // as a Go writer (an import) gives them
		items, isList = make([]interface{}, len(strs)), true
		for i, s := range strs {
			items[i] = s
		}
	}
	if !isList && raw != nil {
		items = []interface{}{raw}
	}
	seen := map[uuid.UUID]bool{}
	for _, it := range items {
		s, ok := it.(string)
		if m, isRef := it.(map[string]interface{}); isRef {
			if m["type"] == "more" {
				continue
			}
			s, ok = m["id"].(string)
		}
		id, err := uuid.Parse(strings.TrimSpace(s))
		if !ok || err != nil {
			b, _ := json.Marshal(it)
			return nil, string(b)
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, ""
}

// linksToAdd is the links a cell adds between row and the rows of table t
// it lists: rows deleted since are passed over, and anything that isn't a
// row of t is an error.
func linksToAdd(ctx context.Context, l tableLink, t *model.DataTable, fieldName string, row uuid.UUID, ids []uuid.UUID) ([]model.Link, error) {
	if len(ids) > maxLinks {
		return nil, errLinkAtMost
	}
	rows, err := model.RowsIn(ctx, t.Id, ids)
	if err != nil {
		return nil, errLinkedRowsUnread
	}
	return addsOf(l, t, fieldName, row, ids, rows)
}

// errLinkAtMost and errLinkedRowsUnread refuse a cell's links: it lists too
// many rows at once, or the rows it lists couldn't be read.
var (
	errLinkAtMost       = fmt.Errorf("Link at most %d rows at a time", maxLinks)
	errLinkedRowsUnread = fmt.Errorf("failed to check the linked rows")
)

// addsOf is linksToAdd's links, with rows the rows of t that ids name, as
// RowsIn reads them. Pure.
func addsOf(l tableLink, t *model.DataTable, fieldName string, row uuid.UUID, ids []uuid.UUID, rows map[uuid.UUID]bool) ([]model.Link, error) {
	add := make([]model.Link, 0, len(ids))
	for _, id := range ids {
		live, found := rows[id]
		if !found {
			return nil, fmt.Errorf("%q links to rows of %q: %s isn't one", fieldName, t.Name, id)
		}
		if live {
			add = append(add, l.link(row, id))
		}
	}
	return add, nil
}

// takeLinks takes the cells linking to tables out of a new row, as the links
// it's made with (linkCellsOf). It also returns the tables the links go to,
// whose readers should look again.
func takeLinks(ctx context.Context, actor Actor, fields []*model.Field, values map[string]interface{}, row uuid.UUID) ([]model.FieldLinks, []uuid.UUID, error) {
	rows := []rowCells{{row: row, values: values}}
	linkCellsOf(ctx, actor, fields, rows, false)
	if rows[0].err != nil {
		return nil, nil, rows[0].err
	}
	var changes []model.FieldLinks
	var tables []uuid.UUID
	for _, c := range rows[0].cells {
		changes = append(changes, c.Links)
		tables = append(tables, c.table)
	}
	return changes, tables, nil
}

// linkCell is one cell's change to its row's links, and the table they go to.
type linkCell struct {
	model.RowLinks
	table uuid.UUID
}

// rowCells is a row whose cells linking to tables linkCellsOf reads, taking
// them out of its values, and what they come to: its cells' changes to its
// links, or why it can't have them (then none).
type rowCells struct {
	row    uuid.UUID
	values map[string]interface{}
	cells  []linkCell
	err    error
}

// linkGroup is how many rows' cells of a field linkCellsOf reads at once,
// so each read stays bounded: a row lists at most maxLinks rows.
const linkGroup = 100

// linkCellsOf takes the cells linking to tables out of rows' values (links
// aren't stored in a row's values), as changes to their links:
//   - a cell links its row to the rows it lists, by id or as a read gave them
//     (linkItems), whichever side of the link it's on;
//   - with replace (a patch to a row that is there) it sets the row's links
//     through its field to those rows, adding the missing and removing the
//     others, so writing a relation's cell means for a row there what it
//     means for a new one; a cell not written leaves its links be;
//   - a cell linking to a table the actor can't open is left out.
//
// Each field's table is read once, and a group's links and the rows its
// cells list in a read each, where reading a row at a time made them reads
// of every row (a batch's patches, an import's links). A row's error is its
// own: it gets no links, and the others are read on.
func linkCellsOf(ctx context.Context, actor Actor, fields []*model.Field, rows []rowCells, replace bool) {
	for _, f := range fields {
		l, ok := linkOf(f)
		if !ok {
			continue
		}
		field, ok := l.linkField()
		var cells []rawCell
		for i := range rows {
			if rows[i].err != nil {
				continue // its first error is its answer, as a row at a time
			}
			raw, present := rows[i].values[f.Id.String()]
			delete(rows[i].values, f.Id.String())
			if present && ok {
				cells = append(cells, rawCell{at: i, raw: raw})
			}
		}
		if len(cells) == 0 {
			continue
		}
		t, err := model.GetTableByID(ctx, l.table)
		if err != nil || t == nil || !canView(t, actor) {
			continue
		}
		for len(cells) > 0 {
			n := min(linkGroup, len(cells))
			readLinkCells(ctx, f, l, field, t, rows, cells[:n], replace)
			cells = cells[n:]
		}
	}
	for i := range rows {
		if rows[i].err != nil {
			rows[i].cells = nil
		}
	}
}

// rawCell is a row's cell of a link field as written, by the row's place in
// linkCellsOf's rows.
type rawCell struct {
	at  int
	raw interface{}
}

// readLinkCells reads a group of rows' cells of link field f, to table t: the
// rows' links through it in one read (with replace) and the rows the cells
// list in one, and adds each row's change to its cells, or sets its error.
func readLinkCells(ctx context.Context, f *model.Field, l tableLink, field uuid.UUID, t *model.DataTable, rows []rowCells, cells []rawCell, replace bool) {
	ids := make([][]uuid.UUID, len(cells))
	for j, c := range cells {
		var bad string
		if ids[j], bad = linkItems(c.raw); bad != "" {
			rows[c.at].err = fmt.Errorf("%q links to rows of %q by their ids: %s isn't one", f.Name, t.Name, bad)
		}
	}
	remove := make([][]model.Link, len(cells))
	if replace {
		var which []uuid.UUID
		var limits []int
		for _, c := range cells {
			if rows[c.at].err == nil {
				which, limits = append(which, rows[c.at].row), append(limits, maxLinks+1)
			}
		}
		to := l.inverseOf != "" // the rows are the other end of field's links
		now, err := model.LinksOf(ctx, field, which, limits, to)
		for j, c := range cells {
			r := &rows[c.at]
			if r.err != nil {
				continue
			}
			if err != nil {
				r.err = fmt.Errorf("failed to read the row's links")
				continue
			}
			want := make(map[uuid.UUID]bool, len(ids[j]))
			for _, id := range ids[j] {
				want[id] = true
			}
			has := make(map[uuid.UUID]bool, len(now[r.row]))
			for _, id := range now[r.row] {
				has[id] = true
				if !want[id] {
					remove[j] = append(remove[j], l.link(r.row, id))
				}
			}
			var missing []uuid.UUID
			for _, id := range ids[j] {
				if !has[id] {
					missing = append(missing, id)
				}
			}
			ids[j] = missing
		}
	}
	var listed []uuid.UUID
	for j, c := range cells {
		if rows[c.at].err != nil {
			continue
		}
		if len(ids[j]) > maxLinks {
			rows[c.at].err = errLinkAtMost
			continue
		}
		listed = append(listed, ids[j]...)
	}
	live, err := model.RowsIn(ctx, t.Id, listed)
	for j, c := range cells {
		r := &rows[c.at]
		if r.err != nil {
			continue
		}
		if err != nil && len(ids[j]) > 0 {
			r.err = errLinkedRowsUnread
			continue
		}
		add, aerr := addsOf(l, t, f.Name, r.row, ids[j], live)
		if aerr != nil {
			r.err = aerr
			continue
		}
		if len(add)+len(remove[j]) > 0 {
			r.cells = append(r.cells, linkCell{RowLinks: model.RowLinks{Row: r.row, Shown: f.Id, Links: model.FieldLinks{Field: field, Add: add, Remove: remove[j]}}, table: l.table})
		}
	}
}

// errTooManyLinks is what a write that would take a row past maxLinks links
// through a field says.
var errTooManyLinks = fmt.Errorf("A row can link to at most %d rows through a field", maxLinks)

// tellEvery is the least time between two notices to a table's readers that
// its links changed: those in between are sent as one, at its end.
var tellEvery = 2 * time.Second

// publishLinks tells a table's readers that its links, or the rows they link
// to, changed, so they read it again.
var publishLinks = func(id uuid.UUID) {
	mqttBusiness.PublishTableRow(id.String(), map[string]interface{}{"action": "links"})
}

// told is when each table's readers were last told, and the tables with a
// notice waiting to be sent.
var told = struct {
	sync.Mutex
	at      map[uuid.UUID]time.Time
	waiting map[uuid.UUID]bool
}{at: map[uuid.UUID]time.Time{}, waiting: map[uuid.UUID]bool{}}

// tellTables has the readers of each table look again: links between its
// rows and another table's, or the rows they link to, changed. Its row lists
// are stale at once (a filter on links reads them, and this table's ledger
// never saw the change); its readers are told at most once each tellEvery,
// so a burst of writes elsewhere has them look again once, at the end.
func tellTables(ctx context.Context, ids ...uuid.UUID) {
	seen := map[uuid.UUID]bool{}
	tables := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			tables = append(tables, id)
		}
	}
	rowQuery.ListsStale(ctx, tables...)
	for _, id := range tables {
		switch wait := toldIn(id); {
		case wait == 0:
			publishLinks(id)
		case wait > 0:
			time.AfterFunc(wait, func() {
				told.Lock()
				delete(told.waiting, id)
				told.at[id] = time.Now()
				told.Unlock()
				publishLinks(id)
			})
		}
	}
}

// toldIn books a notice to a table's readers: 0 to send it now, how long to
// wait before sending it, or less than 0 when one is waiting already.
func toldIn(id uuid.UUID) time.Duration {
	told.Lock()
	defer told.Unlock()
	if told.waiting[id] {
		return -1
	}
	now := time.Now()
	if wait := tellEvery - now.Sub(told.at[id]); wait > 0 {
		told.waiting[id] = true
		return wait
	}
	if len(told.at) > 10000 {
		for t, at := range told.at {
			if now.Sub(at) > tellEvery {
				delete(told.at, t)
			}
		}
	}
	told.at[id] = now
	return 0
}

// tellLinkedTables has the readers of the tables linking with a table look
// again, after a row of it that's linked changed (or was, before it was
// deleted): their cells show its name, and their rollups add it up.
func tellLinkedTables(ctx context.Context, table, row uuid.UUID, wasLinked bool) {
	if !wasLinked {
		if has, err := model.RowHasLinks(ctx, row); err != nil || !has {
			return
		}
	}
	tellTablesLinkedWith(ctx, table)
}

// tellTablesLinkedWith has the readers of the tables linking with a table
// look again: one of its fields changed, which their cells or rollups may
// read, or which shows their links.
func tellTablesLinkedWith(ctx context.Context, table uuid.UUID) {
	if tables, err := model.TablesLinkedWith(ctx, table); err == nil {
		tellTables(ctx, tables...)
	}
}

// LinksChanged is how many links a change made, and how many it removed:
// links there already, rows deleted since and links gone already don't
// count.
type LinksChanged struct {
	Added, Removed int
}

// ChangeLinks links a row to rows add, and unlinks it from rows remove,
// through a relation field linking to a table, or the field showing such
// links from the other table. The actor needs view access to both tables;
// rows deleted since are passed over, and anything else that isn't a row of
// the other table is an error. It returns the row, as the actor reads it,
// and how many links changed.
func ChangeLinks(ctx context.Context, tableID, rowID, fieldID uuid.UUID, add, remove []uuid.UUID, actor Actor) (*model.Row, LinksChanged, error) {
	ctx = asViewer(ctx, actor)
	t, err := loadViewable(ctx, tableID, actor)
	if err != nil {
		return nil, LinksChanged{}, err
	}
	fields, err := model.ListFields(ctx, tableID)
	if err != nil {
		return nil, LinksChanged{}, fmt.Errorf("failed to load fields")
	}
	var l tableLink
	found := false
	for _, f := range fields {
		if f.Id == fieldID {
			l, found = linkOf(f)
		}
	}
	if !found {
		return nil, LinksChanged{}, fmt.Errorf("That field doesn't link to a table")
	}
	field, ok := l.linkField()
	other, err := model.GetTableByID(ctx, l.table)
	if !ok || err != nil || other == nil || !canView(other, actor) {
		return nil, LinksChanged{}, errForbidden
	}
	row, err := model.GetRowByID(ctx, tableID, rowID)
	if err != nil {
		return nil, LinksChanged{}, fmt.Errorf("failed to load row")
	}
	if row == nil {
		return nil, LinksChanged{}, errNotFound
	}
	if len(add)+len(remove) > maxLinks {
		return nil, LinksChanged{}, fmt.Errorf("Change at most %d links at a time", maxLinks)
	}
	change := model.FieldLinks{Field: field}
	if change.Add, err = linksToAdd(ctx, l, other, l.field.Name, rowID, add); err != nil {
		return nil, LinksChanged{}, err
	}
	for _, id := range remove {
		change.Remove = append(change.Remove, l.link(rowID, id))
	}
	var done LinksChanged
	cs := newChangeSet(ctx, tableID, actor, rowLedger.ChangeLinks)
	if done.Added, done.Removed, err = model.ChangeRowLinks(ctx, rowID, fieldID, change, maxLinks, cs, observe); err != nil {
		if errors.Is(err, model.ErrTooManyLinks) {
			return nil, LinksChanged{}, errTooManyLinks
		}
		return nil, LinksChanged{}, fmt.Errorf("failed to change the links")
	}
	tellTables(ctx, tableID, l.table)
	announce(ctx, t, cs, hooksOnly)
	withComputed(ctx, fields, []*model.Row{row})
	return row, done, nil
}

// linkFieldConfig checks a relation field's config as it's saved (self is
// the field, or uuid.Nil for a new one). One linking to a table must link to
// one the actor can open, and keeps the table it was made with. It returns
// the config to store and, when its links should show from the other table
// too ("two_way") and don't yet, that table, which the actor must be able to
// change.
func linkFieldConfig(ctx context.Context, actor Actor, self uuid.UUID, cfg map[string]interface{}, fields []*model.Field) (map[string]interface{}, *model.DataTable, error) {
	target, _ := cfg["relation_target"].(string)
	var was *tableLink
	for _, f := range fields {
		if f.Id == self {
			if l, ok := linkOf(f); ok {
				was = &l
			}
		}
	}
	if target != linksTable {
		if was != nil {
			return nil, nil, fmt.Errorf("A link to another table can't become another kind of field; delete it instead")
		}
		return cfg, nil, nil
	}
	ref, _ := cfg["table_id"].(string)
	id, err := uuid.Parse(ref)
	if err != nil {
		return nil, nil, fmt.Errorf("Choose the table to link to")
	}
	twoWay, _ := cfg["two_way"].(bool)
	out := map[string]interface{}{"relation_target": linksTable, "table_id": id.String()}
	if was != nil {
		if was.table != id {
			return nil, nil, fmt.Errorf("A relation's table can't change; make a new relation instead")
		}
		// Kept as it was made: which table, and which field shows it there.
		if was.inverseOf != "" {
			out["inverse_of"] = was.inverseOf
		}
		if was.inverse != "" {
			out["inverse"] = was.inverse
		}
		if !twoWay || was.inverse != "" || was.inverseOf != "" {
			return out, nil, nil
		}
	}
	t, err := model.GetTableByID(ctx, id)
	if err != nil || t == nil || !canView(t, actor) {
		return nil, nil, fmt.Errorf("Choose a table you can open")
	}
	if !twoWay {
		return out, nil, nil
	}
	if !canManage(t, actor) {
		return nil, nil, fmt.Errorf("Only someone who can change %q can show these links there", t.Name)
	}
	return out, t, nil
}

// inverseName is a name for the field showing a table's links in another:
// the linking table's name, numbered when the other already has a field so
// named.
func inverseName(name string, fields []*model.Field) string {
	taken := map[string]bool{}
	for _, f := range fields {
		taken[strings.ToLower(strings.TrimSpace(f.Name))] = true
	}
	out := name
	for n := 2; taken[strings.ToLower(out)]; n++ {
		out = fmt.Sprintf("%s %d", name, n)
	}
	return out
}

// pairFor is the field showing relation field f's links in table to (whose
// fields are there), last among them; f's config is set to point at it.
func pairFor(f *model.Field, from, to *model.DataTable, there []*model.Field) *model.Field {
	// Named against the fields there, and f, in case it's the same table.
	other := &model.Field{Id: uuid.New(), TableId: to.Id, Name: inverseName(from.Name, append(there, f)), Type: model.FieldRelation}
	for _, g := range there {
		if g.Position >= other.Position {
			other.Position = g.Position + 1
		}
	}
	if to.Id == from.Id && f.Position >= other.Position {
		other.Position = f.Position + 1
	}
	mine, _ := json.Marshal(map[string]string{"relation_target": linksTable, "table_id": to.Id.String(), "inverse": other.Id.String()})
	theirs, _ := json.Marshal(map[string]string{"relation_target": linksTable, "table_id": from.Id.String(), "inverse_of": f.Id.String()})
	f.Config, other.Config = string(mine), string(theirs)
	return other
}

// fieldsThere is the fields of the table a relation's links show in, which
// must have room for one more field linking to a table, besides adding.
func fieldsThere(ctx context.Context, to *model.DataTable, adding ...*model.Field) ([]*model.Field, error) {
	there, err := model.ListFields(ctx, to.Id)
	if err != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	if err := roomFor(append(there, adding...), uuid.Nil, model.FieldRelation, true); err != nil {
		return nil, fmt.Errorf("%q has no room to show these links: %v", to.Name, err)
	}
	return there, nil
}

// createLinkPair makes a relation field and the field showing its links in
// the other table, each pointing at the other, in one transaction. A table
// linking to itself needs room for both.
func createLinkPair(ctx context.Context, f *model.Field, from, to *model.DataTable) (*model.Field, error) {
	f.Id = uuid.New()
	var adding []*model.Field
	if to.Id == from.Id {
		adding = append(adding, f)
	}
	there, err := fieldsThere(ctx, to, adding...)
	if err != nil {
		return nil, err
	}
	if err := model.CreateFieldsWithIDs(ctx, f, pairFor(f, from, to, there)); err != nil {
		return nil, fmt.Errorf("failed to create field")
	}
	return f, nil
}

// showLinksIn saves relation field f, which an edit made show its links in
// table to, with the field showing them there.
func showLinksIn(ctx context.Context, f *model.Field, from, to *model.DataTable) error {
	there, err := fieldsThere(ctx, to)
	if err != nil {
		return err
	}
	if err := model.CreateFieldsWithIDs(ctx, pairFor(f, from, to, there)); err != nil {
		return fmt.Errorf("failed to create field")
	}
	if err := model.UpdateField(ctx, f); err != nil {
		return fmt.Errorf("failed to update field")
	}
	return nil
}

// computedFieldConfig checks the config of a field being saved in table t,
// whose fields are fields (self, or uuid.Nil for a new one), when its values
// are worked out on reads or link to a table, and sets the config to store.
// For a relation that should show its links in the other table too, it
// returns that table.
func computedFieldConfig(ctx context.Context, actor Actor, t *model.DataTable, fields []*model.Field, self uuid.UUID, in *FieldInput) (*model.DataTable, error) {
	typ := strings.TrimSpace(in.Type)
	for _, f := range fields {
		if f.Id == self {
			if err := typeChangeProblem(f, typ); err != nil {
				return nil, err
			}
		}
	}
	switch typ {
	case model.FieldFormula:
		cfg, err := formulaConfigWith(fields, self, in.Config, linkedFieldsFor(ctx, actor))
		if err != nil {
			return nil, err
		}
		in.Config = cfg
	case model.FieldRelation:
		cfg, other, err := linkFieldConfig(ctx, actor, self, in.Config, fields)
		if err != nil {
			return nil, err
		}
		if target, _ := cfg["relation_target"].(string); target == linksTable {
			if err := roomFor(fields, self, model.FieldRelation, true); err != nil {
				return nil, err
			}
		}
		in.Config = cfg
		return other, nil
	case model.FieldRollup:
		if err := roomFor(fields, self, model.FieldRollup, false); err != nil {
			return nil, err
		}
		cfg, err := rollupFieldConfig(ctx, actor, t.Id, self, fields, in.Config)
		if err != nil {
			return nil, err
		}
		in.Config = cfg
	}
	return nil, nil
}

// unlinkPair deletes a field, and what goes with it for a relation linking
// to a table, in one transaction:
//   - one whose links show from the other table takes the field showing them,
//     and its links, with it;
//   - one showing another's links goes alone, and the other is no longer
//     shown elsewhere.
func unlinkPair(ctx context.Context, f *model.Field, by uuid.UUID) error {
	l, ok := linkOf(f)
	switch {
	case !ok:
		return model.DeleteField(ctx, f.TableId, f.Id, by)
	case l.inverseOf == "":
		var other *model.Field
		if id, err := uuid.Parse(l.inverse); err == nil {
			other = &model.Field{Id: id, TableId: l.table}
		}
		return model.DeleteFieldPair(ctx, f, other, by)
	}
	return model.DeleteShowingField(ctx, f, l.table, by)
}

// rollupFieldConfig checks a rollup's config as it's saved in table own,
// with fields: a relation here that links to a table the actor can open, one
// of that table's fields (none for counting), and a way of adding it up that
// fits that field. When that field is a rollup, the rollups self (the field
// saved, or none yet) would go through mustn't go too deep or come back round.
func rollupFieldConfig(ctx context.Context, actor Actor, own, self uuid.UUID, fields []*model.Field, cfg map[string]interface{}) (map[string]interface{}, error) {
	rel, _ := cfg["relation"].(string)
	how, _ := cfg["aggregate"].(string)
	of, _ := cfg["field"].(string)
	var l tableLink
	found := false
	for _, f := range fields {
		if f.Id.String() == rel {
			l, found = linkOf(f)
		}
	}
	if !found {
		return nil, fmt.Errorf("Choose a relation that links to a table")
	}
	if _, ok := rollupGives[how]; !ok {
		return nil, fmt.Errorf("Choose how to add it up")
	}
	t, err := model.GetTableByID(ctx, l.table)
	if err != nil || t == nil || !canView(t, actor) {
		return nil, fmt.Errorf("Choose a relation to a table you can open")
	}
	out := map[string]interface{}{"relation": rel, "aggregate": how}
	format, err := resultFormat(cfg["format"])
	if err != nil {
		return nil, err
	}
	if format != nil {
		out["format"] = format
	}
	theirs := fields
	if l.table != own {
		if theirs, err = model.ListFields(ctx, l.table); err != nil {
			return nil, fmt.Errorf("failed to load fields")
		}
	}
	where, err := rollupWhere(cfg["where"], liveOnly(theirs))
	if err != nil {
		return nil, err
	}
	if len(where) > 0 {
		out["where"] = where
	}
	order, err := rollupOrder(cfg, liveOnly(theirs))
	if err != nil {
		return nil, err
	}
	for k, v := range order {
		out[k] = v
	}
	if how == "count" {
		return out, nil
	}
	if how == rollupByFormula {
		src, _ := cfg["formula"].(string)
		if err := checkRollupFormula(src); err != nil {
			return nil, err
		}
		out["formula"] = src
	}
	var field *model.Field
	for _, f := range theirs {
		if f.Id.String() == of {
			field = f
		}
	}
	if field == nil {
		return nil, fmt.Errorf("Choose the field to add up")
	}
	if why := rollupProblem(how, field, theirs); why != "" {
		return nil, fmt.Errorf("%s", why)
	}
	if field.Type == model.FieldRollup {
		if why := rollupChainProblem(self, field, theirs, linkedFieldsFor(ctx, actor)); why != "" {
			return nil, fmt.Errorf("%s", why)
		}
	}
	out["field"] = of
	return out, nil
}

// ListTableFields is a table's fields as readers get them, for choosing
// what a rollup adds up there (view access).
func ListTableFields(ctx context.Context, id uuid.UUID, actor Actor) ([]*model.Field, error) {
	ctx = asViewer(ctx, actor)
	if _, err := loadViewable(ctx, id, actor); err != nil {
		return nil, err
	}
	fields, err := model.ListFields(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	presentComputedFields(fields, planComputed(ctx, fields))
	return fields, nil
}

// PickedRow is a row offered for linking to, named as a link shows it.
type PickedRow struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

const (
	// maxPicked is how many rows a search for rows to link offers.
	maxPicked = 20
	// pickPage and pickScan are how many rows at a time, and in all, a
	// search reads when rows are named by a formula, which isn't stored to
	// search.
	pickPage = 500
	pickScan = 5000
)

// PickRows finds a table's rows to link to (view access): those whose name
// holds text q, ignoring case, or the first ones when q is empty.
func PickRows(ctx context.Context, id uuid.UUID, q string, actor Actor) ([]PickedRow, error) {
	ctx = asViewer(ctx, actor)
	tb, err := loadViewable(ctx, id, actor)
	if err != nil {
		return nil, err
	}
	fields, err := model.ListFields(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	t := newTarget()
	t.table, t.visible, t.fields, t.label = tb, true, fields, labelField(fields)
	out := make([]PickedRow, 0, maxPicked)
	if t.label == nil {
		return out, nil
	}
	q = strings.TrimSpace(q)
	want := strings.ToLower(q)
	c := &computed{own: id, budget: formula.NewBudget()}
	now, loc := time.Now(), zoneFrom(ctx)
	// offer names rows, reading only the cells their names need, and offers
	// them, those named with q when match is set; it says when there are
	// enough.
	offer := func(ids []uuid.UUID, match bool) bool {
		c.load(ctx, t, ids, now, loc)
		for _, id := range ids {
			lr, ok := t.rows[id]
			if !ok || (match && !strings.Contains(strings.ToLower(lr.label), want)) {
				continue
			}
			if out = append(out, PickedRow{ID: id.String(), Label: lr.label}); len(out) == maxPicked {
				return true
			}
		}
		return false
	}
	switch {
	case q == "":
		ids, err := model.ListRowIDs(ctx, id, maxPicked, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to load rows")
		}
		offer(ids, false)
	case t.label.Type != model.FieldFormula:
		rows, err := model.SearchRowsByCell(ctx, id, t.label.Id.String(), q, maxPicked)
		if err != nil {
			return nil, fmt.Errorf("failed to load rows")
		}
		ids := make([]uuid.UUID, len(rows))
		for i, r := range rows {
			ids[i] = r.Id
		}
		offer(ids, false)
	default:
		for offset := 0; offset < pickScan; offset += pickPage {
			ids, err := model.ListRowIDs(ctx, id, pickPage, offset)
			if err != nil {
				return nil, fmt.Errorf("failed to load rows")
			}
			if offer(ids, true) || len(ids) < pickPage {
				break
			}
		}
	}
	return out, nil
}

// opens is whether a can open t. (On the AI edition a second person an agent
// acts for is asked too; this edition has no agents.)
func opens(ctx context.Context, t *model.DataTable, a Actor) bool {
	return t != nil && canView(t, a)
}
