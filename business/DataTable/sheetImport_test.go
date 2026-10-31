package business

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	formula "github.com/akashc777/OneCamp/business/Formula"
	"github.com/google/uuid"
)

// workbookFixture is a file of testdata/sheets read as its sheets.
func workbookFixture(t *testing.T, name string) []contentIOBusiness.Sheet {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sheets", name))
	if err != nil {
		t.Fatal(err)
	}
	sheets, err := contentIOBusiness.ReadSheets(contentIOBusiness.Input{Filename: name, Size: int64(len(data)), File: bytes.NewReader(data)})
	if err != nil {
		t.Fatal(err)
	}
	return sheets
}

// A workbook becomes a base of a table a sheet, each column the kind its
// cells are: amounts with their currencies, percents, days, checkboxes,
// lengths of time, long text. Its hidden sheet and its empty one are left
// out, and said, as are its formulas (their values come) and the times its
// days were written with.
func TestAWorkbookBecomesABase(t *testing.T) {
	for _, file := range []string{"libreoffice-deals.xlsx", "libreoffice-deals.ods"} {
		t.Run(file, func(t *testing.T) {
			var heard []string
			base, err := sheetBase(workbookFixture(t, file), SheetOptions{Signs: formula.PlainSigns, DayFirst: true, Zone: time.UTC}, nil, nil, func(done, total int) {
				heard = append(heard, fmt.Sprintf("%d/%d", done, total))
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(base.Tables) != 1 || base.Tables[0].Name != "Deals" || !base.Private {
				t.Fatalf("the base is %d tables (private: %v), want Deals alone, private", len(base.Tables), base.Private)
			}
			var kinds []string
			for _, f := range base.Tables[0].Fields {
				kind := f.Name + ":" + f.Type
				if f.Config != nil {
					kind += fmt.Sprint(f.Config)
				}
				kinds = append(kinds, kind)
			}
			want := "[Name:text Region:text Amount:numbermap[format:map[currency:USD]] Fee:numbermap[format:map[currency:INR]] Share:numbermap[format:map[percent:true]] " +
				"Closed:date Signed:date Won:checkbox Hours:durationmap[format:h:mm] Starts:durationmap[format:h:mm] Total:numbermap[format:map[currency:USD]] " +
				"Notes:long_text Ratio:number Code:text Size:number]"
			if fmt.Sprint(kinds) != want {
				t.Errorf("the fields are\n%v,\nwant\n%s", kinds, want)
			}
			var notes []string
			for _, n := range base.Notes {
				notes = append(notes, n.Table+"/"+n.Field+":"+n.Code)
			}
			if want := "[Deals/c7:TIMES_LEFT_OUT Deals/c11:FORMULA_AS_VALUES Deals/c13:CELL_LEFT_OUT Lists/:SHEET_LEFT_OUT Empty/:SHEET_LEFT_OUT]"; fmt.Sprint(notes) != want {
				t.Errorf("the notes are %v, want %s", notes, want)
			}
			var rows []string
			if err := base.Tables[0].Rows(func(page []ImportRow) error {
				for _, r := range page {
					rows = append(rows, fmt.Sprint(r.Cells["c1"], " ", r.Cells["c3"], " ", r.Cells["c6"], " ", r.Cells["c8"], " ", r.Cells["c9"], " ", r.Cells["c13"]))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if want := "[Ada 1200.5 2026-10-23 true 5400 <nil> Bo 30 2026-01-05 <nil> 94500 0.5 Cy 4 2026-02-01 true 2700 0.25]"; fmt.Sprint(rows) != want {
				t.Errorf("the rows are %v,\nwant %s", rows, want)
			}
			if fmt.Sprint(heard) != "[3/3]" {
				t.Errorf("the job heard %v of the rows, want [3/3]", heard)
			}
		})
	}
}

// A row whose only value is an unticked box is brought as the sheet has it,
// a row with nothing ticked, and the job hears every row it counted.
func TestARowOfAnUntickedBoxIsBrought(t *testing.T) {
	var heard []string
	sheet := sheetOfRows([]string{"Name", "Done"}, []string{"Ada", "yes"}, []string{"", "no"}, []string{"Cy", "yes"})
	base, err := sheetBase([]contentIOBusiness.Sheet{sheet}, SheetOptions{}, nil, nil, func(done, total int) {
		heard = append(heard, fmt.Sprintf("%d/%d", done, total))
	})
	if err != nil || len(base.Tables) != 1 {
		t.Fatalf("%d tables, %v", len(base.Tables), err)
	}
	var rows []string
	if err := base.Tables[0].Rows(func(page []ImportRow) error {
		for _, r := range page {
			rows = append(rows, fmt.Sprint(r.Cells))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(rows) != "[map[c1:Ada c2:true] map[] map[c1:Cy c2:true]]" || fmt.Sprint(heard) != "[3/3]" {
		t.Errorf("the rows are %v, and the job heard %v", rows, heard)
	}
}

// Sheets of one name (a crafted file can have them) become tables told
// apart, each keeping its own rows.
func TestSheetsOfOneNameBecomeTablesOfTwo(t *testing.T) {
	a, b := sheetOfRows([]string{"Name"}, []string{"Ada"}), sheetOfRows([]string{"Name"}, []string{"Bo"})
	a.Name, b.Name = "Data", "data"
	base, err := sheetBase([]contentIOBusiness.Sheet{a, b, {Name: " ", Rows: sheetOfRows([]string{"Name"}, []string{"Cy"}).Rows}}, SheetOptions{}, nil, nil, func(int, int) {})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tb := range base.Tables {
		names = append(names, tb.Key+"="+tb.Name)
	}
	if fmt.Sprint(names) != "[Data=Data data 2=data 2 Sheet 3=Sheet 3]" {
		t.Errorf("the tables are %v", names)
	}
}

// What an import left out is said by its table's and field's names, not
// their keys.
func TestAnImportSaysWhatItLeftOutByName(t *testing.T) {
	base := ImportBase{Tables: []ImportTable{{Key: "Deals", Name: "Deals", Fields: []ImportField{{Key: "c1", Name: "Name"}, {Key: "c7", Name: "Signed"}}}}}
	id := uuid.New()
	made := madeOf(base, &ImportedBase{Tables: map[string]uuid.UUID{"Deals": id}, RowsMade: 3, Notes: []ImportNote{
		{Table: "Deals", Field: "c7", Code: "TIMES_LEFT_OUT", Msg: "times"},
		{Table: "Lists", Code: "SHEET_LEFT_OUT", Msg: "hidden"},
		{Table: "Deals", Field: "c9", Row: "12", Code: "CELL_LEFT_OUT", Msg: "a cell"},
	}})
	var said []string
	for _, s := range made.Skipped {
		said = append(said, s.What+": "+s.Why)
	}
	if want := "[Deals / Signed: times Lists: hidden Deals / c9 / 12: a cell]"; fmt.Sprint(said) != want {
		t.Errorf("the import says %v, want %s", said, want)
	}
	if len(made.Created) != 1 || made.Created[0].UUID != id.String() || made.Count != 3 || made.Show == nil {
		t.Errorf("the import made %+v", made)
	}
}

// A spreadsheet's preview shows how its text was read and each sheet as it
// would come: its guessed columns, its rows' count, its first rows as the
// file shows them, and what its columns keep less than whole. Hidden and
// empty sheets are shown too, for the person to choose.
func TestASpreadsheetsPreviewShowsItsSheetsAsRead(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sheets", "libreoffice-deals.xlsx"))
	if err != nil {
		t.Fatal(err)
	}
	in := contentIOBusiness.Input{Filename: "libreoffice-deals.xlsx", Size: int64(len(data)), File: bytes.NewReader(data)}
	raw, err := sheetImporter{}.Preview(context.Background(), accessBusiness.Subject{UserUUID: uuid.NewString()}, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	var p sheetPreview
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if p.Signs != formula.PlainSigns || !p.DayFirst || p.Zone != "UTC" {
		t.Errorf("read with %+v, day first %v, in %s; want plain signs, day first, UTC", p.Signs, p.DayFirst, p.Zone)
	}
	var sheets []string
	for _, s := range p.Sheets {
		sheets = append(sheets, fmt.Sprintf("%d %s hidden=%v rows=%d fields=%d sample=%d notes=%d", s.Index, s.Name, s.Hidden, s.Rows, len(s.Fields), len(s.Sample), len(s.Notes)))
	}
	if want := "[0 Deals hidden=false rows=3 fields=15 sample=3 notes=3 1 Lists hidden=true rows=2 fields=1 sample=2 notes=0 2 Empty hidden=false rows=0 fields=0 sample=0 notes=0]"; fmt.Sprint(sheets) != want {
		t.Fatalf("the preview's sheets are\n%v,\nwant\n%s", sheets, want)
	}
	if got, want := strings.Join(p.Sheets[0].Sample[0], "|"), "Ada|North|$1200.5|₹50|12.5%|2026-10-23|2026-10-23 14:30|TRUE|1:30|9:30|$2401|Big deal|#DIV/0!|00123|3"; got != want {
		t.Errorf("Deals' first row shows %s, want %s", got, want)
	}
	if f := p.Sheets[0].Fields[2]; f.Key != "c3" || f.Name != "Amount" || f.Type != "number" || fmt.Sprint(f.Config) != "map[format:map[currency:USD]]" {
		t.Errorf("Deals' Amount shows as %+v", f)
	}

	// Read as the person says, a sheet's columns are guessed anew: numbers
	// written with a comma before their decimals.
	prices := []byte("Item;Price\nTea;1,50\nCake;3,25\n")
	csv := contentIOBusiness.Input{Filename: "Prices.csv", Size: int64(len(prices)), File: bytes.NewReader(prices)}
	for choices, want := range map[string]string{"": "text", `{"signs":{"decimal":",","group":"."}}`: "number"} {
		raw, err := sheetImporter{}.Preview(context.Background(), accessBusiness.Subject{UserUUID: uuid.NewString()}, csv, json.RawMessage(choices))
		if err != nil {
			t.Fatal(err)
		}
		var p sheetPreview
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Sheets) != 1 || len(p.Sheets[0].Fields) != 2 {
			t.Fatalf("read with %q, the preview is %+v", choices, p.Sheets)
		}
		if got := p.Sheets[0].Fields[1].Type; got != want {
			t.Errorf("read with %q, Price is %s, want %s", choices, got, want)
		}
	}
}

// The person's confirmed choices make the tables: a hidden sheet they chose
// comes, a sheet they left out doesn't (and isn't said), and each column is
// the name and kind they chose, or left out.
func TestTheChoicesMakeTheTablesThePersonChose(t *testing.T) {
	yes, no := true, false
	base, err := sheetBase(workbookFixture(t, "libreoffice-deals.xlsx"), SheetOptions{Signs: formula.PlainSigns, DayFirst: true, Zone: time.UTC}, []sheetChoice{
		{Index: 0, Fields: []fieldChoice{{Key: "c2", Type: "select"}, {Key: "c3", Name: "Value"}, {Key: "c12", Type: "text"}, {Key: "c14", Leave: true}}},
		{Index: 1, Bring: &yes},
		{Index: 2, Bring: &no},
	}, nil, func(int, int) {})
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for _, tb := range base.Tables {
		var fields []string
		for _, f := range tb.Fields {
			kind := f.Name + ":" + f.Type
			if opts := fieldTypes.Options(f.Config); len(opts) > 0 {
				var labels []string
				for _, o := range opts {
					labels = append(labels, fieldTypes.OptionLabel(o))
				}
				kind += fmt.Sprint(labels)
			}
			fields = append(fields, kind)
		}
		tables = append(tables, tb.Name+fmt.Sprint(fields))
	}
	want := "[Deals[Name:text Region:select[North South] Value:number Fee:number Share:number Closed:date Signed:date Won:checkbox Hours:duration Starts:duration Total:number Notes:text Ratio:number Size:number] Lists[Status:text]]"
	if fmt.Sprint(tables) != want {
		t.Errorf("the tables are\n%v,\nwant\n%s", tables, want)
	}
	var notes []string
	for _, n := range base.Notes {
		notes = append(notes, n.Table+"/"+n.Field+":"+n.Code)
	}
	if want := "[Deals/c7:TIMES_LEFT_OUT Deals/c11:FORMULA_AS_VALUES Deals/c13:CELL_LEFT_OUT]"; fmt.Sprint(notes) != want {
		t.Errorf("the notes are %v, want %s", notes, want)
	}
	var rows []string
	if err := base.Tables[0].Rows(func(page []ImportRow) error {
		for _, r := range page {
			_, code := r.Cells["c14"]
			rows = append(rows, fmt.Sprint(r.Cells["c2"], " ", r.Cells["c3"], " ", code))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(rows) != "[North 1200.5 false <nil> 30 false South 4 false]" {
		t.Errorf("Deals' rows hold %v", rows)
	}
}

// A kind the person chooses that some of a column's cells aren't leaves
// those cells empty and says how many; a name they choose that another
// column has is told apart; and the server's own zone isn't one they can
// choose.
func TestAChosenKindSaysWhatItLeavesOut(t *testing.T) {
	sheet := sheetOfRows([]string{"Name", "Amount", "Note"}, []string{"Ada", "1", "a"}, []string{"Bo", "x", "b"}, []string{"Cy", "y", "c"})
	chosen := []sheetChoice{{Index: 0, Fields: []fieldChoice{{Key: "c2", Type: "number"}, {Key: "c3", Name: "name"}}}}
	base, err := sheetBase([]contentIOBusiness.Sheet{sheet}, SheetOptions{Signs: formula.PlainSigns}, chosen, nil, func(int, int) {})
	if err != nil || len(base.Tables) != 1 {
		t.Fatalf("%d tables, %v", len(base.Tables), err)
	}
	var notes, names []string
	for _, n := range base.Notes {
		notes = append(notes, n.Table+"/"+n.Field+":"+n.Code+" "+n.Msg)
	}
	for _, f := range base.Tables[0].Fields {
		names = append(names, f.Name+":"+f.Type)
	}
	if want := "[Sheet/c2:CELL_LEFT_OUT 2 of its cells aren't numbers, so they're left empty.]"; fmt.Sprint(notes) != want {
		t.Errorf("the notes are %v, want %s", notes, want)
	}
	if want := "[Name:text Amount:number name 2:text]"; fmt.Sprint(names) != want {
		t.Errorf("the fields are %v, want %s", names, want)
	}
	var amounts []string
	if err := base.Tables[0].Rows(func(page []ImportRow) error {
		for _, r := range page {
			amounts = append(amounts, fmt.Sprint(r.Cells["c2"]))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(amounts) != "[1 <nil> <nil>]" {
		t.Errorf("the amounts are %v", amounts)
	}
	if _, _, err := readChoices(json.RawMessage(`{"zone":"Local"}`)); err == nil || !strings.Contains(err.Error(), "isn't a time zone") {
		t.Errorf("the server's own zone was taken: %v", err)
	}
}

// A length of time chosen for a column reads a cell that isn't one by its
// text, as people write durations (1:30, 90m, a bare 90 as minutes), so a
// CSV's durations and a workbook's numbers are kept and not said to be left
// empty; only the cells it can't read are. A written duration with seconds
// shows them.
func TestAChosenDurationSaysOnlyTheCellsItCantRead(t *testing.T) {
	sheet := contentIOBusiness.Sheet{Name: "Sheet", Rows: func(row func([]contentIOBusiness.Cell) error) error {
		for _, cells := range [][]contentIOBusiness.Cell{
			{{Text: "Task"}, {Text: "Spent"}, {Text: "Left"}},
			{{Text: "Plan"}, {Text: "1:30"}, {Text: "1:30"}},
			{{Text: "Build"}, {Text: "90m"}, {Text: "soon"}},
			{{Text: "Ship"}, {Text: "90"}, {Text: "later"}},
			{{Text: "Test"}, {Text: "45", Kind: contentIOBusiness.CellNumber, Number: 45}, {Text: "TRUE", Kind: contentIOBusiness.CellBool, Bool: true}},
			{{Text: "Wait"}, {Text: "1:30:15"}, {Text: ""}},
		} {
			if err := row(cells); err != nil {
				return err
			}
		}
		return nil
	}}
	chosen := []sheetChoice{{Index: 0, Fields: []fieldChoice{{Key: "c2", Type: "duration"}, {Key: "c3", Type: "duration"}}}}
	base, err := sheetBase([]contentIOBusiness.Sheet{sheet}, SheetOptions{Signs: formula.PlainSigns}, chosen, nil, func(int, int) {})
	if err != nil || len(base.Tables) != 1 {
		t.Fatalf("%d tables, %v", len(base.Tables), err)
	}
	var notes []string
	for _, n := range base.Notes {
		notes = append(notes, n.Field+":"+n.Code+" "+n.Msg)
	}
	if want := "[c3:CELL_LEFT_OUT 3 of its cells aren't lengths of time, so they're left empty.]"; fmt.Sprint(notes) != want {
		t.Errorf("the notes are %v, want %s", notes, want)
	}
	formats := map[string]any{}
	for _, f := range base.Tables[0].Fields {
		if f.Type == "duration" {
			formats[f.Key] = f.Config["format"]
		}
	}
	if want := "map[c2:h:mm:ss c3:h:mm]"; fmt.Sprint(formats) != want {
		t.Errorf("the durations show as %v, want %s", formats, want)
	}
	var kept []string
	if err := base.Tables[0].Rows(func(page []ImportRow) error {
		for _, r := range page {
			kept = append(kept, fmt.Sprint(r.Cells["c2"], "/", r.Cells["c3"]))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := "[5400/5400 5400/<nil> 5400/<nil> 2700/<nil> 5415/<nil>]"; fmt.Sprint(kept) != want {
		t.Errorf("the durations kept are %v, want %s", kept, want)
	}
}

// Choices that can't be followed are said, never guessed at.
func TestChoicesThatCantBeFollowedAreSaid(t *testing.T) {
	for choices, says := range map[string]string{
		`{"day_first":"yes"}`:                   "couldn't be read",
		`{"signs":{"decimal":".","group":"."}}`: "aren't written with",
		`{"zone":"Mars/Olympus"}`:               "isn't a time zone",
	} {
		if _, _, err := readChoices(json.RawMessage(choices)); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want it said that it %s", choices, err, says)
		}
	}
	sheets := workbookFixture(t, "libreoffice-deals.xlsx")
	for _, c := range []struct {
		chosen []sheetChoice
		says   string
	}{
		{[]sheetChoice{{Index: 9}}, "a sheet the file doesn't have"},
		{[]sheetChoice{{Index: 0, Fields: []fieldChoice{{Key: "c99", Type: "text"}}}}, "a column the sheet doesn't have"},
		{[]sheetChoice{{Index: 0, Fields: []fieldChoice{{Key: "c1", Type: "person"}}}}, `can't be made a "person" field`},
		{[]sheetChoice{{Index: 0, MatchBy: "c1"}}, "only when the sheet comes into a table"},
		{[]sheetChoice{{Index: 0, Fields: []fieldChoice{{Key: "c1", Field: "a field"}}}}, "only when the sheet comes into a table"},
	} {
		if _, err := sheetBase(sheets, SheetOptions{}, c.chosen, nil, func(int, int) {}); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%+v: %v, want it said: %s", c.chosen, err, c.says)
		}
	}
}
