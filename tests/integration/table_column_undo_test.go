//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// V-455: a column's change (made, renamed, deleted) is a change set of the
// row ledger, so an Undo pressed after it takes the column change back, never
// the change before it (a cell nobody touched since). A change of a column
// that rewrote its cells is still refused, and still doesn't reach past it.
// So is one of a column linking two tables. Undoing a column's change takes
// the right to change the table's columns, as the person holds it now.
func TestAColumnsChangeIsWhatUndoTakesBack(t *testing.T) {
	w := newTableWorld(t, "maya", "ravi")
	maya := w.people["maya"]
	ravi := w.people["ravi"]                                                 // a member: may write rows, not change columns
	raviAdmin := dataTableBusiness.Actor{UserID: ravi.UserID, IsAdmin: true} // the same person while a workspace admin
	tb := w.table("maya", "Booths")
	item := w.field("maya", tb, "Item", dataTableModel.FieldText, nil)

	made, err := dataTableBusiness.Batch(w.ctx, tb.Id, dataTableBusiness.BatchInput{Create: []dataTableBusiness.RowInput{
		{Values: map[string]interface{}{item: "Booth"}},
	}}, maya)
	if err != nil || len(made.Created) != 1 {
		t.Fatalf("a row: %v", err)
	}
	row := made.Created[0].Id
	edit := func(v string) {
		t.Helper()
		if _, err := dataTableBusiness.Batch(w.ctx, tb.Id, dataTableBusiness.BatchInput{Patch: []dataTableBusiness.PatchInput{
			{Row: row, Values: map[string]interface{}{item: v}},
		}}, maya); err != nil {
			t.Fatalf("edit to %s: %v", v, err)
		}
	}
	cell := func() interface{} { t.Helper(); return w.stored(tb, row)[item] }
	column := func(id string) *dataTableModel.Field {
		t.Helper()
		fields, err := dataTableModel.ListFields(w.ctx, tb.Id)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range fields {
			if f.Id.String() == id {
				return f
			}
		}
		return nil
	}
	undo := func() (*dataTableBusiness.UndoResult, error) {
		return dataTableBusiness.Undo(w.ctx, tb.Id, nil, maya)
	}
	id := func(s string) uuid.UUID { return uuid.MustParse(s) }

	t.Run("a rename is undone before the cell edit made before it, and redone after it", func(t *testing.T) {
		edit("Zeta")
		thing := "Thing"
		if _, err := dataTableBusiness.UpdateField(w.ctx, tb.Id, id(item), dataTableBusiness.FieldPatch{Name: &thing}, maya); err != nil {
			t.Fatal(err)
		}
		if _, err := undo(); err != nil {
			t.Fatalf("the undo: %v", err)
		}
		if f := column(item); f == nil || f.Name != "Item" {
			t.Fatalf("the undo left the column named %v, want Item", f)
		}
		if got := cell(); got != "Zeta" {
			t.Fatalf("the undo of a rename changed a cell nobody touched since: %v, want Zeta", got)
		}
		if _, err := undo(); err != nil || cell() != "Booth" {
			t.Fatalf("the second undo: %v, cell %v, want Booth", err, cell())
		}
		for _, want := range []string{"cell", "name"} {
			if _, err := dataTableBusiness.Redo(w.ctx, tb.Id, maya); err != nil {
				t.Fatalf("the redo of the %s: %v", want, err)
			}
		}
		if f := column(item); f == nil || f.Name != "Thing" || cell() != "Zeta" {
			t.Fatalf("after two redos: column %v, cell %v; want Thing, Zeta", f, cell())
		}
	})

	t.Run("a column made goes to the trash on undo, and comes back on redo", func(t *testing.T) {
		edit("Kappa")
		notes := w.field("maya", tb, "Notes", dataTableModel.FieldText, nil)
		if _, err := undo(); err != nil {
			t.Fatalf("the undo: %v", err)
		}
		if column(notes) != nil || cell() != "Kappa" {
			t.Fatalf("after the undo: the new column is %v, the cell %v; want it gone and Kappa", column(notes), cell())
		}
		if _, err := dataTableBusiness.Redo(w.ctx, tb.Id, maya); err != nil || column(notes) == nil {
			t.Fatalf("the redo didn't bring the column back: %v", err)
		}
	})

	t.Run("a column deleted comes back with its cells on undo", func(t *testing.T) {
		size := w.field("maya", tb, "Size", dataTableModel.FieldText, nil)
		if _, err := dataTableBusiness.Batch(w.ctx, tb.Id, dataTableBusiness.BatchInput{Patch: []dataTableBusiness.PatchInput{
			{Row: row, Values: map[string]interface{}{size: "Large"}},
		}}, maya); err != nil {
			t.Fatal(err)
		}
		if err := dataTableBusiness.DeleteField(w.ctx, tb.Id, id(size), maya); err != nil {
			t.Fatal(err)
		}
		if _, err := undo(); err != nil {
			t.Fatalf("the undo: %v", err)
		}
		if column(size) == nil || w.stored(tb, row)[size] != "Large" {
			t.Fatalf("the undo didn't bring the column back whole: %v, %v", column(size), w.stored(tb, row)[size])
		}
	})

	t.Run("a type change that rewrote cells is refused, and doesn't reach past it", func(t *testing.T) {
		count := w.field("maya", tb, "Count", dataTableModel.FieldText, nil)
		if _, err := dataTableBusiness.Batch(w.ctx, tb.Id, dataTableBusiness.BatchInput{Patch: []dataTableBusiness.PatchInput{
			{Row: row, Values: map[string]interface{}{count: "12"}},
		}}, maya); err != nil {
			t.Fatal(err)
		}
		edit("Omega")
		number := dataTableModel.FieldNumber
		if n, err := dataTableBusiness.UpdateField(w.ctx, tb.Id, id(count), dataTableBusiness.FieldPatch{Type: &number}, maya); err != nil || n != 1 {
			t.Fatalf("the type change rewrote %d cells: %v, want one", n, err)
		}
		if _, err := undo(); err == nil || !strings.Contains(err.Error(), "changing a field") {
			t.Fatalf("the undo of a type change that rewrote cells: %v, want the field-change refusal", err)
		}
		if got := cell(); got != "Omega" {
			t.Fatalf("the refused undo reached past it: the cell is %v, want Omega", got)
		}
	})

	t.Run("a column someone changed since is left as they left it", func(t *testing.T) {
		size := w.field("maya", tb, "Weight", dataTableModel.FieldText, nil)
		bulk, volume := "Bulk", "Volume"
		if _, err := dataTableBusiness.UpdateField(w.ctx, tb.Id, id(size), dataTableBusiness.FieldPatch{Name: &bulk}, maya); err != nil {
			t.Fatal(err)
		}
		if _, err := dataTableBusiness.UpdateField(w.ctx, tb.Id, id(size), dataTableBusiness.FieldPatch{Name: &volume}, raviAdmin); err != nil {
			t.Fatal(err)
		}
		r, err := undo()
		if err != nil || r.Skipped != 1 || r.Changes != 0 {
			t.Fatalf("the undo of a rename renamed again since: %+v, %v; want one skipped", r, err)
		}
		if f := column(size); f == nil || f.Name != "Volume" {
			t.Fatalf("the undo wrote over the later rename: %v", f)
		}
	})

	t.Run("a column linking two tables can't be undone, and the undo doesn't reach past it", func(t *testing.T) {
		vendors := w.table("maya", "Vendors")
		edit("Sigma")
		link := w.field("maya", tb, "Vendor", dataTableModel.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String()})
		if _, err := undo(); err == nil || !strings.Contains(err.Error(), "links two tables") {
			t.Fatalf("the undo of a column linking two tables: %v, want it refused", err)
		}
		if column(link) == nil || cell() != "Sigma" {
			t.Fatalf("the refused undo changed something: the column %v, the cell %v; want it there and Sigma", column(link), cell())
		}
	})

	t.Run("a column brought back from the trash goes back on undo", func(t *testing.T) {
		shade := w.field("maya", tb, "Shade", dataTableModel.FieldText, nil)
		if err := dataTableBusiness.DeleteField(w.ctx, tb.Id, id(shade), maya); err != nil {
			t.Fatal(err)
		}
		if _, err := dataTableBusiness.RestoreFromTrash(w.ctx, tb.Id, dataTableBusiness.RestoreInput{Fields: []uuid.UUID{id(shade)}}, maya); err != nil || column(shade) == nil {
			t.Fatalf("the restore: %v", err)
		}
		if _, err := undo(); err != nil {
			t.Fatalf("the undo: %v", err)
		}
		if column(shade) != nil {
			t.Fatal("the undo of a restore left the column out of the trash")
		}
	})

	t.Run("an undo of a column's change takes the right to change the columns, as it is now", func(t *testing.T) {
		if _, err := dataTableBusiness.Batch(w.ctx, tb.Id, dataTableBusiness.BatchInput{Patch: []dataTableBusiness.PatchInput{
			{Row: row, Values: map[string]interface{}{item: "Tau"}},
		}}, ravi); err != nil {
			t.Fatalf("a member's edit: %v", err)
		}
		label := "Label"
		if _, err := dataTableBusiness.UpdateField(w.ctx, tb.Id, id(item), dataTableBusiness.FieldPatch{Name: &label}, raviAdmin); err != nil {
			t.Fatal(err)
		}
		if _, err := dataTableBusiness.Undo(w.ctx, tb.Id, nil, ravi); err == nil || !strings.Contains(err.Error(), "not authorized") {
			t.Fatalf("an undo of a column's change by someone who may no longer change columns: %v, want it refused", err)
		}
		if f := column(item); f == nil || f.Name != "Label" || cell() != "Tau" {
			t.Fatalf("the refused undo changed something: column %v, cell %v; want Label, Tau", f, cell())
		}
		if _, err := dataTableBusiness.Undo(w.ctx, tb.Id, nil, raviAdmin); err != nil {
			t.Fatalf("the admin's undo: %v", err)
		}
		if f := column(item); f == nil || f.Name != "Thing" || cell() != "Tau" {
			t.Fatalf("after the admin's undo: column %v, cell %v; want Thing, Tau", f, cell())
		}
	})

	t.Run("a column made the primary stays when its making is undone", func(t *testing.T) {
		// Made at the front, where the primary goes: making it the primary
		// moves it nowhere, so the column is just as its making left it and
		// only the primary's own rule keeps the undo from trashing it (made
		// at the end, the move to the front would skip it as changed since).
		f, err := dataTableBusiness.CreateField(w.ctx, tb.Id, dataTableBusiness.FieldInput{Name: "Title", Type: dataTableModel.FieldText, Position: -1000}, maya)
		if err != nil {
			t.Fatalf("a column at the front: %v", err)
		}
		title := f.Id.String()
		yes := true
		if _, err := dataTableBusiness.UpdateField(w.ctx, tb.Id, id(title), dataTableBusiness.FieldPatch{Primary: &yes}, maya); err != nil {
			t.Fatalf("making it the primary: %v", err)
		}
		r, err := undo()
		if err != nil || r.Skipped != 1 || r.Changes != 0 {
			t.Fatalf("the undo of the primary's making: %+v, %v; want one skipped", r, err)
		}
		if f := column(title); f == nil || !f.Primary {
			t.Fatalf("the undo trashed the table's primary: %v", f)
		}
	})

	t.Run("a column's change reads none of the table's rows in its write", func(t *testing.T) {
		big := w.table("maya", "Stands")
		label := w.field("maya", big, "Label", dataTableModel.FieldText, nil)
		for i := 0; i < 20; i++ {
			rows := make([]dataTableBusiness.RowInput, 100)
			for j := range rows {
				rows[j] = dataTableBusiness.RowInput{Values: map[string]interface{}{label: "Stand"}}
			}
			if _, err := dataTableBusiness.Batch(w.ctx, big.Id, dataTableBusiness.BatchInput{Create: rows}, maya); err != nil {
				t.Fatalf("rows: %v", err)
			}
		}
		renamed := "Sign"
		before := rowsRead(w.ctx, t)
		if _, err := dataTableBusiness.UpdateField(w.ctx, big.Id, id(label), dataTableBusiness.FieldPatch{Name: &renamed}, maya); err != nil {
			t.Fatalf("the rename: %v", err)
		}
		if n := rowsRead(w.ctx, t) - before; n >= 2000 {
			t.Fatalf("a rename of a column of 2,000 rows read %d rows: its change set walked the table in its write", n)
		}
	})
}

// rowsRead is how many of the rows Postgres has read, through or by index,
// once its counts have held still for a second (a backend reports a
// transaction's counts up to half a second after it ends, later when loaded).
func rowsRead(ctx context.Context, t *testing.T) int64 {
	t.Helper()
	once := func() int64 {
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
			t.Fatal(err)
		}
		var n int64
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT COALESCE(SUM(seq_tup_read + COALESCE(idx_tup_fetch, 0)), 0)
			FROM pg_stat_user_tables WHERE relname = 'data_table_rows'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	last, since, start := once(), time.Now(), time.Now()
	for time.Since(since) < time.Second {
		if time.Since(start) > 20*time.Second {
			t.Fatal("the counts never held still")
		}
		time.Sleep(100 * time.Millisecond)
		if n := once(); n != last {
			last, since = n, time.Now()
		}
	}
	return last
}

// A column retyped so that every cell is kept as it was (text of ISO days to
// a date, a select to text) rewrites no cell, so its change set has no row:
// its cells' entries are still made again, as the new type's, once it
// commits, as Row Query's sorts and filters read them (V-455, CR-211 row
// 2494 F3; CR-211's probe zz_cr211_retype_kept_test.go).
func TestARetypeThatKeepsEveryCellIndexesItsColumnAgain(t *testing.T) {
	w := newTableWorld(t, "amara")
	amara := w.people["amara"]
	tb := w.table("amara", "Dates")
	entries := func(field, col string) int {
		t.Helper()
		var n int
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM data_table_cell_index WHERE field_id = $1::uuid AND `+col+` IS NOT NULL`, field).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	fill := func(field string, values ...string) {
		t.Helper()
		var rows []dataTableBusiness.RowInput
		for _, v := range values {
			rows = append(rows, dataTableBusiness.RowInput{Values: map[string]interface{}{field: v}})
		}
		if _, err := dataTableBusiness.Batch(w.ctx, tb.Id, dataTableBusiness.BatchInput{Create: rows}, amara); err != nil {
			t.Fatal(err)
		}
	}
	retype := func(field, to string) {
		t.Helper()
		n, err := dataTableBusiness.UpdateField(w.ctx, tb.Id, uuid.MustParse(field), dataTableBusiness.FieldPatch{Type: &to}, amara)
		if err != nil || n != 0 {
			t.Fatalf("retype to %s: %d cells rewritten, %v; want none rewritten", to, n, err)
		}
	}

	when := w.field("amara", tb, "When", dataTableModel.FieldText, nil)
	fill(when, "2026-11-03", "2026-11-01", "2026-11-02")
	retype(when, dataTableModel.FieldDate)
	if d := entries(when, "d"); d != 3 {
		t.Errorf("a text column of ISO days retyped to a date has %d date entries (d), want 3: its entries are still text's", d)
	}

	stage := w.field("amara", tb, "Stage", dataTableModel.FieldSelect,
		map[string]interface{}{"options": []map[string]interface{}{{"label": "Open"}, {"label": "Won"}}})
	fill(stage, "Won", "Open")
	if p := entries(stage, "n"); p != 2 {
		t.Fatalf("a select's entries hold %d option places (n), want 2", p)
	}
	retype(stage, dataTableModel.FieldText)
	if p := entries(stage, "n"); p != 0 {
		t.Errorf("a select retyped to text still has %d option places (n), want none: its entries are still the select's", p)
	}
}

// A change to how a column looks (an option's colour, a text's wrap) rewrites
// no cell and changes nothing its entries are made from, so it walks no row;
// options only reordered move their places (n) alone, again walking no row
// (V-455, CR-212 row 2500 F4: such a change walked a 100,000-row table, 6.5
// s, where beta did nothing).
func TestAChangeToHowAColumnLooksWalksNoRow(t *testing.T) {
	w := newTableWorld(t, "maya")
	maya := w.people["maya"]
	tb := w.table("maya", "Deals")
	stage := w.field("maya", tb, "Stage", dataTableModel.FieldSelect, map[string]interface{}{"options": []map[string]interface{}{
		{"label": "Open", "color": "blue"}, {"label": "Won", "color": "green"}}})
	note := w.field("maya", tb, "Note", dataTableModel.FieldText, nil)
	for i := 0; i < 20; i++ {
		rows := make([]dataTableBusiness.RowInput, 100)
		for j := range rows {
			rows[j] = dataTableBusiness.RowInput{Values: map[string]interface{}{stage: []string{"Open", "Won"}[j%2], note: "Call"}}
		}
		if _, err := dataTableBusiness.Batch(w.ctx, tb.Id, dataTableBusiness.BatchInput{Create: rows}, maya); err != nil {
			t.Fatalf("rows: %v", err)
		}
	}
	// options is Stage's options as the table keeps them (ids included).
	options := func() []map[string]interface{} {
		t.Helper()
		var cfg struct {
			Options []map[string]interface{} `json:"options"`
		}
		var raw string
		if err := w.env.PG.QueryRow(`SELECT config FROM data_table_fields WHERE id = $1::uuid`, stage).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil || len(cfg.Options) != 2 {
			t.Fatalf("Stage's options: %s, %v", raw, err)
		}
		return cfg.Options
	}
	// wonPlace is the place (n) a Won entry holds, the same for each.
	wonPlace := func() float64 {
		t.Helper()
		var lo, hi float64
		if err := w.env.PG.QueryRow(`SELECT min(n), max(n) FROM data_table_cell_index WHERE field_id = $1::uuid AND t = 'won'`, stage).Scan(&lo, &hi); err != nil || lo != hi {
			t.Fatalf("Won's places: %v to %v, %v", lo, hi, err)
		}
		return lo
	}
	change := func(what, field string, p dataTableBusiness.FieldPatch) {
		t.Helper()
		before := rowsRead(w.ctx, t)
		if n, err := dataTableBusiness.UpdateField(w.ctx, tb.Id, uuid.MustParse(field), p, maya); err != nil || n != 0 {
			t.Fatalf("%s: %d cells rewritten, %v; want none rewritten", what, n, err)
		}
		if n := rowsRead(w.ctx, t) - before; n >= 2000 {
			t.Errorf("%s on a column of 2,000 rows read %d rows: it walked the table", what, n)
		}
	}

	if p := wonPlace(); p != 2 {
		t.Fatalf("Won is the second option, its place %v", p)
	}
	recoloured := options()
	recoloured[0]["color"], recoloured[1]["color"] = "red", "purple"
	change("an option recoloured", stage, dataTableBusiness.FieldPatch{Config: map[string]interface{}{"options": recoloured}})
	if p := wonPlace(); p != 2 {
		t.Errorf("an option recoloured moved Won's place to %v, want 2", p)
	}
	change("a text column's wrap", note, dataTableBusiness.FieldPatch{Config: map[string]interface{}{"wrap": true}})
	moved := options()
	moved[0], moved[1] = moved[1], moved[0]
	change("the options reordered", stage, dataTableBusiness.FieldPatch{Config: map[string]interface{}{"options": moved}})
	if p := wonPlace(); p != 1 {
		t.Errorf("Won moved first has its entries' place %v, want 1", p)
	}
}
