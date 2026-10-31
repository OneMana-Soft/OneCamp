//go:build integration

package business

// Tables S1: a spreadsheet file becomes a table through the Content IO
// importer into tables, its columns' kinds guessed and its cells stored as
// those kinds store them.
//
// Run: integ_rel.sh <tree> <log> '^(TestASpreadsheetBecomesAPrivateTable|TestAWorkbookBecomesTables|TestASheetComesIntoATableByItsKey|TestASheetComesIntoATableByAKeyWrittenAnotherWay|TestAPreviewShowsTheTableASheetComesInto|TestAnImportCutOffInATableTakesBackWhatItWrote|TestAMemberWhoCantOpenATableBringsNoSheetIntoIt|TestASpreadsheetsTablesAreItsImportersAlone)$' with PKGS='./business/DataTable/'

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestASpreadsheetBecomesAPrivateTable(t *testing.T) {
	w := newRelationWorld(t)
	csv := []byte("Name,Amount,Due,Status\n" +
		"Ada,\"$1,200.50\",23/10/2026,Open\n" +
		"Bo,$30,05/01/2026,Done\n" +
		",,,\n" +
		"Cy,$4,01/02/2026,Open\n" +
		"Dee,$5,02/02/2026,Done\n")
	in := contentIOBusiness.Input{Filename: "Invoices.csv", Size: int64(len(csv)), File: bytes.NewReader(csv)}
	var reports []string
	made, err := sheetImporter{}.Make(w.ctx, accessBusiness.Subject{UserUUID: w.me.UserID.String()}, in, nil, func(done, total int) {
		reports = append(reports, fmt.Sprintf("%d/%d", done, total))
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(made.Created) != 1 || made.Created[0].Kind != "table" || made.Created[0].Title != "Invoices" || made.Show == nil || made.Count != 4 || len(made.Skipped) != 0 {
		t.Fatalf("the import made %+v", made)
	}
	if fmt.Sprint(reports) != "[4/4]" {
		t.Errorf("the job heard %v of its rows, want [4/4]", reports)
	}
	id := uuid.MustParse(made.Created[0].UUID)
	tb, err := model.GetTableByID(w.ctx, id)
	if err != nil || tb == nil || tb.Visibility != model.VisibilityPrivate {
		t.Fatalf("the table is %+v (%v); want it private to the person importing, as an imported base is", tb, err)
	}

	fields, err := model.ListFields(w.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	var kinds []string
	for _, f := range fields {
		keys[f.Name] = f.Id.String()
		kind := f.Type
		if f.Type == model.FieldSelect {
			var labels []string
			for _, o := range fieldTypes.OptionsOf(f) {
				labels = append(labels, o.Label)
			}
			kind += fmt.Sprint(labels)
		}
		kinds = append(kinds, f.Name+":"+kind)
	}
	if want := "[Name:text Amount:number Due:date Status:select[Open Done]]"; fmt.Sprint(kinds) != want {
		t.Errorf("the fields are %v, want %s", kinds, want)
	}

	rows, err := model.ListRows(w.ctx, id, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		var cells map[string]any
		if err := json.Unmarshal([]byte(r.Values), &cells); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprint(cells[keys["Name"]], " ", cells[keys["Amount"]], " ", cells[keys["Due"]], " ", cells[keys["Status"]]))
	}
	if want := "[Ada 1200.5 2026-10-23 Open Bo 30 2026-01-05 Done Cy 4 2026-02-01 Open Dee 5 2026-02-02 Done]"; fmt.Sprint(got) != want {
		t.Errorf("the rows are %q, want %s (the empty row left out)", got, want)
	}

	// A row too large for a table to keep is left out, and said; the rows
	// around it come in.
	notes := []byte("Name,Notes\nAda,short\nBo," + strings.Repeat("n", 120<<10) + "\nCy,short\n")
	made, err = sheetImporter{}.Make(w.ctx, accessBusiness.Subject{UserUUID: w.me.UserID.String()},
		contentIOBusiness.Input{Filename: "Notes.csv", Size: int64(len(notes)), File: bytes.NewReader(notes)}, nil, func(int, int) {})
	if err != nil {
		t.Fatalf("a row too large ended the import: %v", err)
	}
	if made.Count != 2 || len(made.Skipped) != 1 || made.Skipped[0].What != "Notes" || !strings.Contains(made.Skipped[0].Why, "too large") {
		t.Errorf("the import of a row too large made %+v", made)
	}
}

// A workbook LibreOffice wrote, as an Excel file and as an OpenDocument
// file, becomes its sheets' table, each cell stored as its field's kind
// stores it, and its job says what was left out, by name: the hidden and
// the empty sheet, the formulas behind their values, the times of day.
func TestAWorkbookBecomesTables(t *testing.T) {
	for _, file := range []string{"libreoffice-deals.xlsx", "libreoffice-deals.ods"} {
		t.Run(file, func(t *testing.T) {
			w := newRelationWorld(t)
			data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sheets", file))
			if err != nil {
				t.Fatal(err)
			}
			in := contentIOBusiness.Input{Filename: file, Size: int64(len(data)), File: bytes.NewReader(data)}
			made, err := sheetImporter{}.Make(w.ctx, accessBusiness.Subject{UserUUID: w.me.UserID.String()}, in, nil, func(int, int) {})
			if err != nil {
				t.Fatal(err)
			}
			if len(made.Created) != 1 || made.Created[0].Title != "Deals" || made.Count != 3 {
				t.Fatalf("the import made %+v", made)
			}
			var said []string
			for _, s := range made.Skipped {
				said = append(said, s.What)
			}
			if want := "[Deals / Signed Deals / Total Deals / Ratio Lists Empty]"; fmt.Sprint(said) != want {
				t.Errorf("the job names %v as left out, want %s", said, want)
			}
			id := uuid.MustParse(made.Created[0].UUID)
			fields, err := model.ListFields(w.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			keys := map[string]string{}
			var kinds []string
			for _, f := range fields {
				keys[f.Name] = f.Id.String()
				kinds = append(kinds, f.Name+":"+f.Type)
			}
			if want := "[Name:text Region:text Amount:number Fee:number Share:number Closed:date Signed:date Won:checkbox Hours:duration Starts:duration Total:number Notes:long_text Ratio:number Code:text Size:number]"; fmt.Sprint(kinds) != want {
				t.Errorf("the fields are %v,\nwant %s", kinds, want)
			}
			rows, err := model.ListRows(w.ctx, id, 10, 0)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range rows {
				var cells map[string]any
				if err := json.Unmarshal([]byte(r.Values), &cells); err != nil {
					t.Fatal(err)
				}
				var row []string
				for _, name := range []string{"Name", "Amount", "Fee", "Share", "Signed", "Won", "Hours", "Total", "Notes", "Ratio", "Code"} {
					row = append(row, fmt.Sprint(cells[keys[name]]))
				}
				got = append(got, fmt.Sprint(row))
			}
			want := []string{
				"[Ada 1200.5 50 12.5 2026-10-23 true 5400 2401 Big deal <nil> 00123]",
				"[Bo 30 100000 7 2026-01-05 <nil> 94500 60 two\nlines 0.5 00456]",
				"[Cy 4 1 100 2026-02-01 true 2700 8 <nil> 0.25 00789]",
			}
			if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", want) {
				t.Errorf("the rows are\n%q,\nwant\n%q", got, want)
			}
		})
	}
}

// A sheet comes into a table of the person's, each column into the field of
// its name. A row whose Code names a row of the table changes it, a later row
// naming the same one changes it again, and the rest are made, one without a
// Code too. A column the table has no field for is said. One undo takes the
// whole import back.
func TestASheetComesIntoATableByItsKey(t *testing.T) {
	w := newRelationWorld(t)
	tb, name := w.table("Deals")
	code := w.field(tb, "Code", model.FieldText, nil)
	amount := w.field(tb, "Amount", model.FieldNumber, nil)
	for _, r := range [][3]any{{"A", "a1", 1.0}, {"B", "b1", 2.0}, {"C", "c1", 3.0}} {
		w.row(tb, map[string]any{name.Id.String(): r[0], code.Id.String(): r[1], amount.Id.String(): r[2]})
	}
	deals := func() string {
		t.Helper()
		var out []string
		for _, c := range w.cells(tb, &w.me) {
			out = append(out, fmt.Sprint(c[name.Id.String()], " ", c[code.Id.String()], " ", c[amount.Id.String()]))
		}
		slices.Sort(out)
		return fmt.Sprint(out)
	}
	before := deals()
	if before != "[A a1 1 B b1 2 C c1 3]" {
		t.Fatalf("Deals holds %s before the import", before)
	}
	csv := []byte("Code,Name,Amount,Extra\nb1,B2,20,x\nd1,D,40,y\nb1,B3,30,z\n,E,50,w\n")
	in := contentIOBusiness.Input{Filename: "Deals.csv", Size: int64(len(csv)), File: bytes.NewReader(csv)}
	choices := json.RawMessage(fmt.Sprintf(`{"sheets":[{"index":0,"into":%q,"match_by":"c1"}]}`, tb.Id))
	made, err := sheetImporter{}.Make(w.ctx, accessBusiness.Subject{UserUUID: w.me.UserID.String()}, in, choices, func(int, int) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(made.Created) != 0 || made.Show == nil || made.Show.UUID != tb.Id.String() || made.Show.Title != "Deals" || made.Count != 4 {
		t.Fatalf("the import made %+v, want its 4 rows brought into Deals", made)
	}
	var said []string
	for _, s := range made.Skipped {
		said = append(said, s.What+": "+s.Why)
	}
	if want := `[Deals / Extra: The table has no field named "Extra", so the column isn't brought. ` +
		`Deals / Code: An earlier row names "b1" too, so this one changes the same row.]`; fmt.Sprint(said) != want {
		t.Errorf("the import said %v, want %s", said, want)
	}
	if got := deals(); got != "[A a1 1 B3 b1 30 C c1 3 D d1 40 E <nil> 50]" {
		t.Errorf("Deals holds %s after the import", got)
	}
	if _, err := Undo(w.ctx, tb.Id, nil, w.me); err != nil {
		t.Fatal(err)
	}
	if got := deals(); got != before {
		t.Errorf("one undo left Deals holding %s, want %s", got, before)
	}
}

// A sheet coming into a table by an email, a link or a phone number names a
// row as people tell them apart: an email whatever its case, a link whatever
// the case of its scheme and host, its path as written, and a phone number by
// its digits, however they're spaced. Brought in again written another way,
// it changes the rows it names, not a second of each.
func TestASheetComesIntoATableByAKeyWrittenAnotherWay(t *testing.T) {
	w := newRelationWorld(t)
	for _, c := range []struct{ kind, kept, csv, want string }{
		{model.FieldEmail, "Maya.Patel@Example.com",
			"Key,Amount\nmaya.patel@EXAMPLE.COM,2\n",
			"[maya.patel@EXAMPLE.COM 2]"},
		{model.FieldURL, "https://Example.com/Brief",
			"Key,Amount\nHTTPS://EXAMPLE.COM/Brief,2\nhttps://example.com/brief,3\n",
			"[HTTPS://EXAMPLE.COM/Brief 2 https://example.com/brief 3]"},
		{model.FieldPhone, "+1 (555) 010-0199 ext. 12",
			"Key,Amount\n+1 555.010.0199 x12,2\n+1 555 010 0199,3\n",
			"[+1 555 010 0199 3 +1 555.010.0199 x12 2]"},
	} {
		tb, _ := w.table("By " + c.kind)
		key := w.field(tb, "Key", c.kind, nil)
		amount := w.field(tb, "Amount", model.FieldNumber, nil)
		w.row(tb, map[string]any{key.Id.String(): c.kept, amount.Id.String(): 1.0})
		in := contentIOBusiness.Input{Filename: "Keys.csv", Size: int64(len(c.csv)), File: bytes.NewReader([]byte(c.csv))}
		choices := json.RawMessage(fmt.Sprintf(`{"sheets":[{"index":0,"into":%q,"match_by":"c1"}]}`, tb.Id))
		if _, err := (sheetImporter{}).Make(w.ctx, accessBusiness.Subject{UserUUID: w.me.UserID.String()}, in, choices, func(int, int) {}); err != nil {
			t.Fatalf("by %s: %v", c.kind, err)
		}
		var got []string
		for _, cells := range w.cells(tb, &w.me) {
			got = append(got, fmt.Sprint(cells[key.Id.String()], " ", cells[amount.Id.String()]))
		}
		slices.Sort(got)
		if fmt.Sprint(got) != c.want {
			t.Errorf("by %s, the table holds %v after the import, want %s", c.kind, got, c.want)
		}
	}
}

// A sheet's preview, chosen into a table, shows each column in its field
// there, for anyone who can open the table; a table the person can't open,
// or that isn't there, isn't shown, and which it is isn't said.
func TestAPreviewShowsTheTableASheetComesInto(t *testing.T) {
	w := newRelationWorld(t)
	tb, name := w.table("Deals")
	amount := w.field(tb, "Amount", model.FieldNumber, nil)
	pay, err := CreateTable(w.ctx, TableInput{Name: "Pay", Visibility: "private"}, w.me)
	if err != nil {
		t.Fatal(err)
	}
	csv := []byte("Name,Amount,Extra\nAda,20,x\n")
	preview := func(who Actor, into uuid.UUID) (sheetPreview, error) {
		in := contentIOBusiness.Input{Filename: "Deals.csv", Size: int64(len(csv)), File: bytes.NewReader(csv)}
		var p sheetPreview
		raw, err := sheetImporter{}.Preview(w.ctx, accessBusiness.Subject{UserUUID: who.UserID.String()}, in, json.RawMessage(fmt.Sprintf(`{"sheets":[{"index":0,"into":%q}]}`, into)))
		if err == nil {
			err = json.Unmarshal(raw, &p)
		}
		return p, err
	}
	p, err := preview(w.member, tb.Id)
	if err != nil || len(p.Sheets) != 1 {
		t.Fatalf("the member's preview: %+v, %v", p, err)
	}
	var goes []string
	for _, f := range p.Sheets[0].Fields {
		goes = append(goes, fmt.Sprintf("%s>%s:%t", f.Key, f.Field, f.Leave))
	}
	if want := fmt.Sprintf("[c1>%s:false c2>%s:false c3>:true]", name.Id, amount.Id); fmt.Sprint(goes) != want {
		t.Errorf("the member's preview shows %v, want %s", goes, want)
	}
	for _, into := range []uuid.UUID{pay.Id, uuid.New()} {
		if _, err := preview(w.member, into); !errors.Is(err, errNoTarget) {
			t.Errorf("the member's preview into %s: %v, want %v", into, err, errNoTarget)
		}
	}
}

// An import into a table that's cut off after its first batch is written,
// its job's context cancelled, takes back what it wrote: the table is as it
// was, though the context the import ran on has ended. That holds for the
// options it gave a select for the values it brought, and only those: an
// option added while it ran stays, and so does one of its options that a row
// took up meanwhile, with that row's cell (another tab, both).
func TestAnImportCutOffInATableTakesBackWhatItWrote(t *testing.T) {
	w := newRelationWorld(t)
	tb, name := w.table("Deals")
	stage := w.field(tb, "Stage", model.FieldSelect, map[string]interface{}{"options": []map[string]interface{}{{"label": "Lead"}}})
	stages := func() string {
		t.Helper()
		return labelsOf(t, optionsOfField(t, w.ctx, tb.Id, "Stage"), "Lead", "Lost")
	}
	a := w.row(tb, map[string]any{name.Id.String(): "A"})
	names := func() string {
		t.Helper()
		var out []string
		for _, c := range w.cells(tb, &w.me) {
			out = append(out, fmt.Sprint(c[name.Id.String()]))
		}
		slices.Sort(out)
		return fmt.Sprint(out)
	}
	ctx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	wrote := false
	base := ImportBase{Private: true, Tables: []ImportTable{{Key: "Deals", Name: "Deals", Into: tb.Id,
		Fields: []ImportField{
			{Key: "c1", Name: "Name", Type: model.FieldText, Into: name.Id},
			{Key: "c2", Name: "Stage", Type: model.FieldSelect, Into: stage.Id},
		},
		Rows: func(page func([]ImportRow) error) error {
			if err := page([]ImportRow{{Cells: map[string]any{"c1": "B", "c2": "Won"}}, {Cells: map[string]any{"c1": "C", "c2": "Paused"}}}); err != nil {
				return err
			}
			wrote = names() == "[A B C]" && stages() == "Lead, Won, Paused"
			opts := []any{}
			for _, o := range optionsOfField(t, w.ctx, tb.Id, "Stage") {
				opts = append(opts, o)
			}
			lost := FieldPatch{Config: map[string]any{"options": append(opts, map[string]any{"label": "Lost"})}}
			if _, err := UpdateField(w.ctx, tb.Id, stage.Id, lost, w.me); err != nil {
				t.Fatalf("another tab's option for Stage: %v", err)
			}
			if _, err := PatchRow(w.ctx, tb.Id, a, map[string]any{stage.Id.String(): "Paused"}, w.me); err != nil {
				t.Fatalf("another tab's Paused for A: %v", err)
			}
			cancel()
			return ctx.Err()
		}}}}
	if _, err := ImportBaseAs(ctx, w.me, base); err == nil {
		t.Fatal("the import cut off returned no error")
	}
	if !wrote {
		t.Fatal("the first batch and the options it brought were never written, so nothing tested taking them back")
	}
	if got := names(); got != "[A]" {
		t.Errorf("after the import was cut off Deals holds %s, want [A]", got)
	}
	if got := stages(); got != "Lead, Paused, Lost" {
		t.Errorf("after the import was cut off Stage's options are %s, want Lead, Paused, Lost", got)
	}
	if got := w.cells(tb, &w.me)[a][stage.Id.String()]; got != "Paused" {
		t.Errorf("after the import was cut off A's Stage is %v, want Paused", got)
	}
}

// A sheet comes into a table only for someone who may open it. A member who
// can't open a private table brings nothing into it, and is told just what
// they'd be told of a table that isn't there; the table is as it was, and
// they're left with no table of their own. Its owner brings the same file in.
func TestAMemberWhoCantOpenATableBringsNoSheetIntoIt(t *testing.T) {
	w := newRelationWorld(t)
	pay, err := CreateTable(w.ctx, TableInput{Name: "Pay", Visibility: "private"}, w.me)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := model.ListFields(w.ctx, pay.Id)
	if err != nil || len(fields) == 0 {
		t.Fatalf("Pay has no name field: %v", err)
	}
	w.row(pay, map[string]any{fields[0].Id.String(): "Ada"})
	csv := []byte("Name\nMallory\n")
	bring := func(who Actor, into uuid.UUID) error {
		in := contentIOBusiness.Input{Filename: "Pay.csv", Size: int64(len(csv)), File: bytes.NewReader(csv)}
		choices := json.RawMessage(fmt.Sprintf(`{"sheets":[{"index":0,"into":%q}]}`, into))
		_, err := sheetImporter{}.Make(w.ctx, accessBusiness.Subject{UserUUID: who.UserID.String()}, in, choices, func(int, int) {})
		return err
	}
	names := func() string {
		t.Helper()
		var out []string
		for _, c := range w.cells(pay, &w.me) {
			out = append(out, fmt.Sprint(c[fields[0].Id.String()]))
		}
		slices.Sort(out)
		return fmt.Sprint(out)
	}
	had, err := ListTables(w.ctx, w.member)
	if err != nil {
		t.Fatal(err)
	}
	for _, into := range []uuid.UUID{pay.Id, uuid.New()} {
		if err := bring(w.member, into); !errors.Is(err, errNoTarget) {
			t.Errorf("the member's import into %s: %v, want %v", into, err, errNoTarget)
		}
	}
	if got := names(); got != "[Ada]" {
		t.Errorf("Pay holds %s after the member's import, want [Ada]", got)
	}
	if theirs, err := ListTables(w.ctx, w.member); err != nil || len(theirs) != len(had) {
		t.Errorf("the member's refused imports left them %d tables (%v), want the %d they had", len(theirs), err, len(had))
	}
	if err := bring(w.me, pay.Id); err != nil {
		t.Fatalf("Pay's owner brings the file in: %v", err)
	}
	if got := names(); got != "[Ada Mallory]" {
		t.Errorf("Pay holds %s after its owner's import, want [Ada Mallory]", got)
	}
}

// The tables a spreadsheet makes are its importer's alone, as an imported
// base is: another member can't open them or find them listed, and an admin,
// who keeps access to every table, can.
func TestASpreadsheetsTablesAreItsImportersAlone(t *testing.T) {
	w := newRelationWorld(t)
	admin := Actor{UserID: uuid.New(), IsAdmin: true}
	csv := []byte("Name,Salary\nAda,100\n")
	in := contentIOBusiness.Input{Filename: "Salaries.csv", Size: int64(len(csv)), File: bytes.NewReader(csv)}
	made, err := sheetImporter{}.Make(w.ctx, accessBusiness.Subject{UserUUID: w.member.UserID.String()}, in, nil, func(int, int) {})
	if err != nil || len(made.Created) != 1 {
		t.Fatalf("the member's import made %+v (%v)", made, err)
	}
	id := uuid.MustParse(made.Created[0].UUID)
	listed := func(who Actor) bool {
		t.Helper()
		tables, err := ListTables(w.ctx, who)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(tables, func(tb *model.DataTable) bool { return tb.Id == id })
	}
	if _, err := GetBundle(w.ctx, id, w.member); err != nil || !listed(w.member) {
		t.Errorf("the importer opens their table: %v, listed %t", err, listed(w.member))
	}
	if _, err := GetBundle(w.ctx, id, w.me); !errors.Is(err, errForbidden) {
		t.Errorf("another member opening the importer's table: %v, want %v", err, errForbidden)
	}
	if listed(w.me) {
		t.Error("another member's tables list the importer's table")
	}
	if _, err := GetBundle(w.ctx, id, admin); err != nil || !listed(admin) {
		t.Errorf("an admin opens the importer's table: %v, listed %t", err, listed(admin))
	}
}

// V-40: rows imported into a table go through the import's own job, the way
// the table's Import rows sends them. The file is read and left ready, the
// person confirms their choice, and the rows are made. The job then ends done,
// saying how many rows it made and where. It never says "There was nothing in
// the file to import.": that message came after the rows had landed and sent
// people to import them a second time. (Make's own tests never ran the job:
// TestASheetComesIntoATableByItsKey calls it directly.)
func TestRowsImportedIntoATableEndTheirJobDone(t *testing.T) {
	w := newRelationWorld(t)
	integration.SetupMinio(t, "v40-into-table")
	// The job's runner reads its person from the graph, as the server does
	// (business/ContentIO subjectFor): someone with no node there can't import.
	integration.SetupDgraph(t)
	if _, err := userDomain.CreateOrUpdateDgraphUser(w.ctx, &dgraphStruct.DgraphUser{Uuid: w.me.UserID.String(), UserName: "owner"}); err != nil {
		t.Fatalf("the owner in the graph: %v", err)
	}
	tb, _ := w.table("Deals")
	w.field(tb, "Amount", model.FieldNumber, nil)
	const n = 30
	var file strings.Builder
	file.WriteString("Name,Amount\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&file, "Deal %02d,%d\n", i, i*10)
	}
	csv := []byte(file.String())
	who := accessBusiness.Subject{UserUUID: w.me.UserID.String()}
	// The queue's runner, as the server runs it, for this test alone.
	ctx, stop := context.WithCancel(w.ctx)
	t.Cleanup(stop)
	sweepBusiness.Jobs.StartAll(ctx)

	into := json.RawMessage(fmt.Sprintf(`{"sheets":[{"index":0,"into":%q}]}`, tb.Id))
	job, err := contentIOBusiness.Pipeline.Import(ctx, who, contentIOBusiness.ImportRequest{
		Filename: "Deals.csv", Into: contentIOBusiness.IntoTable, Size: int64(len(csv)), Choices: into, Body: bytes.NewReader(csv),
	})
	if err != nil {
		t.Fatalf("start the import: %v", err)
	}
	wait := func(until ...contentIOBusiness.JobStatus) contentIOBusiness.Job {
		t.Helper()
		for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			j, err := contentIOBusiness.Pipeline.Job(ctx, who, job.ID)
			if err != nil {
				t.Fatalf("read the job: %v", err)
			}
			if slices.Contains(until, j.Status) {
				return j
			}
			if time.Now().After(deadline) {
				t.Fatalf("the job is still %s (%d%%) after 90 s, want %v", j.Status, j.Progress, until)
			}
		}
	}
	if read := wait(contentIOBusiness.JobReady, contentIOBusiness.JobFailed); read.Status != contentIOBusiness.JobReady {
		t.Fatalf("reading the file: %s %q, want it ready for the person's choice", read.Status, read.Error)
	}
	if _, err := contentIOBusiness.Pipeline.Choose(ctx, who, job.ID, into, true); err != nil {
		t.Fatalf("confirm the import: %v", err)
	}
	done := wait(contentIOBusiness.JobDone, contentIOBusiness.JobFailed)
	if done.Status != contentIOBusiness.JobDone || done.Error != "" || done.Made != n || done.Show == nil || done.Show.UUID != tb.Id.String() {
		t.Fatalf("the import ended %s %q, made %d, show %+v; want it done, its %d rows made in Deals", done.Status, done.Error, done.Made, done.Show, n)
	}
	if got := len(w.cells(tb, &w.me)); got != n {
		t.Fatalf("Deals holds %d rows after the import, want %d", got, n)
	}
}
