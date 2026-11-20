//go:build integration

package business

import (
	"strings"
	"testing"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// A base of two tables comes across whole: its plain fields, the two sides of
// a link as one relation shown from both, a rollup over it and a formula over
// that, every row (a cell no kind takes left out and said, never its row),
// each row's links once both tables' rows are there, and a view. An import
// that fails part way takes away what it made.
func TestImportBaseBringsATwoTableBase(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	vendors := ImportTable{Key: "tblV", Name: "Vendors", Fields: []ImportField{
		{Key: "fName", Name: "Name", Type: model.FieldText},
		{Key: "fRating", Name: "Rating", Type: model.FieldNumber},
		{Key: "fOrders", Name: "Orders", Type: model.FieldRelation, LinksTo: "tblO", Pair: "fVendor"},
		{Key: "fSpend", Name: "Spend", Type: model.FieldRollup, Through: "fOrders", Of: "fAmount", Aggregate: "sum"},
		{Key: "fBig", Name: "Big spender", Type: model.FieldFormula, Formula: "IF({Spend} > 100, \"yes\", \"no\")"},
	}}
	orders := ImportTable{Key: "tblO", Name: "Orders", Fields: []ImportField{
		{Key: "fTitle", Name: "Title", Type: model.FieldText},
		{Key: "fAmount", Name: "Amount", Type: model.FieldNumber},
		{Key: "fVendor", Name: "Vendor", Type: model.FieldRelation, LinksTo: "tblV", Pair: "fOrders"},
	}, Views: []ImportView{{Name: "All orders", Type: "grid"}}}
	vendors.Rows = func(page func([]ImportRow) error) error {
		return page([]ImportRow{
			{Key: "recA", Cells: map[string]any{"fName": "Acme", "fRating": 4.0}, Links: map[string][]string{"fOrders": {"recO1", "recO2"}}},
			{Key: "recG", Cells: map[string]any{"fName": "Globex", "fRating": "lots"}},
		})
	}
	orders.Rows = func(page func([]ImportRow) error) error {
		if err := page([]ImportRow{{Key: "recO1", Cells: map[string]any{"fTitle": "Booth", "fAmount": 90.0}}}); err != nil {
			return err
		}
		return page([]ImportRow{{Key: "recO2", Cells: map[string]any{"fTitle": "Banners", "fAmount": 60.0}, Links: map[string][]string{"fVendor": {"recA"}}}})
	}
	made, err := ImportBaseAs(ctx, w.me, ImportBase{Tables: []ImportTable{vendors, orders}})
	if err != nil {
		t.Fatalf("import: %v %+v", err, made.Notes)
	}
	if made.RowsMade != 4 || len(made.Tables) != 2 {
		t.Fatalf("made %d rows in %d tables", made.RowsMade, len(made.Tables))
	}
	var leftOut []string
	for _, n := range made.Notes {
		leftOut = append(leftOut, n.Code+" "+n.Row+" "+n.Field)
	}
	if strings.Join(leftOut, ";") != "CELL_LEFT_OUT recG fRating" {
		t.Errorf("notes: %v", made.Notes)
	}
	vt, err := model.GetTableByID(ctx, made.Tables["tblV"])
	if err != nil {
		t.Fatal(err)
	}
	ot, _ := model.GetTableByID(ctx, made.Tables["tblO"])
	vcells := w.cells(vt, &w.me)
	acme := vcells[made.Rows["tblV/recA"]]
	if got := labels(acme[made.Fields["tblV/fOrders"].String()]); got != "Booth, Banners" {
		t.Errorf("Acme's orders: %q", got)
	}
	if acme[made.Fields["tblV/fSpend"].String()] != 150.0 || acme[made.Fields["tblV/fBig"].String()] != "yes" {
		t.Errorf("Acme's spend and formula: %v %v", acme[made.Fields["tblV/fSpend"].String()], acme[made.Fields["tblV/fBig"].String()])
	}
	globex := vcells[made.Rows["tblV/recG"]]
	if globex[made.Fields["tblV/fName"].String()] != "Globex" || globex[made.Fields["tblV/fRating"].String()] != nil {
		t.Errorf("Globex: %v", globex)
	}
	ocells := w.cells(ot, &w.me)
	if got := labels(ocells[made.Rows["tblO/recO1"]][made.Fields["tblO/fVendor"].String()]); got != "Acme" {
		t.Errorf("Booth's vendor: %q", got)
	}
	// The pair is one relation: Orders' side shows Vendors' links, named as
	// the source named it.
	fields, _ := model.ListFields(ctx, ot.Id)
	for _, f := range fields {
		if f.Id == made.Fields["tblO/fVendor"] {
			if l, ok := linkOf(f); !ok || l.inverseOf != made.Fields["tblV/fOrders"].String() || f.Name != "Vendor" {
				t.Errorf("Orders' Vendor: %+v", f)
			}
		}
	}
	views, _ := model.ListViews(ctx, ot.Id, uuid.Nil)
	found := false
	for _, v := range views {
		found = found || v.Name == "All orders"
	}
	if !found {
		t.Errorf("Orders' views: %+v", views)
	}

	// Part way: a page that fails takes the base away again.
	broken := ImportTable{Key: "tblX", Name: "Broken", Fields: []ImportField{{Key: "f", Name: "Name", Type: model.FieldText}},
		Rows: func(page func([]ImportRow) error) error { return errBroken }}
	gone, err := ImportBaseAs(ctx, w.me, ImportBase{Tables: []ImportTable{broken}})
	if err == nil {
		t.Fatal("a failing source imported")
	}
	if tb, _ := model.GetTableByID(ctx, gone.Tables["tblX"]); tb != nil && tb.DeletedAt == nil {
		t.Error("a failed import left its table")
	}
}

var errBroken = &importTestError{"the source stopped"}

type importTestError struct{ s string }

func (e *importTestError) Error() string { return e.s }

// A link a source names only on the side of a pair that shows the other's
// links (the side made second) is brought, and reads from both sides; the
// source's own rows are left as they were.
func TestImportBaseBringsLinksNamedOnEitherSide(t *testing.T) {
	w := newRelationWorld(t)
	teams := ImportTable{Key: "tblT", Name: "Teams", Fields: []ImportField{
		{Key: "fName", Name: "Team", Type: model.FieldText},
		{Key: "fPeople", Name: "People", Type: model.FieldRelation, LinksTo: "tblP", Pair: "fTeam"},
		{Key: "fSize", Name: "Size", Type: model.FieldRollup, Through: "fPeople", Of: "fName", Aggregate: "count"},
	}}
	people := ImportTable{Key: "tblP", Name: "People", Fields: []ImportField{
		{Key: "fName", Name: "Name", Type: model.FieldText},
		{Key: "fTeam", Name: "Team", Type: model.FieldRelation, LinksTo: "tblT", Pair: "fPeople"},
	}}
	teams.Rows = func(page func([]ImportRow) error) error {
		return page([]ImportRow{{Key: "t1", Cells: map[string]any{"fName": "Design"}}, {Key: "t2", Cells: map[string]any{"fName": "Sales"}}})
	}
	theirs := map[string][]string{"fTeam": {"t1"}}
	people.Rows = func(page func([]ImportRow) error) error {
		return page([]ImportRow{
			{Key: "p1", Cells: map[string]any{"fName": "Ada"}, Links: theirs},
			{Key: "p2", Cells: map[string]any{"fName": "Bo"}, Links: map[string][]string{"fTeam": {"t1"}}},
			{Key: "p3", Cells: map[string]any{"fName": "Cy"}, Links: map[string][]string{"fTeam": {"t2"}}},
		})
	}
	made, err := ImportBaseAs(w.ctx, w.me, ImportBase{Tables: []ImportTable{teams, people}})
	if err != nil || len(made.Notes) != 0 {
		t.Fatalf("import: %v %+v", err, made.Notes)
	}
	tt, _ := model.GetTableByID(w.ctx, made.Tables["tblT"])
	pt, _ := model.GetTableByID(w.ctx, made.Tables["tblP"])
	tcells, pcells := w.cells(tt, &w.me), w.cells(pt, &w.me)
	design := tcells[made.Rows["tblT/t1"]]
	if got := labels(design[made.Fields["tblT/fPeople"].String()]); got != "Ada, Bo" || design[made.Fields["tblT/fSize"].String()] != 2.0 {
		t.Errorf("Design's people %q, size %v", got, design[made.Fields["tblT/fSize"].String()])
	}
	if got := labels(pcells[made.Rows["tblP/p3"]][made.Fields["tblP/fTeam"].String()]); got != "Sales" {
		t.Errorf("Cy's team: %q", got)
	}
	if len(theirs) != 1 || strings.Join(theirs["fTeam"], ",") != "t1" {
		t.Errorf("the source's links were written to: %v", theirs)
	}
}

// A files cell holds files of its own table: one uploaded to it is kept with
// its name, size and type; one of another place, or of another table, is
// refused, whoever writes; and an import keeps a source's file through Files.
func TestFilesCellsHoldTheirTablesFiles(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	tb, _ := w.table("Contracts")
	other, _ := w.table("Other")
	files := w.field(tb, "Files", model.FieldAttachment, nil)
	attach := func(srcKey, srcValue string) string {
		id := uuid.New()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, $3, $4, $5)`,
			id, "k/"+id.String(), srcKey, srcValue, w.me.UserID); err != nil {
			t.Fatal(err)
		}
		return id.String()
	}
	mine := attach("table", tb.Id.String())
	theirs := attach("table", other.Id.String())
	chat := attach("channel", tb.Id.String())
	row, err := CreateRow(ctx, tb.Id, RowInput{Values: map[string]interface{}{files.Id.String(): []interface{}{
		map[string]interface{}{"id": mine, "name": "plan.pdf", "size": 2048.0, "mime": "application/pdf"}}}}, w.me)
	if err != nil {
		t.Fatalf("a file of the table: %v", err)
	}
	cell := w.cells(tb, &w.me)[row.Id][files.Id.String()]
	list, _ := cell.([]interface{})
	if len(list) != 1 || list[0].(map[string]interface{})["name"] != "plan.pdf" {
		t.Errorf("the cell: %v", cell)
	}
	for name, id := range map[string]string{"another table's": theirs, "a channel's": chat} {
		if _, err := PatchRow(ctx, tb.Id, row.Id, map[string]interface{}{files.Id.String(): []interface{}{map[string]interface{}{"id": id, "name": "x"}}}, w.me); err == nil {
			t.Errorf("%s file was taken", name)
		}
	}

	// An import keeps a source's file through Files, as a file of the table.
	made, err := ImportBaseAs(ctx, w.me, ImportBase{
		Files: func(table uuid.UUID, f ImportFile) (fieldTypes.FileRef, bool) {
			if f.Name == "gone.png" {
				return fieldTypes.FileRef{}, false
			}
			return fieldTypes.FileRef{ID: attach("table", table.String()), Name: f.Name, Size: f.Size, Mime: f.Mime}, true
		},
		Tables: []ImportTable{{Key: "t", Name: "Imported", Fields: []ImportField{{Key: "n", Name: "Name", Type: model.FieldText}, {Key: "f", Name: "Files", Type: model.FieldAttachment}},
			Rows: func(page func([]ImportRow) error) error {
				return page([]ImportRow{{Key: "r", Cells: map[string]any{"n": "Booth", "f": []any{ImportFile{Name: "a.png", Size: 10, Mime: "image/png"}, ImportFile{Name: "gone.png"}}}}})
			}}},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	it, _ := model.GetTableByID(ctx, made.Tables["t"])
	got := w.cells(it, &w.me)[made.Rows["t/r"]][made.Fields["t/f"].String()]
	if l, _ := got.([]interface{}); len(l) != 1 || l[0].(map[string]interface{})["name"] != "a.png" {
		t.Errorf("the imported cell: %v", got)
	}
	if len(made.Notes) != 1 || made.Notes[0].Code != "FILE_LEFT_OUT" {
		t.Errorf("notes: %+v", made.Notes)
	}
}
