package business

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	formula "github.com/akashc777/OneCamp/business/Formula"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// sheetOfRows is a sheet of rows of text.
func sheetOfRows(rows ...[]string) contentIOBusiness.Sheet {
	return contentIOBusiness.Sheet{Name: "Sheet", Rows: func(row func([]contentIOBusiness.Cell) error) error {
		for _, r := range rows {
			cells := make([]contentIOBusiness.Cell, len(r))
			for i, s := range r {
				cells[i] = contentIOBusiness.Cell{Text: s}
			}
			if err := row(cells); err != nil {
				return err
			}
		}
		return nil
	}}
}

// column is a sheet of one column, named Col, holding cells.
func column(cells ...string) contentIOBusiness.Sheet {
	rows := [][]string{{"Col"}}
	for _, c := range cells {
		rows = append(rows, []string{c})
	}
	return sheetOfRows(rows...)
}

// A column is the narrowest kind all its cells fit, and each cell is stored
// as that kind's field stores it, so a sheet comes in as the table someone
// would have set up by hand.
func TestAColumnIsTheNarrowestKindAllItsCellsFit(t *testing.T) {
	plain := SheetOptions{Signs: formula.PlainSigns}
	ist, _ := time.LoadLocation("Asia/Kolkata")
	cases := []struct {
		name   string
		cells  []string
		opts   SheetOptions
		kind   string
		config string // the config as fmt prints it, options as their labels
		cell   string // a cell, and as it's stored ("<nil>": left out)
		stored string
	}{
		{"numbers as people write them", []string{"1", "2.5", "-3", "1,200.50"}, plain, model.FieldNumber, "map[]", "1,200.50", "1200.5"},
		{"percents", []string{"12%", "7.5 %"}, plain, model.FieldNumber, "map[format:map[percent:true]]", "12%", "12"},
		{"dollars", []string{"$1,200", "$3.50"}, plain, model.FieldNumber, "map[format:map[currency:USD]]", "$3.50", "3.5"},
		{"rupees, grouped as in India", []string{"₹1,00,000", "₹50"}, plain, model.FieldNumber, "map[format:map[currency:INR]]", "₹1,00,000", "100000"},
		{"euros, a sign no column shows", []string{"€12", "€3"}, plain, model.FieldNumber, "map[]", "€12", "12"},
		{"numbers with a comma for the decimal sign", []string{"1.234,5", "2,5"}, SheetOptions{Signs: formula.NumberSigns{Decimal: ",", Group: "."}}, model.FieldNumber, "map[]", "1.234,5", "1234.5"},
		{"ids with leading zeros stay text", []string{"00123", "00456"}, plain, model.FieldText, "map[]", "00123", "00123"},
		{"yes and no", []string{"yes", "no", "Yes"}, plain, model.FieldCheckbox, "map[]", "Yes", "true"},
		{"an unticked box holds no value", []string{"true", "false"}, plain, model.FieldCheckbox, "map[]", "false", "<nil>"},
		{"ones and zeros are counts", []string{"1", "0", "1"}, plain, model.FieldNumber, "map[]", "0", "0"},
		{"ISO dates", []string{"2026-10-23", "2026-01-05"}, plain, model.FieldDate, "map[]", "2026-01-05", "2026-01-05"},
		{"day first, as a day past 12 says", []string{"23/10/2026", "05/01/2026"}, plain, model.FieldDate, "map[]", "05/01/2026", "2026-01-05"},
		{"month first, as a day past 12 says", []string{"10/23/2026", "01/05/2026"}, plain, model.FieldDate, "map[]", "01/05/2026", "2026-01-05"},
		{"day first when nothing says, for one who writes it so", []string{"03/04/2026"}, SheetOptions{DayFirst: true}, model.FieldDate, "map[]", "03/04/2026", "2026-04-03"},
		{"month first when nothing says, for one who writes it so", []string{"03/04/2026"}, SheetOptions{}, model.FieldDate, "map[]", "03/04/2026", "2026-03-04"},
		{"months in words", []string{"23 Oct 2026", "Oct 5, 2026", "5 January 2026"}, plain, model.FieldDate, "map[]", "Oct 5, 2026", "2026-10-05"},
		{"times, their day kept", []string{"2026-10-23 14:30", "23/10/2026 9:05 PM"}, SheetOptions{Zone: ist}, model.FieldDate, "map[]", "2026-10-23 14:30", "2026-10-23"},
		{"a time just past midnight keeps its day", []string{"2026-10-23 01:00"}, SheetOptions{Zone: ist}, model.FieldDate, "map[]", "2026-10-23 01:00", "2026-10-23"},
		{"a time with its own zone", []string{"2026-10-23T14:30:00+02:00"}, plain, model.FieldDate, "map[]", "2026-10-23T14:30:00+02:00", "2026-10-23"},
		{"a moment falls on its day where the importer is", []string{"2026-10-23T22:30:00Z"}, SheetOptions{Zone: ist}, model.FieldDate, "map[]", "2026-10-23T22:30:00Z", "2026-10-24"},
		{"a database's fractions of a second", []string{"2026-10-23 14:30:00.000", "2026-01-05 09:05:59.123"}, plain, model.FieldDate, "map[]", "2026-01-05 09:05:59.123", "2026-01-05"},
		{"not a day", []string{"31/02/2026", "2026-10-23"}, plain, model.FieldText, "map[]", "31/02/2026", "31/02/2026"},
		{"emails", []string{"ada@example.com", "bo@example.org"}, plain, model.FieldEmail, "map[]", "bo@example.org", "bo@example.org"},
		{"links", []string{"https://onemana.dev/pricing", "www.example.com"}, plain, model.FieldURL, "map[]", "www.example.com", "www.example.com"},
		{"phone numbers", []string{"+91 98765 43210", "(555) 123-4567"}, plain, model.FieldPhone, "map[]", "(555) 123-4567", "(555) 123-4567"},
		{"phone numbers with an extension", []string{"555-0100 ext 12", "+44 20 7946 0958 x3"}, plain, model.FieldPhone, "map[]", "555-0100 ext 12", "555-0100 ext 12"},
		{"ticks as marks and letters", []string{"✔", "✗", "y", "N", "✘"}, plain, model.FieldCheckbox, "map[]", "✔", "true"},
		{"a few values over and over", []string{"Open", "Closed", "Open", "Open", "Closed"}, plain, model.FieldSelect, "[Open Closed]", "Closed", "Closed"},
		{"lists of a few values", []string{"red, blue", "blue", "green, red", "red", "blue, green"}, plain, model.FieldMultiSelect, "[red blue green]", "green, red", "[green red]"},
		{"values that don't repeat", []string{"Ada", "Bo", "Cy"}, plain, model.FieldText, "map[]", "Bo", "Bo"},
		{"a line break", []string{"one\ntwo", "three"}, plain, model.FieldLongText, "map[]", "three", "three"},
		{"numbers and words", []string{"12", "twelve"}, plain, model.FieldText, "map[]", "twelve", "twelve"},
		{"nothing", []string{"", ""}, plain, model.FieldText, "map[]", "", "<nil>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := GuessColumns(column(c.cells...), c.opts)
			if err != nil || len(got.Fields) != 1 {
				t.Fatalf("%v, %d fields", err, len(got.Fields))
			}
			f := got.Fields[0]
			config := fmt.Sprint(f.Config)
			if opts := fieldTypes.Options(f.Config); len(opts) > 0 {
				var labels []string
				for _, o := range opts {
					labels = append(labels, fieldTypes.OptionLabel(o))
				}
				config = fmt.Sprint(labels)
			}
			if f.Type != c.kind || config != c.config {
				t.Errorf("a column of %q is %s %s, want %s %s", c.cells, f.Type, config, c.kind, c.config)
			}
			stored := got.Row([]contentIOBusiness.Cell{{Text: c.cell}})
			v, ok := stored[f.Key]
			if fmt.Sprint(v) != c.stored || ok != (c.stored != "<nil>") {
				t.Errorf("%q is stored as %v (%v), want %s", c.cell, v, ok, c.stored)
			}
			// What the import stores is what its field's kind takes.
			if ok {
				config, _ := json.Marshal(f.Config)
				field := &model.Field{Id: uuid.New(), Name: f.Name, Type: f.Type, Config: string(config)}
				if _, err := fieldTypes.Kinds.Check([]*model.Field{field}, map[string]any{field.Id.String(): v}, fieldTypes.Input{}); err != nil {
					t.Errorf("%q is stored as %v, which a %s field refuses: %v", c.cell, v, f.Type, err)
				}
			}
		})
	}
}

// A spreadsheet that says more of its cells than a file does is taken at its
// word: a column of dropdowns is a select of the dropdown's values in its
// order (with any value typed past them), one of people chips is people by
// email, and one of links is a url holding where each goes, so a Google Sheet
// comes in as its owner set it up (tables S1 slice 6).
func TestWhatASheetSaysOfItsCellsChoosesTheirKind(t *testing.T) {
	type cell = contentIOBusiness.Cell
	stages := []string{"Lead", "Won", "Lost"}
	pick := func(s string) cell { return cell{Text: s, Choices: stages} }
	yesNo := func(s string) cell { return cell{Text: s, Choices: []string{"Yes", "No"}} }
	chip := func(text string, emails ...string) cell { return cell{Text: text, People: emails} }
	link := func(text, to string) cell { return cell{Text: text, Link: to} }
	cases := []struct {
		name    string
		cells   []cell
		kind    string
		options string
		cell    cell
		stored  string
		notes   string
	}{
		{"dropdowns, in the dropdown's order", []cell{pick("Won"), pick("Lead"), pick("Won")}, model.FieldSelect, "[Lead Won Lost]", pick("Lost"), "Lost", "[]"},
		{"a dropdown holding a value typed past its list", []cell{pick("Won"), pick("Paused")}, model.FieldSelect, "[Lead Won Lost Paused]", pick("Paused"), "Paused", "[]"},
		{"dropdowns nothing is picked in yet", []cell{pick(""), pick("")}, model.FieldSelect, "[Lead Won Lost]", pick(""), "<nil>", "[]"},
		{"dropdowns of yes and no stay a select", []cell{yesNo("Yes"), yesNo("No")}, model.FieldSelect, "[Yes No]", yesNo("No"), "No", "[]"},
		{"people chips", []cell{chip("Ann Lee", "ann@example.com"), chip("Bo, Cy", "bo@example.com", "cy@example.com")}, model.FieldPeople, "[]", chip("Bo, Cy", "bo@example.com", "cy@example.com"), "[{bo@example.com } {cy@example.com }]", "[]"},
		{"chips beside a plain name stay text", []cell{chip("Ann Lee", "ann@example.com"), {Text: "Someone"}}, model.FieldText, "[]", cell{Text: "Someone"}, "Someone", "[]"},
		{"links, kept as where they go", []cell{link("Spec", "https://example.com/spec"), link("https://example.com/b", "https://example.com/b")}, model.FieldURL, "[]", link("Spec", "https://example.com/spec"), "https://example.com/spec", "[LINK_TEXT_LEFT_OUT]"},
		{"links that show where they go", []cell{link("https://example.com/a", "https://example.com/a"), link("https://example.com/b", "https://example.com/b")}, model.FieldURL, "[]", link("https://example.com/a", "https://example.com/a"), "https://example.com/a", "[]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A second column of text gives every row a value, as in a sheet.
			rows := [][]cell{{{Text: "Col"}, {Text: "Other"}}}
			for _, x := range c.cells {
				rows = append(rows, []cell{x, {Text: "x"}})
			}
			got, err := GuessColumns(contentIOBusiness.Sheet{Name: "Sheet", Rows: func(row func([]cell) error) error {
				for _, r := range rows {
					if err := row(r); err != nil {
						return err
					}
				}
				return nil
			}}, SheetOptions{Signs: formula.PlainSigns})
			if err != nil || len(got.Fields) != 2 {
				t.Fatalf("%v, %d fields", err, len(got.Fields))
			}
			f := got.Fields[0]
			var options []string
			if list, ok := f.Config["options"].([]any); ok {
				for _, o := range list {
					options = append(options, fmt.Sprint(o.(map[string]any)["label"]))
				}
			}
			if f.Type != c.kind || fmt.Sprint(options) != c.options {
				t.Errorf("the column is %s with options %v, want %s with %s", f.Type, options, c.kind, c.options)
			}
			stored := got.Row([]cell{c.cell})
			if v, ok := stored[f.Key]; fmt.Sprint(v) != c.stored || ok != (c.stored != "<nil>") {
				t.Errorf("%q is stored as %v (%v), want %s", c.cell.Text, v, ok, c.stored)
			}
			var notes []string
			for _, n := range got.Notes {
				notes = append(notes, n.Code)
			}
			if fmt.Sprint(notes) != c.notes {
				t.Errorf("the notes are %v, want %s", notes, c.notes)
			}
		})
	}
}

// A table takes a sheet's first 500 columns; how many more hold a value is
// said, by its widest row.
func TestColumnsPastWhatATableTakesAreSaid(t *testing.T) {
	const takes = 500
	texts := func(n int, prefix string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s%d", prefix, i+1)
		}
		return out
	}
	narrow := texts(takes-1, "v")
	got, err := GuessColumns(sheetOfRows(texts(takes, "Col "), texts(takes+3, "v"), append(narrow, "", "", "", "", "", "far")), SheetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var notes []string
	for _, n := range got.Notes {
		notes = append(notes, n.Code+": "+n.Msg)
	}
	if len(got.Fields) != takes || fmt.Sprint(notes) != "[COLUMNS_LEFT_OUT: A table takes a sheet's first 500 columns, so the 5 after them aren't brought.]" {
		t.Errorf("%d fields, notes %v", len(got.Fields), notes)
	}
	var cells []contentIOBusiness.Cell
	for _, s := range append(narrow, "last", "past") {
		cells = append(cells, contentIOBusiness.Cell{Text: s})
	}
	if stored := got.Row(cells); len(stored) != takes || stored[fmt.Sprintf("c%d", takes)] != "last" {
		t.Errorf("a row of %d cells is stored as %d", len(cells), len(stored))
	}
}

// The first row names the columns: one with no name, or a name an earlier
// column has, is named so it's told apart, and a row wider than the header
// adds columns.
func TestASheetsFirstRowNamesItsColumns(t *testing.T) {
	got, err := GuessColumns(sheetOfRows(
		[]string{"Name", "", "name"},
		[]string{"Ada", "1", "x", "2026-10-23"},
		[]string{"", "", "", ""},
		[]string{"Bo", "2"},
	), SheetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range got.Fields {
		names = append(names, f.Key+"="+f.Name)
	}
	if want := "[c1=Name c2=Column 2 c3=name 2 c4=Column 4]"; fmt.Sprint(names) != want {
		t.Errorf("columns %v, want %s", names, want)
	}
	if got.Rows != 2 {
		t.Errorf("%d rows under the header, want 2 (an empty one isn't one)", got.Rows)
	}
}

// A workbook's cell that says what it holds is taken as it is: its number,
// whatever its text reads as, its checkbox, its day (the time of day isn't
// kept, and said), its length of time. A formula's error holds nothing, and
// a column of formulas' values says so.
func TestATypedCellIsTakenAsItIs(t *testing.T) {
	type cell = contentIOBusiness.Cell
	num := func(n float64, text string) cell {
		return cell{Kind: contentIOBusiness.CellNumber, Number: n, Text: text}
	}
	usd := func(n float64) cell { c := num(n, "$"+fmt.Sprint(n)); c.Currency = "USD"; return c }
	pct := func(n float64) cell { c := num(n, fmt.Sprint(n)+"%"); c.Percent = true; return c }
	box := func(b bool) cell { return cell{Kind: contentIOBusiness.CellBool, Bool: b, Text: fmt.Sprint(b)} }
	day := func(s string) cell {
		tm, _ := time.Parse("2006-01-02 15:04", s)
		return cell{Kind: contentIOBusiness.CellDate, Time: tm, Text: s}
	}
	dur := func(secs float64) cell {
		return cell{Kind: contentIOBusiness.CellDuration, Number: secs, Text: fmt.Sprint(secs)}
	}
	formulaOf := func(c cell) cell { c.Formula = true; return c }
	failed := cell{Kind: contentIOBusiness.CellError, Text: "#DIV/0!", Formula: true}
	cases := []struct {
		name   string
		cells  []cell
		kind   string
		config string
		cell   cell
		stored string
		notes  string
	}{
		{"amounts in dollars", []cell{usd(1200.5), usd(30)}, model.FieldNumber, "map[format:map[currency:USD]]", usd(30), "30", "[]"},
		{"percents as they show", []cell{pct(12.5), pct(7)}, model.FieldNumber, "map[format:map[percent:true]]", pct(7), "7", "[]"},
		{"a number whose text reads otherwise", []cell{num(1234.5, "1.234,50 €"), num(2, "2,00 €")}, model.FieldNumber, "map[]", num(1234.5, "1.234,50 €"), "1234.5", "[]"},
		{"checkboxes", []cell{box(true), box(false)}, model.FieldCheckbox, "map[]", box(false), "<nil>", "[]"},
		{"days, their times not kept", []cell{day("2026-10-23 14:30"), day("2026-01-05 00:00")}, model.FieldDate, "map[]", day("2026-10-23 01:00"), "2026-10-23", "[TIMES_LEFT_OUT]"},
		{"days", []cell{day("2026-10-23 00:00"), day("2026-01-05 00:00")}, model.FieldDate, "map[]", day("2026-01-05 00:00"), "2026-01-05", "[]"},
		{"lengths of time", []cell{dur(5400), dur(94500)}, model.FieldDuration, "map[format:h:mm]", dur(94500), "94500", "[]"},
		{"lengths of time to the second", []cell{dur(5400), dur(61)}, model.FieldDuration, "map[format:h:mm:ss]", dur(61), "61", "[]"},
		{"a formula's error holds nothing, and is said", []cell{num(1, "1"), failed, num(2, "2")}, model.FieldNumber, "map[]", failed, "<nil>", "[CELL_LEFT_OUT]"},
		{"formulas' values", []cell{formulaOf(num(1, "1")), formulaOf(num(2, "2"))}, model.FieldNumber, "map[]", formulaOf(num(2, "2")), "2", "[FORMULA_AS_VALUES]"},
		{"numbers and words", []cell{num(12, "12"), {Text: "N/A"}}, model.FieldText, "map[]", num(12, "12"), "12", "[]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Each row has a second column of text, so a row whose cell holds
			// nothing still has a value, as a sheet's rows do.
			rows := [][]cell{{{Text: "Col"}, {Text: "Other"}}}
			for _, x := range c.cells {
				rows = append(rows, []cell{x, {Text: "x"}})
			}
			got, err := GuessColumns(contentIOBusiness.Sheet{Name: "Sheet", Rows: func(row func([]cell) error) error {
				for _, r := range rows {
					if err := row(r); err != nil {
						return err
					}
				}
				return nil
			}}, SheetOptions{Signs: formula.PlainSigns})
			if err != nil || len(got.Fields) != 2 {
				t.Fatalf("%v, %d fields", err, len(got.Fields))
			}
			f := got.Fields[0]
			if fmt.Sprint(f.Config) != c.config || f.Type != c.kind {
				t.Errorf("the column is %s %v, want %s %s", f.Type, f.Config, c.kind, c.config)
			}
			stored := got.Row([]cell{c.cell})
			if v, ok := stored[f.Key]; fmt.Sprint(v) != c.stored || ok != (c.stored != "<nil>") {
				t.Errorf("%q is stored as %v (%v), want %s", c.cell.Text, v, ok, c.stored)
			}
			var notes []string
			for _, n := range got.Notes {
				if n.Field != f.Key {
					t.Errorf("a note of field %q, want %q", n.Field, f.Key)
				}
				notes = append(notes, n.Code)
			}
			if fmt.Sprint(notes) != c.notes {
				t.Errorf("the notes are %v, want %s", notes, c.notes)
			}
		})
	}
}

// A sheet's header is its first row with a value: blank rows above it, and
// rows of empty cells, name nothing, as they hold nothing under it.
func TestTheHeaderIsTheFirstRowWithAValue(t *testing.T) {
	s := sheetOfRows([]string{""}, []string{"", " "}, []string{"Name", "Age"}, []string{"Ada", "3"}, []string{" ", ""}, []string{"Bo", "4"})
	cols, err := GuessColumns(s, SheetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range cols.Fields {
		names = append(names, f.Name+":"+f.Type)
	}
	if fmt.Sprint(names) != "[Name:text Age:number]" || cols.Rows != 2 {
		t.Errorf("the columns are %v over %d rows, want [Name:text Age:number] over 2", names, cols.Rows)
	}
	var rows []string
	if err := sheetRows(s, cols, func(int) {})(func(page []ImportRow) error {
		for _, r := range page {
			rows = append(rows, fmt.Sprint(r.Cells["c1"], " ", r.Cells["c2"]))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(rows) != "[Ada 3 Bo 4]" {
		t.Errorf("the rows under the header are %q", rows)
	}
}
