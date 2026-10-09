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

	"github.com/akashc777/OneCamp/business/DataTable/formula"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
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
}

// rollupFits is whether a field whose cells are of kind k can be added up
// that way.
func rollupFits(how string, k formula.Kind) bool {
	switch how {
	case "count", "count_values", "unique_count", "list", "unique":
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
		return nil, fmt.Errorf("Link at most %d rows at a time", maxLinks)
	}
	rows, err := model.RowsIn(ctx, t.Id, ids)
	if err != nil {
		return nil, fmt.Errorf("failed to check the linked rows")
	}
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

// takeLinks takes the cells linking to tables out of a new row (links aren't
// stored in a row's values), as the links it's made with:
//   - a cell links the row to the rows it lists, by id or as a read gave them
//     (linkItems), whichever side of the link it's on;
//   - a cell linking to a table the actor can't open is left out.
//
// It also returns the tables the links go to, whose readers should look
// again.
func takeLinks(ctx context.Context, actor Actor, fields []*model.Field, values map[string]interface{}, row uuid.UUID) ([]model.FieldLinks, []uuid.UUID, error) {
	var changes []model.FieldLinks
	var tables []uuid.UUID
	for _, f := range fields {
		l, ok := linkOf(f)
		if !ok {
			continue
		}
		raw, present := values[f.Id.String()]
		delete(values, f.Id.String())
		field, ok := l.linkField()
		if !present || !ok {
			continue
		}
		t, err := model.GetTableByID(ctx, l.table)
		if err != nil || t == nil || !canView(t, actor) {
			continue
		}
		ids, bad := linkItems(raw)
		if bad != "" {
			return nil, nil, fmt.Errorf("%q links to rows of %q by their ids: %s isn't one", f.Name, t.Name, bad)
		}
		add, err := linksToAdd(ctx, l, t, f.Name, row, ids)
		if err != nil {
			return nil, nil, err
		}
		if len(add) > 0 {
			changes = append(changes, model.FieldLinks{Field: field, Add: add})
			tables = append(tables, l.table)
		}
	}
	return changes, tables, nil
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
// rows and another table's, or the rows they link to, changed. A table is
// told at most once each tellEvery, so a burst of writes elsewhere has its
// readers look again once, at the end.
func tellTables(ids ...uuid.UUID) {
	seen := map[uuid.UUID]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
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
		tellTables(tables...)
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
	if _, err := loadViewable(ctx, tableID, actor); err != nil {
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
	if done.Added, done.Removed, err = model.ChangeLinks(ctx, change, maxLinks); err != nil {
		if errors.Is(err, model.ErrTooManyLinks) {
			return nil, LinksChanged{}, errTooManyLinks
		}
		return nil, LinksChanged{}, fmt.Errorf("failed to change the links")
	}
	tellTables(tableID, l.table)
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

// computedFieldConfig checks the config of a field being saved in table t
// (self, or uuid.Nil for a new one) when its values are worked out on reads
// or link to a table, and sets the config to store. For a relation that
// should show its links in the other table too, it returns that table.
func computedFieldConfig(ctx context.Context, actor Actor, t *model.DataTable, self uuid.UUID, in *FieldInput) (*model.DataTable, error) {
	typ := strings.TrimSpace(in.Type)
	fields, err := model.ListFields(ctx, t.Id)
	if err != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	for _, f := range fields {
		if _, isLink := linkOf(f); isLink && f.Id == self && typ != model.FieldRelation {
			return nil, fmt.Errorf("A link to another table can't become another kind of field; delete it instead")
		}
	}
	switch typ {
	case model.FieldFormula:
		cfg, err := formulaConfigWith(fields, self, in.Config)
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
		cfg, err := rollupFieldConfig(ctx, actor, t.Id, fields, in.Config)
		if err != nil {
			return nil, err
		}
		in.Config = cfg
	}
	return nil, nil
}

// unlinkPair deletes a field, and what goes with it for a relation linking
// to a table:
//   - one whose links show from the other table takes the field showing them,
//     and its links, with it;
//   - one showing another's links goes alone, and the other is no longer
//     shown elsewhere.
func unlinkPair(ctx context.Context, f *model.Field) error {
	l, ok := linkOf(f)
	switch {
	case !ok:
		return model.DeleteField(ctx, f.TableId, f.Id)
	case l.inverseOf == "":
		var other *model.Field
		if id, err := uuid.Parse(l.inverse); err == nil {
			other = &model.Field{Id: id, TableId: l.table}
		}
		return model.DeleteFieldPair(ctx, f, other)
	}
	if err := model.DeleteField(ctx, f.TableId, f.Id); err != nil {
		return err
	}
	fid, err := uuid.Parse(l.inverseOf)
	if err != nil {
		return nil
	}
	theirs, err := model.ListFields(ctx, l.table)
	if err != nil {
		return nil
	}
	for _, g := range theirs {
		if gl, ok := linkOf(g); ok && g.Id == fid && gl.inverse == f.Id.String() {
			cfg, _ := json.Marshal(map[string]string{"relation_target": linksTable, "table_id": gl.table.String()})
			g.Config = string(cfg)
			_ = model.UpdateField(ctx, g)
		}
	}
	return nil
}

// rollupFieldConfig checks a rollup's config as it's saved in table own,
// with fields: a relation here that links to a table the actor can open, one
// of that table's fields (none for counting), and a way of adding it up that
// fits that field.
func rollupFieldConfig(ctx context.Context, actor Actor, own uuid.UUID, fields []*model.Field, cfg map[string]interface{}) (map[string]interface{}, error) {
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
	if how == "count" {
		return out, nil
	}
	theirs := fields
	if l.table != own {
		if theirs, err = model.ListFields(ctx, l.table); err != nil {
			return nil, fmt.Errorf("failed to load fields")
		}
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
