//go:build integration

package business

// A table's view as an Excel workbook (tables, package 2b), against Postgres
// 12 with every migration: a reader who can't open the table finds nothing
// and gets nothing; the rows come in the view's order, by its spec or by the
// ids the page names, past one window of rows; and each cell reads back,
// through the import's own reader, as the grid shows it, a link into a table
// the reader can't open as a private row, never by its id.
// Run: integ23.sh <log> <tree> './business/DataTable/' with the tests' names.

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// exported is table's export of in for actor, written whole and read back as
// an import reads a workbook: its first sheet's rows, the header first, and
// what it says it cut.
func (w *relationWorld) exported(table uuid.UUID, actor Actor, in ExportInput) ([][]contentIOBusiness.Cell, []string) {
	w.t.Helper()
	e, err := PrepareExport(w.ctx, table, actor, in)
	if err != nil {
		w.t.Fatalf("ready the export: %v", err)
	}
	var b bytes.Buffer
	notes, err := e.WriteXLSX(w.ctx, &b)
	if err != nil {
		w.t.Fatalf("write the export: %v", err)
	}
	sheets, err := contentIOBusiness.ReadSheets(contentIOBusiness.Input{Filename: "export.xlsx", Size: int64(b.Len()), File: bytes.NewReader(b.Bytes())})
	if err != nil || len(sheets) == 0 {
		w.t.Fatalf("read the export back: %d sheets, %v", len(sheets), err)
	}
	var rows [][]contentIOBusiness.Cell
	if err := sheets[0].Rows(func(cells []contentIOBusiness.Cell) error {
		rows = append(rows, slices.Clone(cells))
		return nil
	}); err != nil {
		w.t.Fatalf("read the export's rows: %v", err)
	}
	return rows, notes
}

// textsOf is the text of each cell of a column, below its header.
func textsOf(rows [][]contentIOBusiness.Cell, col int) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows[1:] {
		text := ""
		if col < len(r) {
			text = r[col].Text
		}
		out = append(out, text)
	}
	return out
}

// Another member exports a table private to its owner: it isn't found, as a
// table that isn't there isn't, whether the view names its rows by its rules
// or by their ids, and nothing is made ready to write. Its owner exports it.
func TestAnExportOfATableTheReaderCantOpenIsNotFound(t *testing.T) {
	w := newRelationWorld(t)
	pay, name := w.table("Pay")
	asha := w.row(pay, map[string]interface{}{name.Id.String(): "Asha"})
	if _, _, err := UpdateTable(w.ctx, pay.Id, TablePatch{Visibility: optStr(model.VisibilityPrivate)}, w.me); err != nil {
		t.Fatalf("make Pay private: %v", err)
	}
	fields := []uuid.UUID{name.Id}
	for what, in := range map[string]ExportInput{
		"by its rules":     {Fields: fields},
		"by its rows' ids": {Fields: fields, Rows: []uuid.UUID{asha}},
	} {
		if e, err := PrepareExport(w.ctx, pay.Id, w.member, in); !IsNotFound(err) || e != nil {
			t.Errorf("another member's export of Pay %s: %v, %v; want not found, and nothing to write", what, e, err)
		}
	}
	if _, err := PrepareExport(w.ctx, uuid.New(), w.member, ExportInput{Fields: fields}); !IsNotFound(err) {
		t.Errorf("an export of a table that isn't there: %v; want not found", err)
	}
	rows, _ := w.exported(pay.Id, w.me, ExportInput{Fields: fields})
	if got := textsOf(rows, 0); !slices.Equal(got, []string{"Asha"}) {
		t.Errorf("the owner's export of Pay: %q; want Asha", got)
	}
}

// An export's rows come in the view's order: as its spec sorts them, or as
// the page names them by id (each once, and only the table's), past one
// window of rows either way.
func TestAnExportsRowsComeInTheViewsOrder(t *testing.T) {
	w := newRelationWorld(t)
	deals, name := w.table("Deals")
	score := w.field(deals, "Score", model.FieldNumber, nil)
	b := w.row(deals, map[string]interface{}{name.Id.String(): "B", score.Id.String(): 1})
	a := w.row(deals, map[string]interface{}{name.Id.String(): "A", score.Id.String(): 3})
	c := w.row(deals, map[string]interface{}{name.Id.String(): "C", score.Id.String(): 2})
	other, otherName := w.table("Other")
	stranger := w.row(other, map[string]interface{}{otherName.Id.String(): "Stranger"})
	fields := []uuid.UUID{name.Id, score.Id}

	byScore := &query.Spec{Sorts: []query.Sort{{Field: "field:" + score.Id.String(), Desc: true}}}
	rows, _ := w.exported(deals.Id, w.member, ExportInput{Fields: fields, Spec: byScore})
	if got := textsOf(rows, 0); !slices.Equal(got, []string{"A", "C", "B"}) {
		t.Errorf("by the spec's sort, highest score first: %q; want A, C, B", got)
	}
	rows, _ = w.exported(deals.Id, w.member, ExportInput{Fields: fields, Rows: []uuid.UUID{b, a, b, stranger, c}})
	if got := textsOf(rows, 0); !slices.Equal(got, []string{"B", "A", "C"}) {
		t.Errorf("by ids, one named twice and one of another table: %q; want B, A, C", got)
	}
	// A view the page filtered down to no rows names none: its header alone.
	if rows, _ = w.exported(deals.Id, w.member, ExportInput{Fields: fields, Rows: []uuid.UUID{}}); len(rows) != 1 {
		t.Errorf("a view that shows no rows: %d rows; want its header alone", len(rows))
	}

	// 1,201 rows more, after the three, keyed as the table's own writes key a
	// run of rows (keys: position.Spread after the last key). Rows read in key
	// order, so a row without one would come last, whatever its position.
	var lastKey string
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(w.ctx, `SELECT MAX(position_key) FROM data_table_rows WHERE table_id = $1`, deals.Id).Scan(&lastKey); err != nil {
		t.Fatalf("the table's last key: %v", err)
	}
	ks, err := keys(lastKey, "", 1201)
	if err != nil {
		t.Fatalf("keys for 1,201 rows: %v", err)
	}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(w.ctx, `INSERT INTO data_table_rows (id, table_id, values, position, position_key, created_by)
		SELECT uuid_generate_v4(), $1, jsonb_build_object($2::text, 'Row ' || k.n), 100 + k.n, k.key, $3
		FROM unnest($4::text[]) WITH ORDINALITY AS k(key, n)`, deals.Id, name.Id.String(), w.me.UserID, ks); err != nil {
		t.Fatalf("seed 1,201 rows: %v", err)
	}
	rows, _ = w.exported(deals.Id, w.member, ExportInput{Fields: []uuid.UUID{name.Id}})
	got := textsOf(rows, 0)
	if len(got) != 1204 || got[2] != "C" || got[3] != "Row 1" || got[1203] != "Row 1201" {
		t.Errorf("every row in the table's order: %d rows, %q … %q; want 1,204, B, A, C, then Row 1 to Row 1201", len(got), got[:min(len(got), 4)], got[max(len(got)-1, 0):])
	}

	var last []uuid.UUID
	found, err := postgresInit.DBConn.SqlDB.QueryContext(w.ctx, `SELECT id FROM data_table_rows WHERE table_id = $1 ORDER BY position DESC`, deals.Id)
	if err != nil {
		t.Fatal(err)
	}
	for found.Next() {
		var id uuid.UUID
		if err := found.Scan(&id); err != nil {
			t.Fatal(err)
		}
		last = append(last, id)
	}
	found.Close()
	rows, _ = w.exported(deals.Id, w.member, ExportInput{Fields: []uuid.UUID{name.Id}, Rows: last})
	got = textsOf(rows, 0)
	if len(got) != 1204 || got[0] != "Row 1201" || got[1200] != "Row 1" || got[1203] != "B" {
		t.Errorf("by 1,204 ids, last first: %d rows, %q … %q; want Row 1201 to Row 1, then C, A, B", len(got), got[:min(len(got), 1)], got[max(len(got)-4, 0):])
	}
}

// Each cell of an export reads back, through the import's own reader, as the
// grid shows it: an amount with its currency, a percent, a day, a length of
// time, a checkbox (unticked too), a choice, a list by its names, an id with
// its prefix, a formula's value; and a link into a table the reader can't
// open reads "Private row", never the row's id.
func TestAnExportReadsBackAsTheGridShowsIt(t *testing.T) {
	w := newRelationWorld(t)
	vendors, vName := w.table("Vendors")
	deals, name := w.table("Deals")
	amount := w.field(deals, "Amount", model.FieldNumber, map[string]interface{}{"format": map[string]interface{}{"currency": "INR", "decimals": 2}})
	share := w.field(deals, "Share", model.FieldNumber, map[string]interface{}{"format": map[string]interface{}{"percent": true}})
	due := w.field(deals, "Due", model.FieldDate, nil)
	done := w.field(deals, "Done", model.FieldCheckbox, nil)
	stage := w.field(deals, "Stage", model.FieldSelect, map[string]interface{}{"options": []interface{}{"Won", "Lost"}})
	tags := w.field(deals, "Tags", model.FieldMultiSelect, map[string]interface{}{"options": []interface{}{"Hot", "Big"}})
	took := w.field(deals, "Took", model.FieldDuration, map[string]interface{}{"format": "h:mm:ss"})
	ref := w.field(deals, "Ref", model.FieldAutonumber, map[string]interface{}{"prefix": "DL"})
	double := w.field(deals, "Double", model.FieldFormula, map[string]interface{}{"formula": "{Amount} * 2"})
	vendor := w.field(deals, "Vendor", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String()})
	acme := w.row(vendors, map[string]interface{}{vName.Id.String(): "Acme"})
	w.row(deals, map[string]interface{}{
		name.Id.String(): "Booth", amount.Id.String(): 1250.5, share.Id.String(): 12.5, due.Id.String(): "2026-10-23",
		done.Id.String(): true, stage.Id.String(): "Won", tags.Id.String(): []interface{}{"Hot", "Big"},
		took.Id.String(): 5415, vendor.Id.String(): []interface{}{acme.String()},
	})
	w.row(deals, map[string]interface{}{name.Id.String(): "Film"})
	if _, _, err := UpdateTable(w.ctx, vendors.Id, TablePatch{Visibility: optStr(model.VisibilityPrivate)}, w.me); err != nil {
		t.Fatalf("make Vendors private: %v", err)
	}

	shown := []*model.Field{name, amount, share, due, done, stage, tags, took, ref, double, vendor}
	ids := make([]uuid.UUID, len(shown))
	var header []string
	for i, f := range shown {
		ids[i] = f.Id
		header = append(header, f.Name)
	}
	rows, notes := w.exported(deals.Id, w.member, ExportInput{Fields: ids})
	if len(notes) != 0 || len(rows) != 3 {
		t.Fatalf("the export: %d rows, notes %q; want a header and 2 rows, nothing cut", len(rows), notes)
	}
	var got []string
	for _, c := range rows[0] {
		got = append(got, c.Text)
	}
	if !slices.Equal(got, header) {
		t.Errorf("the header: %q; want %q", got, header)
	}

	booth := rows[1]
	type want struct {
		what string
		ok   bool
	}
	for _, c := range []want{
		{"the name as text", booth[0].Kind == contentIOBusiness.CellText && booth[0].Text == "Booth"},
		{"the amount in rupees", booth[1].Kind == contentIOBusiness.CellNumber && booth[1].Number == 1250.5 && booth[1].Currency == "INR"},
		{"the share as a percent", booth[2].Kind == contentIOBusiness.CellNumber && booth[2].Number == 12.5 && booth[2].Percent},
		{"the day", booth[3].Kind == contentIOBusiness.CellDate && booth[3].Time.Equal(time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC))},
		{"the ticked checkbox", booth[4].Kind == contentIOBusiness.CellBool && booth[4].Bool},
		{"the choice", booth[5].Text == "Won"},
		{"the list by its names", booth[6].Text == "Hot, Big"},
		{"the length of time", booth[7].Kind == contentIOBusiness.CellDuration && booth[7].Number == 5415},
		{"the id with its prefix", booth[8].Text == "DL-1"},
		{"the formula's value", booth[9].Kind == contentIOBusiness.CellNumber && booth[9].Number == 2501},
		{"the link into a private table", booth[10].Text == "Private row"},
		{"the unticked checkbox", rows[2][4].Kind == contentIOBusiness.CellBool && !rows[2][4].Bool},
	} {
		if !c.ok {
			t.Errorf("%s doesn't read back as the grid shows it: %+v", c.what, booth)
		}
	}
	for _, r := range rows {
		for _, c := range r {
			if strings.Contains(c.Text, acme.String()) {
				t.Errorf("a cell names the private row by its id: %q", c.Text)
			}
		}
	}
}

// An export asked for wrongly is refused in its own words: no fields, a field
// of another table or one named twice, rules and ids both, or more ids than a
// request holds.
func TestAnExportAskedForWronglyIsRefused(t *testing.T) {
	w := newRelationWorld(t)
	deals, name := w.table("Deals")
	_, otherName := w.table("Other")
	row := w.row(deals, map[string]interface{}{name.Id.String(): "A"})
	many := make([]uuid.UUID, maxExportRowIDs+1)
	for i := range many {
		many[i] = uuid.New()
	}
	for what, in := range map[string]ExportInput{
		"no fields":                     {},
		"a field of another table":      {Fields: []uuid.UUID{otherName.Id}},
		"a field named twice":           {Fields: []uuid.UUID{name.Id, name.Id}},
		"rules and ids both":            {Fields: []uuid.UUID{name.Id}, Spec: &query.Spec{}, Rows: []uuid.UUID{row}},
		"more ids than a request holds": {Fields: []uuid.UUID{name.Id}, Rows: many},
	} {
		if e, err := PrepareExport(w.ctx, deals.Id, w.me, in); !IsExportInput(err) || e != nil {
			t.Errorf("an export with %s: %v, %v; want it refused as asked for wrongly", what, e, err)
		}
	}
}
