//go:build integration

package business

// Every table a reader can open as one workbook (gsheets-import-02: POST
// /tables/export), against Postgres 12 with every migration, two members and
// tables each of them can't open: a table its reader can't open has no sheet
// at all, not even its name, though the list it is read from names it by
// mistake; each table they can open is a sheet, in the list's order, of every
// field they may see in the table's order and every row they read, read as
// they read it (a link into a table they can't open is a private row), each
// cell of its kind as one table's export writes it; and an export past what
// one workbook holds, or of no table at all, is refused in words before any
// of it is written.
// Run: logs/tables-198/expall/integ.sh with PK='./business/DataTable/' and the tests' names.

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// workbookRead is an export of every table read back as an import reads a
// workbook: each sheet's name in order, hidden ones too, and each sheet's
// rows, the header first, by its name; and each sheet's XML, by its name.
type workbookRead struct {
	names []string
	// tables are the ids the export's row in the admin audit log names.
	tables []string
	rows   map[string][][]contentIOBusiness.Cell
	xml    map[string]string
}

// exportedTables is actor's export of every table, written whole and read
// back, with the table ids its audit row names.
func (w *relationWorld) exportedTables(actor Actor) workbookRead {
	w.t.Helper()
	x, err := PrepareTablesExport(w.ctx, actor)
	if err != nil {
		w.t.Fatalf("ready the export of every table: %v", err)
	}
	var b bytes.Buffer
	if _, err := x.WriteXLSX(w.ctx, &b); err != nil {
		w.t.Fatalf("write the export of every table: %v", err)
	}
	sheets, err := contentIOBusiness.ReadSheets(contentIOBusiness.Input{Filename: "tables.xlsx", Size: int64(b.Len()), File: bytes.NewReader(b.Bytes())})
	if err != nil {
		w.t.Fatalf("read the export back: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		w.t.Fatalf("open the export as a zip: %v", err)
	}
	out := workbookRead{tables: x.Tables(), rows: map[string][][]contentIOBusiness.Cell{}, xml: map[string]string{}}
	for i, s := range sheets {
		out.names = append(out.names, s.Name)
		var rows [][]contentIOBusiness.Cell
		if err := s.Rows(func(cells []contentIOBusiness.Cell) error {
			rows = append(rows, slices.Clone(cells))
			return nil
		}); err != nil {
			w.t.Fatalf("read the sheet %q: %v", s.Name, err)
		}
		out.rows[s.Name] = rows
		// The workbook's sheets are its parts in the order they were written.
		f, err := zr.Open(fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1))
		if err != nil {
			w.t.Fatalf("the sheet %q's part: %v", s.Name, err)
		}
		raw, _ := io.ReadAll(f)
		f.Close()
		out.xml[s.Name] = string(raw)
	}
	return out
}

// header is a sheet's first row's texts.
func header(rows [][]contentIOBusiness.Cell) []string {
	var out []string
	if len(rows) > 0 {
		for _, c := range rows[0] {
			out = append(out, c.Text)
		}
	}
	return out
}

// cellAt is a sheet's cell in row (1, the first below its header) under
// the header field, and whether there is one.
func cellAt(rows [][]contentIOBusiness.Cell, row int, field string) (contentIOBusiness.Cell, bool) {
	col := slices.Index(header(rows), field)
	if row >= len(rows) || col < 0 || col >= len(rows[row]) {
		return contentIOBusiness.Cell{}, false
	}
	return rows[row][col], true
}

// cellText is cellAt's text, "" for none.
func cellText(rows [][]contentIOBusiness.Cell, row int, field string) string {
	c, _ := cellAt(rows, row, field)
	return c.Text
}

// holds is whether any cell of any sheet of the workbook, its names among
// them, says text.
func (b workbookRead) holds(text string) bool {
	for _, n := range b.names {
		if strings.Contains(n, text) {
			return true
		}
		for _, r := range b.rows[n] {
			for _, c := range r {
				if strings.Contains(c.Text, text) {
					return true
				}
			}
		}
	}
	return false
}

// listedNames are the names of the tables the list gives actor, in its
// order: what an export of every table makes its sheets of.
func (w *relationWorld) listedNames(actor Actor) []string {
	w.t.Helper()
	list, err := ListTables(w.ctx, actor)
	if err != nil {
		w.t.Fatalf("list the tables: %v", err)
	}
	var out []string
	for _, t := range list {
		out = append(out, t.Name)
	}
	return out
}

// listedWrongly has the export's list name every table, as an admin's does,
// whoever reads it, until the test ends: a list that errs.
func listedWrongly(t *testing.T) {
	was := tablesListed
	tablesListed = func(ctx context.Context, a Actor) ([]*model.DataTable, error) {
		return model.ListTablesVisibleTo(ctx, a.UserID, true)
	}
	t.Cleanup(func() { tablesListed = was })
}

// Two members, each with a table the other can't open, and a table both can:
// each one's workbook has a sheet for each table they can open, in the list's
// order, with its rows, and none for the other's, not even its name or a row
// of it; a link into the other's table reads as a private row. So it stays
// when the list names every table by mistake: each table is asked again
// before its sheet is made. An admin opens every table, as one table's export
// lets them, and has every sheet. The sheets are exactly the tables whose
// own export the reader may make.
func TestAnExportOfEveryTableHoldsOnlyTheTablesTheReaderCanOpen(t *testing.T) {
	w := newRelationWorld(t)
	pay, payName := w.table("Pay")
	asha := w.row(pay, map[string]interface{}{payName.Id.String(): "Asha"})
	vendors, vName := w.table("Vendors")
	payee := w.field(vendors, "Owed to", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": pay.Id.String()})
	w.row(vendors, map[string]interface{}{vName.Id.String(): "Acme", payee.Id.String(): []interface{}{asha.String()}})
	if _, _, err := UpdateTable(w.ctx, pay.Id, TablePatch{Visibility: optStr(model.VisibilityPrivate)}, w.me); err != nil {
		t.Fatalf("make Pay private: %v", err)
	}
	mine, err := CreateTable(w.ctx, TableInput{Name: "Mine", Visibility: model.VisibilityPrivate}, w.member)
	if err != nil {
		t.Fatalf("the member's own table: %v", err)
	}
	mineFields, _ := model.ListFields(w.ctx, mine.Id)
	if _, err := CreateRow(w.ctx, mine.Id, RowInput{Values: map[string]interface{}{mineFields[0].Id.String(): "Notes to self"}}, w.member); err != nil {
		t.Fatalf("a row of the member's own table: %v", err)
	}
	admin := Actor{UserID: w.me.UserID, IsAdmin: true}

	check := func(when string) {
		t.Helper()
		got := w.exportedTables(w.member)
		if want := w.listedNames(w.member); !slices.Equal(got.names, want) || !slices.Equal(want, []string{"Mine", "Vendors"}) && !slices.Equal(want, []string{"Vendors", "Mine"}) {
			t.Errorf("%s: the member's sheets %q; want the tables they can open, in the list's order (%q)", when, got.names, want)
		}
		if got.holds("Pay") || got.holds("Asha") {
			t.Errorf("%s: the member's workbook names Pay or holds its row: sheets %q", when, got.names)
		}
		if v := got.rows["Vendors"]; len(v) != 2 || cellText(v, 1, "Owed to") != "Private row" {
			t.Errorf("%s: the member's Vendors sheet %v; want Acme, what it is owed to a private row", when, v)
		}
		if m := got.rows["Mine"]; len(m) != 2 || textsOf(m, 0)[0] != "Notes to self" {
			t.Errorf("%s: the member's own table's sheet %v; want its row", when, m)
		}

		owner := w.exportedTables(w.me)
		if want := w.listedNames(w.me); !slices.Equal(owner.names, want) || slices.Contains(owner.names, "Mine") || owner.holds("Notes to self") {
			t.Errorf("%s: the owner's sheets %q; want the tables they can open (%q), never the member's own", when, owner.names, want)
		}
		if p := owner.rows["Pay"]; len(p) != 2 || textsOf(p, 0)[0] != "Asha" {
			t.Errorf("%s: the owner's Pay sheet %v; want Asha", when, p)
		}
		if v := owner.rows["Vendors"]; len(v) != 2 || cellText(v, 1, "Owed to") != "Asha" {
			t.Errorf("%s: the owner's Vendors sheet %v; want what it is owed to by name", when, v)
		}

		all := w.exportedTables(admin)
		if want := w.listedNames(admin); !slices.Equal(all.names, want) || len(want) != 3 {
			t.Errorf("%s: an admin's sheets %q; want every table (%q)", when, all.names, want)
		}

		// A table has a sheet exactly when its own export may be made, and
		// the export's audit row names exactly the sheets' tables, in order.
		ids := map[string]string{pay.Name: pay.Id.String(), vendors.Name: vendors.Id.String(), mine.Name: mine.Id.String()}
		for _, reader := range []Actor{w.member, w.me, admin} {
			got := w.exportedTables(reader)
			sheets, named := got.names, []string{}
			for _, n := range sheets {
				named = append(named, ids[n])
			}
			if !slices.Equal(got.tables, named) {
				t.Errorf("%s: %v's export names the tables %q in the audit log; want its sheets' %q", when, reader, got.tables, named)
			}
			for _, tb := range []*model.DataTable{pay, vendors, mine} {
				_, err := PrepareExport(w.ctx, tb.Id, reader, ExportInput{Fields: []uuid.UUID{liveFieldsOf(t, w, tb.Id)[0].Id}})
				if opens := err == nil; opens != slices.Contains(sheets, tb.Name) {
					t.Errorf("%s: %v's sheet of %s: %v; its own export: %v", when, reader, tb.Name, slices.Contains(sheets, tb.Name), err)
				}
			}
		}
	}
	check("listed as they may see")
	listedWrongly(t)
	check("listed wrongly, every table named")
}

// In a workbook of several tables, each sheet keeps its cells' kinds as one
// table's export does: an amount with its currency, a day as a day, a
// checkbox as one, a link by its row's name, a choice as text under a
// dropdown of the field's options; its header is every live field of the
// table in the table's order, a field in the trash never among them, nor its
// values. A sheet's name is its table's, made one Excel holds: at most 31
// characters, none of : \ / ? * [ ], and no other sheet's in any case.
func TestEachSheetOfAnExportOfEveryTableKeepsItsKinds(t *testing.T) {
	w := newRelationWorld(t)
	vendors, vName := w.table("Vendors")
	tier := w.field(vendors, "Tier", model.FieldSelect, map[string]interface{}{"options": []interface{}{"Gold", "Silver"}})
	acme := w.row(vendors, map[string]interface{}{vName.Id.String(): "Acme", tier.Id.String(): "Gold"})
	deals, name := w.table("Deals")
	amount := w.field(deals, "Amount", model.FieldNumber, map[string]interface{}{"format": map[string]interface{}{"currency": "INR", "decimals": 2}})
	due := w.field(deals, "Due", model.FieldDate, nil)
	done := w.field(deals, "Done", model.FieldCheckbox, nil)
	stage := w.field(deals, "Stage", model.FieldSelect, map[string]interface{}{"options": []interface{}{"Won", "Lost"}})
	vendor := w.field(deals, "Vendor", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String()})
	old := w.field(deals, "Old", model.FieldText, nil)
	w.row(deals, map[string]interface{}{
		name.Id.String(): "Booth", amount.Id.String(): 1250.5, due.Id.String(): "2026-10-23", done.Id.String(): true,
		stage.Id.String(): "Won", vendor.Id.String(): []interface{}{acme.String()}, old.Id.String(): "in the trash",
	})
	if err := DeleteField(w.ctx, deals.Id, old.Id, w.me); err != nil {
		t.Fatalf("put Old in the trash: %v", err)
	}
	slash, _ := w.table("Q3/Q4: plan")
	w.row(slash, map[string]interface{}{liveFieldsOf(t, w, slash.Id)[0].Id.String(): "slash row"})
	same, _ := w.table("q3 q4  plan")
	w.row(same, map[string]interface{}{liveFieldsOf(t, w, same.Id)[0].Id.String(): "same row"})
	long, _ := w.table("A table whose name is longer than a sheet's")
	w.row(long, map[string]interface{}{liveFieldsOf(t, w, long.Id)[0].Id.String(): "long row"})

	got := w.exportedTables(w.member)
	listed, err := ListTables(w.ctx, w.member)
	if err != nil || len(listed) != 5 || len(got.names) < 5 {
		t.Fatalf("the list %d, the sheets %q: %v; want five tables, a sheet each", len(listed), got.names, err)
	}
	seen := map[string]bool{}
	for i, tb := range listed {
		sheet := got.names[i]
		if len(sheet) > 31 || strings.ContainsAny(sheet, `:\/?*[]`) || seen[strings.ToLower(sheet)] {
			t.Errorf("the sheet of %q is named %q: not one Excel holds, or another sheet's", tb.Name, sheet)
		}
		seen[strings.ToLower(sheet)] = true
		var want []string
		for _, f := range liveFieldsOf(t, w, tb.Id) {
			want = append(want, f.Name)
		}
		if h := header(got.rows[sheet]); !slices.Equal(h, want) {
			t.Errorf("the sheet %q (of %q): header %q; want the table's live fields in its order, %q", sheet, tb.Name, h, want)
		}
	}
	if got.holds("Old") || got.holds("in the trash") {
		t.Errorf("a field in the trash, or its value, is in the workbook: %q", header(got.rows["Deals"]))
	}

	d := got.rows["Deals"]
	if len(d) != 2 {
		t.Fatalf("the Deals sheet: %v; want a header and Booth", d)
	}
	booth := d[1]
	at := func(field string) contentIOBusiness.Cell {
		c, _ := cellAt(d, 1, field)
		return c
	}
	for _, c := range []struct {
		what string
		ok   bool
	}{
		{"the name as text", at("Name").Kind == contentIOBusiness.CellText && at("Name").Text == "Booth"},
		{"the amount in rupees", at("Amount").Kind == contentIOBusiness.CellNumber && at("Amount").Number == 1250.5 && at("Amount").Currency == "INR"},
		{"the day", at("Due").Kind == contentIOBusiness.CellDate && at("Due").Time.Equal(time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC))},
		{"the ticked checkbox", at("Done").Kind == contentIOBusiness.CellBool && at("Done").Bool},
		{"the choice", at("Stage").Text == "Won"},
		{"the link by its row's name", at("Vendor").Text == "Acme"},
	} {
		if !c.ok {
			t.Errorf("Booth in a workbook of every table: %s isn't so: %+v", c.what, booth)
		}
	}
	if v := got.rows["Vendors"]; len(v) != 2 || cellText(v, 1, "Tier") != "Gold" {
		t.Errorf("the Vendors sheet %v; want Acme, Gold", v)
	}
	for sheet, options := range map[string]string{"Deals": `"Won,Lost"`, "Vendors": `"Gold,Silver"`} {
		if x := got.xml[sheet]; !strings.Contains(x, `<dataValidation type="list"`) || !strings.Contains(x, options) {
			t.Errorf("the %s sheet has no dropdown of %s", sheet, options)
		}
	}
	for i, tb := range listed {
		if rows := got.rows[got.names[i]]; tb.Id != deals.Id && tb.Id != vendors.Id && (len(rows) != 2 || strings.Contains(got.xml[got.names[i]], "<dataValidation")) {
			t.Errorf("the sheet of %q: %v; want its one row, and no dropdown", tb.Name, rows)
		}
	}
}

// liveFieldsOf is a table's live fields, in its order.
func liveFieldsOf(t *testing.T, w *relationWorld, table uuid.UUID) []*model.Field {
	t.Helper()
	fields, err := model.ListFields(w.ctx, table)
	if err != nil {
		t.Fatalf("the fields of %s: %v", table, err)
	}
	return fields
}

// An export of every table is refused in words, with nothing to write, when
// its reader can open no table, when they can open more tables than one
// workbook holds, or when those tables hold more rows together than it does;
// a table they can't open counts toward neither, though the list names it,
// and a row in the trash counts toward neither.
func TestAnExportOfEveryTableIsRefusedInWordsPastWhatAWorkbookHolds(t *testing.T) {
	w := newRelationWorld(t)
	pay, payName := w.table("Pay")
	w.row(pay, map[string]interface{}{payName.Id.String(): "Asha"})
	w.row(pay, map[string]interface{}{payName.Id.String(): "Ravi"})
	if _, _, err := UpdateTable(w.ctx, pay.Id, TablePatch{Visibility: optStr(model.VisibilityPrivate)}, w.me); err != nil {
		t.Fatalf("make Pay private: %v", err)
	}
	listedWrongly(t)
	refused := func(what string, actor Actor, words string) {
		t.Helper()
		x, err := PrepareTablesExport(w.ctx, actor)
		if !IsTablesExportRefused(err) || x != nil || !strings.Contains(fmt.Sprint(err), words) {
			t.Errorf("%s: %v, %v; want refused in words (%q), nothing to write", what, x, err, words)
		}
	}
	refused("the member can open no table", w.member, "no table you can open")

	vendors, vName := w.table("Vendors")
	w.row(vendors, map[string]interface{}{vName.Id.String(): "Acme"})
	deals, dName := w.table("Deals")
	w.row(deals, map[string]interface{}{dName.Id.String(): "Booth"})
	w.row(deals, map[string]interface{}{dName.Id.String(): "Film"})

	set := func(tables, rows int) {
		maxTablesExported, maxTablesExportRows = tables, rows
	}
	t.Cleanup(func() { set(100, 1_000_000) })
	set(1, 1_000_000)
	refused("more tables than a workbook holds", w.member, "more than 1 table")
	set(2, 2)
	refused("more rows than a workbook holds", w.member, "more than 2 rows")
	// Pay's two rows and its table are the owner's, never the member's.
	set(2, 3)
	if got := w.exportedTables(w.member); len(got.names) != 2 || got.holds("Asha") {
		t.Errorf("at exactly what a workbook holds: sheets %q; want Vendors and Deals", got.names)
	}
	// A row in the trash is no row of the workbook's: Deals' third, trashed,
	// leaves the member's tables at exactly what it holds, and the owner's.
	gone := w.row(deals, map[string]interface{}{dName.Id.String(): "Trashed lead"})
	if _, err := DeleteRow(w.ctx, deals.Id, gone, w.me); err != nil {
		t.Fatalf("trash a row of Deals: %v", err)
	}
	if got := w.exportedTables(w.member); len(got.names) != 2 || got.holds("Trashed lead") {
		t.Errorf("at exactly what a workbook holds, a row in the trash: sheets %q; want Vendors and Deals, without the trashed row", got.names)
	}
	set(3, 5)
	if got := w.exportedTables(w.me); len(got.names) != 3 {
		t.Errorf("the owner, at exactly what a workbook holds: sheets %q; want three", got.names)
	}
	set(2, 5)
	refused("the owner, past what a workbook holds", w.me, "more than 2 tables")
}
