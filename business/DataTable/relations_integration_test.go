//go:build integration

package business

// Relations between tables and rollups end to end, against Postgres 12 with
// every migration (197 allows rollups and keeps links in data_table_links).
// The tests cover:
//   - links read with their rows' names;
//   - the other side, worked out from the links, and changing links from
//     either side;
//   - rollups, and formulas over them;
//   - renames, deletions and permissions;
//   - writes, which add links and never remove them;
//   - limits, at scale.
//
// Run: go test -tags=integration ./business/DataTable/ -run 'TestRelation' -v

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// relationWorld is a workspace for relation tests: an owner and a member.
type relationWorld struct {
	t      *testing.T
	ctx    context.Context
	me     Actor
	member Actor
}

func newRelationWorld(t *testing.T) *relationWorld {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	user := func(name string) uuid.UUID {
		id := uuid.New()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
			INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@example.test", name, name); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		return id
	}
	return &relationWorld{t: t, ctx: ctx, me: Actor{UserID: user("owner")}, member: Actor{UserID: user("member")}}
}

func (w *relationWorld) table(name string) (*model.DataTable, *model.Field) {
	w.t.Helper()
	tb, err := CreateTable(w.ctx, TableInput{Name: name, Visibility: "workspace"}, w.me)
	if err != nil {
		w.t.Fatalf("create %s: %v", name, err)
	}
	fields, err := model.ListFields(w.ctx, tb.Id)
	if err != nil || len(fields) == 0 {
		w.t.Fatalf("%s has no name field: %v", name, err)
	}
	return tb, fields[0]
}

func (w *relationWorld) field(tb *model.DataTable, name, typ string, cfg map[string]interface{}) *model.Field {
	w.t.Helper()
	f, err := CreateField(w.ctx, tb.Id, FieldInput{Name: name, Type: typ, Config: cfg}, w.me)
	if err != nil {
		w.t.Fatalf("create %s: %v", name, err)
	}
	return f
}

// otherSide is the field showing a relation's links in the table it links to.
func (w *relationWorld) otherSide(tb *model.DataTable, of *model.Field) *model.Field {
	w.t.Helper()
	fields, _ := model.ListFields(w.ctx, tb.Id)
	for _, f := range fields {
		if l, ok := linkOf(f); ok && l.inverseOf == of.Id.String() {
			return f
		}
	}
	w.t.Fatalf("%s doesn't show %s's links", tb.Name, of.Name)
	return nil
}

func (w *relationWorld) row(tb *model.DataTable, values map[string]interface{}) uuid.UUID {
	w.t.Helper()
	r, err := CreateRow(w.ctx, tb.Id, RowInput{Values: values}, w.me)
	if err != nil {
		w.t.Fatalf("create a row in %s: %v", tb.Name, err)
	}
	return r.Id
}

// cells is each row's cells, by row, as a bundle read for actor gives them
// (a guest's, for none).
func (w *relationWorld) cells(tb *model.DataTable, actor *Actor) map[uuid.UUID]map[string]interface{} {
	w.t.Helper()
	var b *TableBundle
	var err error
	if actor == nil {
		b, err = GetGuestBundle(w.ctx, tb.Id)
	} else {
		b, err = GetBundle(w.ctx, tb.Id, *actor)
	}
	if err != nil {
		w.t.Fatalf("read %s: %v", tb.Name, err)
	}
	out := map[uuid.UUID]map[string]interface{}{}
	for _, r := range b.Rows {
		out[r.Id] = parseRowValues(r.Values)
	}
	return out
}

// labels is a relation cell's links by name.
func labels(cell interface{}) string {
	refs, _ := cell.([]interface{})
	var names []string
	for _, ref := range refs {
		names = append(names, ref.(map[string]interface{})["label"].(string))
	}
	return strings.Join(names, ", ")
}

func TestRelationsBetweenTables(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	vendors, vName := w.table("Vendors")
	budget, bName := w.table("Budget")
	cost := w.field(budget, "Cost", model.FieldNumber, nil)
	vendor := w.field(budget, "Vendor", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String(), "two_way": true})
	other := w.otherSide(vendors, vendor)
	if other.Name != "Budget" {
		t.Errorf("the other side is named %q", other.Name)
	}
	spend := w.field(vendors, "Spend", model.FieldRollup, map[string]interface{}{"relation": other.Id.String(), "field": cost.Id.String(), "aggregate": "sum"})
	items := w.field(vendors, "Items", model.FieldRollup, map[string]interface{}{"relation": other.Id.String(), "aggregate": "count"})
	double := w.field(vendors, "Double", model.FieldFormula, map[string]interface{}{"formula": "{Spend} * 2"})

	acme := w.row(vendors, map[string]interface{}{vName.Id.String(): "Acme"})
	globex := w.row(vendors, map[string]interface{}{vName.Id.String(): "Globex"})
	booth := w.row(budget, map[string]interface{}{bName.Id.String(): "Booth", cost.Id.String(): 850, vendor.Id.String(): []interface{}{map[string]interface{}{"id": acme.String()}}})
	film := w.row(budget, map[string]interface{}{bName.Id.String(): "Film", cost.Id.String(): 1200, vendor.Id.String(): []interface{}{acme.String()}})
	ads := w.row(budget, map[string]interface{}{bName.Id.String(): "Ads", cost.Id.String(): 40, vendor.Id.String(): []interface{}{map[string]interface{}{"id": globex.String(), "label": "stale"}}})

	// Budget reads its links by name; they're kept apart from the row's values.
	if got := labels(w.cells(budget, &w.me)[ads][vendor.Id.String()]); got != "Globex" {
		t.Errorf("Ads's vendor: %q", got)
	}
	stored, _ := model.GetRowByID(ctx, budget.Id, ads)
	if _, in := parseRowValues(stored.Values)[vendor.Id.String()]; in {
		t.Errorf("links stored in the row: %s", stored.Values)
	}

	check := func(when string, want map[uuid.UUID][4]interface{}) {
		t.Helper()
		got := w.cells(vendors, &w.me)
		for id, v := range want {
			c := got[id]
			if linked := labels(c[other.Id.String()]); linked != v[0] || c[spend.Id.String()] != v[1] || c[items.Id.String()] != v[2] || c[double.Id.String()] != v[3] {
				t.Errorf("%s: %q, spend %v, items %v, double %v; want %v", when, linked, c[spend.Id.String()], c[items.Id.String()], c[double.Id.String()], v)
			}
		}
	}
	check("at first", map[uuid.UUID][4]interface{}{
		acme:   {"Booth, Film", float64(2050), float64(2), float64(4100)},
		globex: {"Ads", float64(40), float64(1), float64(80)},
	})

	// A renamed vendor reads right everywhere.
	if _, err := UpdateRow(ctx, vendors.Id, acme, RowInput{Values: map[string]interface{}{vName.Id.String(): "Acme Inc"}}, w.me); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := labels(w.cells(budget, &w.me)[booth][vendor.Id.String()]); got != "Acme Inc" {
		t.Errorf("after the rename: %q", got)
	}

	// Linked from Vendors' side, Globex gets Booth as well.
	if _, _, err := ChangeLinks(ctx, vendors.Id, globex, other.Id, []uuid.UUID{booth}, nil, w.me); err != nil {
		t.Fatalf("link from the other side: %v", err)
	}
	if got := labels(w.cells(budget, &w.me)[booth][vendor.Id.String()]); got != "Acme Inc, Globex" {
		t.Errorf("Booth's vendors: %q", got)
	}
	check("Globex given Booth", map[uuid.UUID][4]interface{}{
		acme:   {"Booth, Film", float64(2050), float64(2), float64(4100)},
		globex: {"Ads, Booth", float64(890), float64(2), float64(1780)}, // in the order they were linked
	})

	// A whole row sent back with an old copy of the other side's cell, or
	// without its link cell, changes no links.
	if _, err := UpdateRow(ctx, vendors.Id, acme, RowInput{Values: map[string]interface{}{vName.Id.String(): "Acme Inc", other.Id.String(): []interface{}{}}}, w.me); err != nil {
		t.Fatalf("save Acme: %v", err)
	}
	if _, err := UpdateRow(ctx, budget.Id, film, RowInput{Values: map[string]interface{}{bName.Id.String(): "Film", cost.Id.String(): 1200}}, w.me); err != nil {
		t.Fatalf("save Film: %v", err)
	}
	check("after saves that don't name the links", map[uuid.UUID][4]interface{}{acme: {"Booth, Film", float64(2050), float64(2), float64(4100)}})

	// Unlinked from Budget's side.
	if _, _, err := ChangeLinks(ctx, budget.Id, booth, vendor.Id, nil, []uuid.UUID{globex}, w.me); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	// A deleted row drops out of the links and the totals.
	if err := DeleteRow(ctx, budget.Id, film, w.me); err != nil {
		t.Fatalf("delete: %v", err)
	}
	check("Film deleted", map[uuid.UUID][4]interface{}{
		acme:   {"Booth", float64(850), float64(1), float64(1700)},
		globex: {"Ads", float64(40), float64(1), float64(80)},
	})

	// Totals filter on a link by its name.
	res, _, err := AggregateTable(ctx, budget.Id, w.me, QuerySpec{Op: OpCount, Filters: []Filter{{Field: "Vendor", Op: "contains", Value: "Acme"}}})
	if err != nil || res.MatchedRows != 1 {
		t.Errorf("rows with Acme: %+v, %v", res, err)
	}

	// Once Vendors is private: a member sees the links to it as private rows
	// and can't change them, and a guest link sees private rows too.
	if _, _, err := UpdateTable(ctx, vendors.Id, TablePatch{Visibility: optStr(model.VisibilityPrivate)}, w.me); err != nil {
		t.Fatalf("make private: %v", err)
	}
	if got := labels(w.cells(budget, &w.member)[booth][vendor.Id.String()]); got != "Private row" {
		t.Errorf("the member sees %q", got)
	}
	if got := labels(w.cells(budget, nil)[booth][vendor.Id.String()]); got != "Private row" {
		t.Errorf("the guest sees %q", got)
	}
	if _, err := UpdateRow(ctx, budget.Id, booth, RowInput{Values: map[string]interface{}{bName.Id.String(): "Booth 2", vendor.Id.String(): []interface{}{}}}, w.member); err != nil {
		t.Fatalf("the member's save: %v", err)
	}
	if _, _, err := ChangeLinks(ctx, budget.Id, booth, vendor.Id, nil, []uuid.UUID{acme}, w.member); !IsForbidden(err) {
		t.Errorf("the member unlinked a private table's row: %v", err)
	}
	if _, _, err := UpdateTable(ctx, vendors.Id, TablePatch{Visibility: optStr(model.VisibilityWorkspace)}, w.me); err != nil {
		t.Fatalf("make public: %v", err)
	}
	if got := labels(w.cells(budget, &w.me)[booth][vendor.Id.String()]); got != "Acme Inc" {
		t.Errorf("after the member's save, Booth's vendors: %q", got)
	}

	// A relation keeps its table and its kind; a one-way one can be shown in
	// its table later, links and all.
	if _, err := UpdateField(ctx, budget.Id, vendor.Id, FieldPatch{Name: optStr("Vendor"), Type: optStr(model.FieldRelation), Config: map[string]interface{}{"relation_target": "table", "table_id": budget.Id.String()}}, w.me); err == nil {
		t.Error("a relation's table changed")
	}
	if _, err := UpdateField(ctx, budget.Id, vendor.Id, FieldPatch{Name: optStr("Vendor"), Type: optStr(model.FieldText)}, w.me); err == nil {
		t.Error("a relation became text")
	}
	backup := w.field(budget, "Backup", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String()})
	if _, _, err := ChangeLinks(ctx, budget.Id, ads, backup.Id, []uuid.UUID{acme}, nil, w.me); err != nil {
		t.Fatalf("link a backup: %v", err)
	}
	if _, err := UpdateField(ctx, budget.Id, backup.Id, FieldPatch{Name: optStr("Backup"), Type: optStr(model.FieldRelation), Config: map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String(), "two_way": true}}, w.me); err != nil {
		t.Fatalf("show a one-way relation in its table: %v", err)
	}
	backedBy := w.otherSide(vendors, backup)
	if got := labels(w.cells(vendors, &w.me)[acme][backedBy.Id.String()]); got != "Ads" {
		t.Errorf("Acme backs %q", got)
	}

	// Deleting the relation takes the other side with it to the trash, and
	// the rollups over it say so. Its links wait with the field (A2, D14),
	// read by nothing while it's deleted, so a restore brings it back whole.
	linksOf := func(f uuid.UUID) int {
		t.Helper()
		var n int
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM data_table_links WHERE field_id = $1`, f).Scan(&n); err != nil {
			t.Fatalf("count the links: %v", err)
		}
		return n
	}
	shown := func() int {
		t.Helper()
		n := 0
		for _, c := range w.cells(budget, &w.me) {
			refs, _ := c[vendor.Id.String()].([]interface{})
			n += len(refs)
		}
		return n
	}
	linked := linksOf(vendor.Id)
	if linked == 0 || shown() != linked {
		t.Fatalf("before the delete Budget shows %d of its %d links", shown(), linked)
	}
	// historyNames is whether a row's history shows a change to field f.
	historyNames := func(tb *model.DataTable, row, f uuid.UUID) bool {
		t.Helper()
		h, err := RowHistory(ctx, tb.Id, row, nil, w.me)
		if err != nil {
			t.Fatalf("%s's history: %v", tb.Name, err)
		}
		for _, it := range h.Items {
			for _, c := range it.Changes {
				if c.Field == f.String() {
					return true
				}
			}
		}
		return false
	}
	if !historyNames(budget, booth, vendor.Id) || !historyNames(vendors, globex, other.Id) {
		t.Fatal("before the delete the rows' histories don't show their links changing")
	}
	if err := DeleteField(ctx, budget.Id, vendor.Id, w.me); err != nil {
		t.Fatalf("delete the relation: %v", err)
	}
	b, _ := GetBundle(ctx, vendors.Id, w.me)
	for _, f := range b.Fields {
		if f.Id == other.Id {
			t.Error("the other side outlived the relation")
		}
		if f.Id == spend.Id && !strings.Contains(f.Config, "The relation this adds up has been deleted") {
			t.Errorf("spend after the relation went: %s", f.Config)
		}
	}
	if n := linksOf(vendor.Id); n != linked {
		t.Errorf("the deleted relation keeps %d of its %d links, for its restore", n, linked)
	}
	if n := shown(); n != 0 {
		t.Errorf("Budget still shows %d links of the deleted relation", n)
	}

	// Nor does any other reader. Rollups over the relation say it has been
	// deleted rather than add its kept links up, and Vendors' side, which
	// showed them in reverse, is gone with it.
	vc := w.cells(vendors, &w.me)
	for _, id := range []uuid.UUID{acme, globex} {
		for _, f := range []*model.Field{spend, items, double} {
			if v, isNumber := vc[id][f.Id.String()].(float64); isNumber {
				t.Errorf("%s adds up the deleted relation's kept links: %v", f.Name, v)
			}
		}
		if v, in := vc[id][other.Id.String()]; in {
			t.Errorf("Vendors still shows the deleted relation's other side: %v", v)
		}
	}
	// A guest's read, a page of rows (the v1 API's and the AI tools' read),
	// and a total filtered on the relation by name read none of them.
	for id, c := range w.cells(budget, nil) {
		if v, in := c[vendor.Id.String()]; in {
			t.Errorf("a guest reads %s's links of the deleted relation: %v", id, v)
		}
	}
	page, err := ListRows(ctx, budget.Id, w.me, 100, 0)
	if err != nil {
		t.Fatalf("a page of Budget: %v", err)
	}
	for _, r := range page {
		if v, in := parseRowValues(r.Values)[vendor.Id.String()]; in {
			t.Errorf("a page of rows reads the deleted relation: %v", v)
		}
	}
	// A total takes a filter on a field it doesn't know as no filter (as for
	// a typo): it never reads the kept links to match the one row they would.
	all, _, aerr := AggregateTable(ctx, budget.Id, w.me, QuerySpec{Op: OpCount})
	byLink, _, err := AggregateTable(ctx, budget.Id, w.me, QuerySpec{Op: OpCount, Filters: []Filter{{Field: "Vendor", Op: "contains", Value: "Acme"}}})
	if aerr != nil || err != nil || byLink.MatchedRows != all.MatchedRows {
		t.Errorf("a total filtered on the deleted relation matches %+v of %+v rows (%v, %v)", byLink, all, err, aerr)
	}
	// A reader that passes the deleted field itself still reads none of
	// its links: the computed read leaves fields in the trash out.
	trashed, err := model.GetTrashedField(ctx, budget.Id, vendor.Id)
	if err != nil || trashed == nil {
		t.Fatalf("the deleted relation isn't in the trash: %v", err)
	}
	live, _ := model.ListFields(ctx, budget.Id)
	rows, _ := model.ListRows(ctx, budget.Id, 100, 0)
	withComputed(ctx, append(live, trashed), rows)
	for _, r := range rows {
		if v, in := parseRowValues(r.Values)[vendor.Id.String()]; in {
			t.Errorf("a read given the deleted field shows its links: %v", v)
		}
	}
	// Links can't be changed through it, from either side, and stay kept.
	if _, _, err := ChangeLinks(ctx, budget.Id, ads, vendor.Id, []uuid.UUID{acme}, nil, w.me); err == nil {
		t.Error("a link was made through the deleted relation")
	}
	if _, _, err := ChangeLinks(ctx, vendors.Id, acme, other.Id, nil, []uuid.UUID{booth}, w.me); err == nil {
		t.Error("a link was removed through the deleted relation's other side")
	}
	if n := linksOf(vendor.Id); n != linked {
		t.Errorf("the deleted relation keeps %d of its %d links", n, linked)
	}
	// A row's history leaves out the relation's changes, on both sides.
	if historyNames(budget, booth, vendor.Id) {
		t.Error("Booth's history shows the deleted relation's links changing")
	}
	if historyNames(vendors, globex, other.Id) {
		t.Error("Globex's history shows the deleted relation's other side changing")
	}

	// Brought back from the trash, both sides read their links again.
	if _, err := RestoreFromTrash(ctx, budget.Id, RestoreInput{Fields: []uuid.UUID{vendor.Id}}, w.me); err != nil {
		t.Fatalf("restore the relation: %v", err)
	}
	if n := shown(); n != linked {
		t.Errorf("the restored relation shows %d of its %d links", n, linked)
	}
	if !historyNames(budget, booth, vendor.Id) || !historyNames(vendors, globex, other.Id) {
		t.Error("the restored relation's changes aren't in the rows' histories again")
	}
	b, _ = GetBundle(ctx, vendors.Id, w.me)
	back := false
	for _, f := range b.Fields {
		back = back || f.Id == other.Id
	}
	if !back {
		t.Error("the other side didn't come back with the relation")
	}
}

func TestRelationEdges(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	tasks, name := w.table("Tasks")
	blockedBy := w.field(tasks, "Blocked by", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": tasks.Id.String(), "two_way": true})
	blocks := w.otherSide(tasks, blockedBy)
	if blocks.Name != "Tasks" {
		t.Errorf("the other side of a link within a table is named %q", blocks.Name)
	}
	design := w.row(tasks, map[string]interface{}{name.Id.String(): "Design"})
	build := w.row(tasks, map[string]interface{}{name.Id.String(): "Build", blockedBy.Id.String(): []interface{}{design.String()}})
	cells := w.cells(tasks, &w.me)
	if labels(cells[build][blockedBy.Id.String()]) != "Design" || labels(cells[design][blocks.Id.String()]) != "Build" {
		t.Errorf("within a table: %v / %v", cells[build][blockedBy.Id.String()], cells[design][blocks.Id.String()])
	}

	// Picking a row to link finds it by name.
	picked, err := PickRows(ctx, tasks.Id, "des", w.me)
	if err != nil || len(picked) != 1 || picked[0].Label != "Design" {
		t.Errorf("picked %+v, %v", picked, err)
	}
	if all, _ := PickRows(ctx, tasks.Id, "", w.me); len(all) != 2 {
		t.Errorf("picking with no text: %+v", all)
	}

	// A cell links to at most 1,000 rows.
	many := make([]interface{}, maxLinks+1)
	for i := range many {
		many[i] = uuid.New().String()
	}
	if _, err := CreateRow(ctx, tasks.Id, RowInput{Values: map[string]interface{}{blockedBy.Id.String(): many}}, w.me); err == nil {
		t.Error("1,001 links were taken")
	}

	// Deleting the other side keeps the links, no longer shown there.
	if err := DeleteField(ctx, tasks.Id, blocks.Id, w.me); err != nil {
		t.Fatalf("delete the other side: %v", err)
	}
	if got := labels(w.cells(tasks, &w.me)[build][blockedBy.Id.String()]); got != "Design" {
		t.Errorf("after the other side went: %q", got)
	}
	fields, _ := model.ListFields(ctx, tasks.Id)
	for _, f := range fields {
		if f.Id == blockedBy.Id {
			var cfg map[string]interface{}
			_ = json.Unmarshal([]byte(f.Config), &cfg)
			if cfg["inverse"] != nil {
				t.Errorf("still points at the other side: %s", f.Config)
			}
		}
	}

	// A template's link to another table, or a rollup, doesn't come across.
	tb, err := CreateTableFromTemplate(ctx, TableInput{Name: "From a template", Visibility: "workspace"}, []FieldInput{
		{Name: "Name", Type: model.FieldText},
		{Name: "Elsewhere", Type: model.FieldRelation, Config: map[string]interface{}{"relation_target": "table", "table_id": tasks.Id.String()}},
		{Name: "Count", Type: model.FieldRollup, Config: map[string]interface{}{"aggregate": "count"}},
	}, nil, w.me)
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	if made, _ := model.ListFields(ctx, tb.Id); len(made) != 1 {
		t.Errorf("a template made %d fields", len(made))
	}

	// Links show in another table only where the actor can change it.
	theirs, _ := CreateTable(ctx, TableInput{Name: "Theirs", Visibility: "workspace"}, w.member)
	if _, err := CreateField(ctx, tasks.Id, FieldInput{Name: "Theirs", Type: model.FieldRelation, Config: map[string]interface{}{"relation_target": "table", "table_id": theirs.Id.String(), "two_way": true}}, w.me); err == nil {
		t.Error("links were shown in a table the actor can't change")
	}
	if _, err := CreateField(ctx, tasks.Id, FieldInput{Name: "Theirs", Type: model.FieldRelation, Config: map[string]interface{}{"relation_target": "table", "table_id": theirs.Id.String()}}, w.me); err != nil {
		t.Errorf("a one-way link to a table they can open: %v", err)
	}
}

// refuseUpdates has the database refuse every update of a field's row, until
// the func it returns lifts that.
func (w *relationWorld) refuseUpdates(field uuid.UUID) (lift func()) {
	w.t.Helper()
	exec := func(q string) {
		w.t.Helper()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(w.ctx, q); err != nil {
			w.t.Fatalf("%s: %v", q, err)
		}
	}
	exec(fmt.Sprintf(`CREATE FUNCTION refuse_field_updates() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF OLD.id = '%s'::uuid THEN
				RAISE EXCEPTION 'this field''s updates are refused';
			END IF;
			RETURN NEW;
		END $$`, field))
	exec(`CREATE TRIGGER refuse_field_updates BEFORE UPDATE ON data_table_fields FOR EACH ROW EXECUTE FUNCTION refuse_field_updates()`)
	return func() {
		exec(`DROP TRIGGER refuse_field_updates ON data_table_fields`)
		exec(`DROP FUNCTION refuse_field_updates()`)
	}
}

// twoWayRelation is a relation from Posts to Tags whose links show in Tags
// too, in the other side.
type twoWayRelation struct {
	w           *relationWorld
	tags, posts *model.DataTable
	tag, other  *model.Field
}

func (w *relationWorld) twoWay() *twoWayRelation {
	w.t.Helper()
	r := &twoWayRelation{w: w}
	r.tags, _ = w.table("Tags")
	r.posts, _ = w.table("Posts")
	r.tag = w.field(r.posts, "Tags", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": r.tags.Id.String(), "two_way": true})
	r.other = w.otherSide(r.tags, r.tag)
	return r
}

// sides is whether the other side is live, and the field the relation says
// shows its links in Tags.
func (r *twoWayRelation) sides() (bool, string) {
	t := r.w.t
	t.Helper()
	there, err := model.ListFields(r.w.ctx, r.tags.Id)
	if err != nil {
		t.Fatal(err)
	}
	live := false
	for _, f := range there {
		live = live || f.Id == r.other.Id
	}
	mine, err := model.ListFields(r.w.ctx, r.posts.Id)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range mine {
		if f.Id == r.tag.Id {
			l, ok := linkOf(f)
			if !ok || l.table != r.tags.Id {
				t.Fatalf("the relation no longer links to Tags: %s", f.Config)
			}
			return live, l.inverse
		}
	}
	t.Fatal("the relation is gone")
	return false, ""
}

// Deleting the other side of a relation and the relation's no longer showing
// its links there are one change: when the database refuses the relation's
// half, the other side stays too, and the delete says it failed.
func TestRelationOtherSideGoesInOneChange(t *testing.T) {
	w := newRelationWorld(t)
	r := w.twoWay()
	lift := w.refuseUpdates(r.tag.Id)
	if err := DeleteField(w.ctx, r.tags.Id, r.other.Id, w.me); err == nil {
		t.Error("the other side's delete said it went while the relation couldn't stop showing its links there")
	}
	lift()
	if live, shows := r.sides(); !live || shows != r.other.Id.String() {
		t.Errorf("after a refused delete the other side is live: %v, and the relation shows its links in %q", live, shows)
	}
	if err := DeleteField(w.ctx, r.tags.Id, r.other.Id, w.me); err != nil {
		t.Fatalf("delete the other side: %v", err)
	}
	if live, shows := r.sides(); live || shows != "" {
		t.Errorf("after the delete the other side is live: %v, and the relation shows its links in %q", live, shows)
	}
}

// Bringing the other side back from the trash and the relation's showing its
// links there again are one change: when the database refuses the other
// side's half, the relation stays as it was too, and the restore says it
// failed. With the relation deleted, the other side can't come back.
func TestRelationOtherSideComesBackInOneChange(t *testing.T) {
	w := newRelationWorld(t)
	r := w.twoWay()
	if err := DeleteField(w.ctx, r.tags.Id, r.other.Id, w.me); err != nil {
		t.Fatalf("delete the other side: %v", err)
	}
	restore := func() error {
		_, err := RestoreFromTrash(w.ctx, r.tags.Id, RestoreInput{Fields: []uuid.UUID{r.other.Id}}, w.me)
		return err
	}
	lift := w.refuseUpdates(r.other.Id)
	if restore() == nil {
		t.Error("the other side's restore said it came back while it stayed in the trash")
	}
	lift()
	if live, shows := r.sides(); live || shows != "" {
		t.Errorf("after a refused restore the other side is live: %v, and the relation shows its links in %q", live, shows)
	}
	if err := restore(); err != nil {
		t.Fatalf("restore the other side: %v", err)
	}
	if live, shows := r.sides(); !live || shows != r.other.Id.String() {
		t.Errorf("after the restore the other side is live: %v, and the relation shows its links in %q", live, shows)
	}

	if err := DeleteField(w.ctx, r.tags.Id, r.other.Id, w.me); err != nil {
		t.Fatalf("delete the other side again: %v", err)
	}
	if err := DeleteField(w.ctx, r.posts.Id, r.tag.Id, w.me); err != nil {
		t.Fatalf("delete the relation: %v", err)
	}
	if err := restore(); err == nil || !strings.Contains(err.Error(), "Bring back the field") {
		t.Errorf("the other side of a deleted relation came back: %v", err)
	}
}

// linkRows makes n rows of a table, with a number cell, and links each of
// them to row through field, straight in the database.
func (w *relationWorld) linkRows(table, field, num, row uuid.UUID, n int) {
	w.t.Helper()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(w.ctx, `WITH made AS (
			INSERT INTO data_table_rows (id, table_id, values, position)
			SELECT uuid_generate_v4(), $1, jsonb_build_object($2::text, 1), g FROM generate_series(1, $3::int) g RETURNING id)
		INSERT INTO data_table_links (field_id, from_row, to_row) SELECT $4, id, $5 FROM made`, table, num.String(), n, field, row); err != nil {
		w.t.Fatalf("link %d rows: %v", n, err)
	}
}

// Many rows linking to one: counted in full, shown up to a point, and added
// up as far as a rollup goes, saying so past that.
func TestRelationLimits(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	companies, cName := w.table("Companies")
	contacts, _ := w.table("Contacts")
	score := w.field(contacts, "Score", model.FieldNumber, nil)
	company := w.field(contacts, "Company", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": companies.Id.String(), "two_way": true})
	people := w.otherSide(companies, company)
	count := w.field(companies, "People", model.FieldRollup, map[string]interface{}{"relation": people.Id.String(), "aggregate": "count"})
	total := w.field(companies, "Score", model.FieldRollup, map[string]interface{}{"relation": people.Id.String(), "field": score.Id.String(), "aggregate": "sum"})
	names := w.field(companies, "Names", model.FieldFormula, map[string]interface{}{"formula": "LEN({" + people.Name + "})"})
	mid := w.row(companies, map[string]interface{}{cName.Id.String(): "Mid"})
	big := w.row(companies, map[string]interface{}{cName.Id.String(): "Big"})
	huge := w.row(companies, map[string]interface{}{cName.Id.String(): "Huge"})
	w.linkRows(contacts.Id, company.Id, score.Id, mid, maxLinks+200)
	w.linkRows(contacts.Id, company.Id, score.Id, big, maxRolledUp+5)
	w.linkRows(contacts.Id, company.Id, score.Id, huge, maxCounted+20)
	_, _ = postgresInit.DBConn.SqlDB.ExecContext(ctx, `ANALYZE`)

	got := w.cells(companies, &w.me)
	for _, c := range []struct {
		row   uuid.UUID
		n     int
		more  string
		count interface{}
		sum   interface{}
	}{
		{mid, maxLinks + 200, "1,100 more", float64(maxLinks + 200), float64(maxLinks + 200)},
		{big, maxRolledUp + 5, "9,905 more", float64(maxRolledUp + 5), errTooManyLinked.JSON()},
		{huge, maxCounted + 20, "99,901+ more", errTooManyCounted.JSON(), errTooManyLinked.JSON()},
	} {
		cell := got[c.row]
		refs, _ := cell[people.Id.String()].([]interface{})
		last, _ := refs[len(refs)-1].(map[string]interface{})
		if len(refs) != maxShownLinks+1 || last["type"] != "more" || last["label"] != c.more {
			t.Errorf("%d links: %d refs, the last %v", c.n, len(refs), last)
		}
		if fmt.Sprint(cell[count.Id.String()]) != fmt.Sprint(c.count) {
			t.Errorf("%d links counted %v, want %v", c.n, cell[count.Id.String()], c.count)
		}
		if fmt.Sprint(cell[total.Id.String()]) != fmt.Sprint(c.sum) {
			t.Errorf("%d links summed %v, want %v", c.n, cell[total.Id.String()], c.sum)
		}
		// A formula doesn't read a part of the names as all of them.
		if fmt.Sprint(cell[names.Id.String()]) != fmt.Sprint(errTooManyToRead.JSON()) {
			t.Errorf("%d links, a formula reading their names: %v", c.n, cell[names.Id.String()])
		}
	}
	// Totals grouped by those links say they fall short.
	res, _, err := AggregateTable(ctx, companies.Id, w.me, QuerySpec{Op: OpCount, GroupBy: "People"})
	if err != nil || !res.Truncated {
		t.Errorf("grouped by links past those shown: %+v, %v", res, err)
	}
	for _, b := range res.Buckets {
		if strings.Contains(b.Label, "more") {
			t.Errorf("a group for the links not shown: %q", b.Label)
		}
	}

	// A table with no room for another field linking to a table can't show
	// a new relation's links, nor can one linking to itself make two.
	full, _ := w.table("Full")
	for i := 0; i < maxComputed[model.FieldRelation]; i++ {
		w.field(full, fmt.Sprint("Link ", i), model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": companies.Id.String()})
	}
	if _, err := CreateField(ctx, companies.Id, FieldInput{Name: "Full", Type: model.FieldRelation, Config: map[string]interface{}{"relation_target": "table", "table_id": full.Id.String(), "two_way": true}}, w.me); err == nil {
		t.Error("a table past its limit got another field linking to a table")
	}
	self, _ := w.table("Self")
	for i := 0; i < maxComputed[model.FieldRelation]-1; i++ {
		w.field(self, fmt.Sprint("Link ", i), model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": companies.Id.String()})
	}
	if _, err := CreateField(ctx, self.Id, FieldInput{Name: "Parent", Type: model.FieldRelation, Config: map[string]interface{}{"relation_target": "table", "table_id": self.Id.String(), "two_way": true}}, w.me); err == nil {
		t.Error("a table one short of its limit linked to itself both ways")
	}
}

// links is how many links a row makes through a field.
func (w *relationWorld) links(field, from uuid.UUID) int {
	w.t.Helper()
	var n int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(w.ctx, `SELECT COUNT(*) FROM data_table_links WHERE field_id = $1 AND from_row = $2`, field, from).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// A new row's link cells link it, from either side. A row update leaves
// links as they are, so a copy of a row held from an earlier read, written
// back, neither undoes links made since nor puts back those removed. Anything
// in a link cell that isn't a row is refused.
func TestRelationWrites(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	vendors, vName := w.table("Vendors")
	budget, bName := w.table("Budget")
	vendor := w.field(budget, "Vendor", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String(), "two_way": true})
	back := w.otherSide(vendors, vendor)
	acme := w.row(vendors, map[string]interface{}{vName.Id.String(): "Acme"})
	globex := w.row(vendors, map[string]interface{}{vName.Id.String(): "Globex"})
	initech := w.row(vendors, map[string]interface{}{vName.Id.String(): "Initech"})
	booth := w.row(budget, map[string]interface{}{bName.Id.String(): "Booth", vendor.Id.String(): []interface{}{acme.String(), map[string]interface{}{"id": globex.String()}}})
	if got := labels(w.cells(budget, &w.me)[booth][vendor.Id.String()]); got != "Acme, Globex" {
		t.Fatalf("Booth made with %q", got)
	}

	// Held, then written back, from either side, after links changed.
	held := w.cells(budget, &w.me)[booth]
	heldAcme := w.cells(vendors, &w.me)[acme]
	if _, _, err := ChangeLinks(ctx, budget.Id, booth, vendor.Id, []uuid.UUID{initech}, []uuid.UUID{globex}, w.member); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ChangeLinks(ctx, vendors.Id, acme, back.Id, nil, []uuid.UUID{booth}, w.member); err != nil {
		t.Fatal(err)
	}
	held[bName.Id.String()] = "Booth 2"
	if _, err := UpdateRow(ctx, budget.Id, booth, RowInput{Values: held}, w.me); err != nil {
		t.Fatalf("write Booth back: %v", err)
	}
	heldAcme[vName.Id.String()] = "Acme Inc"
	if _, err := UpdateRow(ctx, vendors.Id, acme, RowInput{Values: heldAcme}, w.me); err != nil {
		t.Fatalf("write Acme back: %v", err)
	}
	if got := labels(w.cells(budget, &w.me)[booth][vendor.Id.String()]); got != "Initech" {
		t.Errorf("held copies written back left Booth's vendors as %q", got)
	}

	// An update naming a link cell, with anything at all, changes no links.
	for _, cell := range []interface{}{[]interface{}{acme.String()}, []interface{}{}, nil, []interface{}{"Acme"}} {
		if _, err := UpdateRow(ctx, budget.Id, booth, RowInput{Values: map[string]interface{}{bName.Id.String(): "Booth 3", vendor.Id.String(): cell}}, w.me); err != nil {
			t.Errorf("an update naming %v: %v", cell, err)
		}
	}
	if got := labels(w.cells(budget, &w.me)[booth][vendor.Id.String()]); got != "Initech" {
		t.Errorf("updates naming the link cell left %q", got)
	}

	// A new row with names, or rows of another table, is refused.
	for _, cell := range []interface{}{[]interface{}{"Acme"}, []interface{}{booth.String()}, []interface{}{acme.String(), 7}} {
		_, err := CreateRow(ctx, budget.Id, RowInput{Values: map[string]interface{}{bName.Id.String(): "Ads", vendor.Id.String(): cell}}, w.me)
		if err == nil || !strings.Contains(err.Error(), "Vendors") {
			t.Errorf("%v taken as links: %v", cell, err)
		}
	}
	// A row deleted since is passed over.
	gone := w.row(vendors, map[string]interface{}{vName.Id.String(): "Gone"})
	if err := DeleteRow(ctx, vendors.Id, gone, w.me); err != nil {
		t.Fatal(err)
	}
	film := w.row(budget, map[string]interface{}{bName.Id.String(): "Film", vendor.Id.String(): []interface{}{gone.String(), acme.String()}})
	if got := labels(w.cells(budget, &w.me)[film][vendor.Id.String()]); got != "Acme Inc" {
		t.Errorf("Film made with a row deleted since: %q", got)
	}
	// A change says what it did: not the links there already, rows deleted
	// since, or links gone already.
	_, done, err := ChangeLinks(ctx, budget.Id, booth, vendor.Id, []uuid.UUID{initech, globex, gone}, []uuid.UUID{acme}, w.me)
	if err != nil || done != (LinksChanged{Added: 1}) {
		t.Errorf("linking Initech (there already), Globex and a deleted row, unlinking Acme (not linked): %+v, %v", done, err)
	}
	if _, done, _ := ChangeLinks(ctx, budget.Id, booth, vendor.Id, nil, []uuid.UUID{globex}, w.me); done != (LinksChanged{Removed: 1}) {
		t.Errorf("unlinking Globex: %+v", done)
	}

	// From the other side: a new vendor linked from a budget row as it's made.
	ads := w.row(budget, map[string]interface{}{bName.Id.String(): "Ads"})
	hooli, err := CreateRow(ctx, vendors.Id, RowInput{Values: map[string]interface{}{vName.Id.String(): "Hooli", back.Id.String(): []interface{}{ads.String()}}}, w.me)
	if err != nil {
		t.Fatalf("a vendor made with a link from Ads: %v", err)
	}
	if got := labels(w.cells(budget, &w.me)[ads][vendor.Id.String()]); got != "Hooli" {
		t.Errorf("Ads's vendors: %q", got)
	}
	if got := labels(w.cells(vendors, &w.me)[hooli.Id][back.Id.String()]); got != "Ads" {
		t.Errorf("Hooli's budget rows: %q", got)
	}

	// A new row can't link past its links either, nor make another row pass
	// them.
	var rows []interface{}
	for i := 0; i < maxLinks; i++ {
		rows = append(rows, w.row(vendors, map[string]interface{}{vName.Id.String(): fmt.Sprint("B", i)}).String())
	}
	big := w.row(budget, map[string]interface{}{bName.Id.String(): "Big", vendor.Id.String(): rows})
	if n := w.links(vendor.Id, big); n != maxLinks {
		t.Errorf("Big has %d links", n)
	}
	if _, _, err := ChangeLinks(ctx, budget.Id, big, vendor.Id, []uuid.UUID{acme}, nil, w.me); err == nil {
		t.Error("a row linked past its links")
	}
	if _, err := CreateRow(ctx, vendors.Id, RowInput{Values: map[string]interface{}{vName.Id.String(): "One more", back.Id.String(): []interface{}{big.String()}}}, w.me); err == nil {
		t.Error("a new vendor took Big past its links")
	}
}

// A deleted row's links go with it, from both ends, so they don't fill a
// row's links, and links can't be made to a row as it's deleted.
func TestRelationDeletes(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	tags, tName := w.table("Tags")
	posts, pName := w.table("Posts")
	tag := w.field(posts, "Tags", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": tags.Id.String(), "two_way": true})
	post := w.row(posts, map[string]interface{}{pName.Id.String(): "Post"})
	var all []uuid.UUID
	for i := 0; i < maxLinks; i++ {
		all = append(all, w.row(tags, map[string]interface{}{tName.Id.String(): fmt.Sprint("T", i)}))
	}
	if _, _, err := ChangeLinks(ctx, posts.Id, post, tag.Id, all, nil, w.me); err != nil {
		t.Fatal(err)
	}
	for _, id := range all {
		if err := DeleteRow(ctx, tags.Id, id, w.me); err != nil {
			t.Fatal(err)
		}
	}
	if n := w.links(tag.Id, post); n != 0 {
		t.Errorf("%d links to deleted tags", n)
	}
	fresh := w.row(tags, map[string]interface{}{tName.Id.String(): "Fresh"})
	if _, _, err := ChangeLinks(ctx, posts.Id, post, tag.Id, []uuid.UUID{fresh}, nil, w.me); err != nil {
		t.Errorf("a post whose tags were deleted can't link a new one: %v", err)
	}
	// From the other end: a deleted post leaves Fresh with no links back.
	if err := DeleteRow(ctx, posts.Id, post, w.me); err != nil {
		t.Fatal(err)
	}
	if has, _ := model.RowHasLinks(ctx, fresh); has {
		t.Error("a deleted post's link outlived it")
	}
	// The tables mostly added to are vacuumed as the server does it, outside
	// a transaction: the links, and the row ledger's change sets and changes.
	for _, table := range model.InsertMostly {
		if _, err := model.TuplesChanged(ctx, table); err != nil {
			t.Errorf("counting %s's changes: %v", table, err)
		}
		if err := model.Vacuum(ctx, table); err != nil {
			t.Errorf("vacuuming %s: %v", table, err)
		}
	}
	if err := model.Vacuum(ctx, "users"); err == nil {
		t.Error("a table that isn't the tables' own was vacuumed")
	}

	// Rows linked as they're deleted: each link is made, then deleted with
	// its row, or not made.
	post = w.row(posts, map[string]interface{}{pName.Id.String(): "Post 2"})
	for round := 0; round < 20; round++ {
		victim := w.row(tags, map[string]interface{}{tName.Id.String(): fmt.Sprint("V", round)})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, _ = ChangeLinks(ctx, posts.Id, post, tag.Id, []uuid.UUID{victim}, nil, w.me)
		}()
		go func() { defer wg.Done(); _ = DeleteRow(ctx, tags.Id, victim, w.me) }()
		wg.Wait()
		if has, _ := model.RowHasLinks(ctx, victim); has {
			t.Fatalf("round %d: a link to a deleted row", round)
		}
	}
}

// Links made at once can't take a row past its links.
func TestRelationCapRace(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	tags, tName := w.table("Tags")
	posts, pName := w.table("Posts")
	tag := w.field(posts, "Tags", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": tags.Id.String()})
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO data_table_rows (id, table_id, values, position)
		SELECT uuid_generate_v4(), $1, jsonb_build_object($2::text, 'T' || g), g FROM generate_series(1, $3::int) g`, tags.Id, tName.Id.String(), maxLinks+7); err != nil {
		t.Fatal(err)
	}
	rs, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `SELECT id FROM data_table_rows WHERE table_id = $1 ORDER BY position`, tags.Id)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for rs.Next() {
		var id uuid.UUID
		if err := rs.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rs.Close()
	for round := 0; round < 10; round++ {
		post := w.row(posts, map[string]interface{}{pName.Id.String(): "Post"})
		if _, _, err := ChangeLinks(ctx, posts.Id, post, tag.Id, ids[:maxLinks-1], nil, w.me); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(id uuid.UUID) {
				defer wg.Done()
				_, _, _ = ChangeLinks(ctx, posts.Id, post, tag.Id, []uuid.UUID{id}, nil, w.me)
			}(ids[maxLinks-1+i])
		}
		wg.Wait()
		if n := w.links(tag.Id, post); n != maxLinks {
			t.Fatalf("round %d: a post ended with %d links (cap %d)", round, n, maxLinks)
		}
	}
}

// At a modest scale, every row reads right: 500 customers with 50 orders
// each, the orders named and added up.
func TestRelationsAtScale(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	customers, cName := w.table("Customers")
	orders, oName := w.table("Orders")
	amount := w.field(orders, "Amount", model.FieldNumber, nil)
	cust := w.field(orders, "Customer", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": customers.Id.String(), "two_way": true})
	back := w.otherSide(customers, cust)
	count := w.field(customers, "Orders", model.FieldRollup, map[string]interface{}{"relation": back.Id.String(), "aggregate": "count"})
	revenue := w.field(customers, "Revenue", model.FieldRollup, map[string]interface{}{"relation": back.Id.String(), "field": amount.Id.String(), "aggregate": "sum"})
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO data_table_rows (id, table_id, values, position)
		SELECT uuid_generate_v4(), $1, jsonb_build_object($2::text, 'C' || g), g FROM generate_series(1, 500) g`, customers.Id, cName.Id.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO data_table_rows (id, table_id, values, position)
		SELECT uuid_generate_v4(), $1, jsonb_build_object($2::text, 'O' || g, $3::text, 10), g FROM generate_series(1, 25000) g`, orders.Id, oName.Id.String(), amount.Id.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO data_table_links (field_id, from_row, to_row)
		SELECT $1, o.id, c.id FROM
		  (SELECT id, row_number() OVER (ORDER BY position) % 500 AS k FROM data_table_rows WHERE table_id = $2) o
		  JOIN (SELECT id, row_number() OVER (ORDER BY position) % 500 AS k FROM data_table_rows WHERE table_id = $3) c USING (k)`, cust.Id, orders.Id, customers.Id); err != nil {
		t.Fatal(err)
	}
	_, _ = postgresInit.DBConn.SqlDB.ExecContext(ctx, `ANALYZE`)
	got := w.cells(customers, &w.me)
	if len(got) != 500 {
		t.Fatalf("%d customers", len(got))
	}
	for id, c := range got {
		refs, _ := c[back.Id.String()].([]interface{})
		if c[count.Id.String()] != float64(50) || c[revenue.Id.String()] != float64(500) || len(refs) != 50 || strings.Contains(labels(refs), "…") {
			t.Fatalf("customer %s: %v orders, revenue %v, %d shown: %.80s", id, c[count.Id.String()], c[revenue.Id.String()], len(refs), labels(refs))
		}
	}
	// The orders read their customer, named.
	for _, c := range w.cells(orders, &w.me) {
		if name := labels(c[cust.Id.String()]); !strings.HasPrefix(name, "C") {
			t.Fatalf("an order's customer: %q", name)
		}
	}
}

// A batch's patch writing a link cell sets the row's links through it, from
// either side: those it lists stay or are added, the others go, in the
// batch's one change set, which an undo takes back. A patch with only link
// cells writes no cell. A row that isn't the table's is refused, so no link
// is written from it.
func TestBatchPatchSetsLinks(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	vendors, vName := w.table("Vendors")
	budget, bName := w.table("Budget")
	vendor := w.field(budget, "Vendor", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String(), "two_way": true})
	back := w.otherSide(vendors, vendor)
	acme := w.row(vendors, map[string]interface{}{vName.Id.String(): "Acme"})
	globex := w.row(vendors, map[string]interface{}{vName.Id.String(): "Globex"})
	initech := w.row(vendors, map[string]interface{}{vName.Id.String(): "Initech"})
	booth := w.row(budget, map[string]interface{}{bName.Id.String(): "Booth", vendor.Id.String(): []interface{}{acme.String(), globex.String()}})
	stage := w.row(budget, map[string]interface{}{bName.Id.String(): "Stage"})

	res, err := Batch(ctx, budget.Id, BatchInput{Kind: "import", Patch: []PatchInput{
		{Row: booth, Values: map[string]interface{}{vendor.Id.String(): []interface{}{globex.String(), initech.String()}}},
		{Row: stage, Values: map[string]interface{}{bName.Id.String(): "Main stage", vendor.Id.String(): []interface{}{map[string]interface{}{"id": acme.String()}}}},
	}}, w.me)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	cells := w.cells(budget, &w.me)
	if got := labels(cells[booth][vendor.Id.String()]); got != "Globex, Initech" {
		t.Errorf("Booth's vendors are %q", got)
	}
	if got := labels(cells[stage][vendor.Id.String()]); got != "Acme" || cells[stage][bName.Id.String()] != "Main stage" {
		t.Errorf("Stage is %v", cells[stage])
	}
	if got := labels(w.cells(vendors, &w.me)[acme][back.Id.String()]); got != "Main stage" {
		t.Errorf("Acme's other side shows %q", got)
	}

	// From the other side: Initech's cell set to Stage alone takes it off Booth.
	if _, err := Batch(ctx, vendors.Id, BatchInput{Patch: []PatchInput{{Row: initech, Values: map[string]interface{}{back.Id.String(): []interface{}{stage.String()}}}}}, w.me); err != nil {
		t.Fatalf("batch from the other side: %v", err)
	}
	cells = w.cells(budget, &w.me)
	if got := labels(cells[booth][vendor.Id.String()]); got != "Globex" {
		t.Errorf("after the other side's write, Booth's vendors are %q", got)
	}
	if got := labels(cells[stage][vendor.Id.String()]); got != "Acme, Initech" {
		t.Errorf("after the other side's write, Stage's vendors are %q", got)
	}

	// Undo takes the first batch's links back as they were.
	if _, err := Undo(ctx, vendors.Id, nil, w.me); err != nil {
		t.Fatalf("undo the other side: %v", err)
	}
	if _, err := Undo(ctx, budget.Id, &res.ChangeSet, w.me); err != nil {
		t.Fatalf("undo the batch: %v", err)
	}
	cells = w.cells(budget, &w.me)
	// An undo puts the links back, in the order it makes them.
	if got := sortedLabels(cells[booth][vendor.Id.String()]); got != "Acme, Globex" {
		t.Errorf("after undo, Booth's vendors are %q", got)
	}
	if got := labels(cells[stage][vendor.Id.String()]); got != "" || cells[stage][bName.Id.String()] != "Stage" {
		t.Errorf("after undo, Stage is %v", cells[stage])
	}

	// A row of another table can't be patched into links.
	if _, err := Batch(ctx, budget.Id, BatchInput{Patch: []PatchInput{{Row: acme, Values: map[string]interface{}{vendor.Id.String(): []interface{}{globex.String()}}}}}, w.me); err == nil {
		t.Error("a patch to another table's row was taken")
	}
	if n := w.links(vendor.Id, acme); n != 0 {
		t.Errorf("a refused patch wrote %d links from Acme", n)
	}

	// A batch's rows are read a group at a time: past a group each row still
	// gets its own links, and a row naming what isn't a vendor is refused by
	// itself, the batch with it.
	create := make([]RowInput, linkGroup+20)
	for i := range create {
		create[i] = RowInput{Values: map[string]interface{}{bName.Id.String(): fmt.Sprintf("Line %d", i)}}
	}
	made, err := Batch(ctx, budget.Id, BatchInput{Create: create}, w.me)
	if err != nil || len(made.Created) != len(create) {
		t.Fatalf("make the lines: %v", err)
	}
	vendorOf := func(i int) (uuid.UUID, string) {
		if i%2 == 1 {
			return globex, "Globex"
		}
		return acme, "Acme"
	}
	patch := make([]PatchInput, len(made.Created))
	for i, r := range made.Created {
		to, _ := vendorOf(i)
		patch[i] = PatchInput{Row: r.Id, Values: map[string]interface{}{vendor.Id.String(): []interface{}{to.String()}}}
	}
	if _, err := Batch(ctx, budget.Id, BatchInput{Patch: patch}, w.me); err != nil {
		t.Fatalf("a batch past a group: %v", err)
	}
	cells = w.cells(budget, &w.me)
	for i, r := range made.Created {
		if _, want := vendorOf(i); labels(cells[r.Id][vendor.Id.String()]) != want {
			t.Fatalf("line %d's vendor is %q, want %q", i, labels(cells[r.Id][vendor.Id.String()]), want)
		}
	}
	first, second := made.Created[0].Id, made.Created[1].Id
	_, err = Batch(ctx, budget.Id, BatchInput{Patch: []PatchInput{
		{Row: first, Values: map[string]interface{}{vendor.Id.String(): []interface{}{initech.String()}}},
		{Row: second, Values: map[string]interface{}{vendor.Id.String(): []interface{}{booth.String()}}},
	}}, w.me)
	var refused BatchErrors
	if !errors.As(err, &refused) || len(refused) != 1 || refused[0].Row != second || !strings.Contains(refused[0].Msg, "isn't one") {
		t.Fatalf("a row naming what isn't a vendor: %v", err)
	}
	if got := labels(w.cells(budget, &w.me)[first][vendor.Id.String()]); got != "Acme" {
		t.Errorf("a refused batch changed line 0's vendor to %q", got)
	}
}

// sortedLabels is a relation cell's links by name, in name order.
func sortedLabels(cell interface{}) string {
	names := strings.Split(labels(cell), ", ")
	sort.Strings(names)
	return strings.Join(names, ", ")
}
