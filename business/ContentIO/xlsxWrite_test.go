package business

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

// writeBook writes sheets as a workbook, and opens it as an import reads one.
func writeBook(t *testing.T, sheets ...WorkbookSheet) ([]string, []byte) {
	t.Helper()
	var b bytes.Buffer
	notes, err := WriteXLSX(&b, sheets)
	if err != nil {
		t.Fatal(err)
	}
	return notes, b.Bytes()
}

// rowsOfCells is a WorkbookSheet's rows from cells.
func rowsOfCells(rows [][]Cell) func(func([]Cell) error) error {
	return func(row func([]Cell) error) error {
		for _, r := range rows {
			if err := row(r); err != nil {
				return err
			}
		}
		return nil
	}
}

// partOf is a written workbook's part, as text.
func partOf(t *testing.T, book []byte, name string) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(book), int64(len(book)))
	if err != nil {
		t.Fatal(err)
	}
	f, err := zr.Open(name)
	if err != nil {
		t.Fatalf("the workbook has no %s: %v", name, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// cellsRead are a sheet's rows as ReadSheets reads them.
func cellsRead(t *testing.T, s Sheet) [][]Cell {
	t.Helper()
	var out [][]Cell
	if err := s.Rows(func(cells []Cell) error {
		out = append(out, slices.Clone(cells))
		return nil
	}); err != nil {
		t.Fatalf("reading %s: %v", s.Name, err)
	}
	return out
}

// A workbook written is read back as it was written: each cell the kind it
// was, its value, its percent or its currency, and text as it was, with what
// a workbook's text escapes, what XML escapes and what looks like a formula
// still text.
func TestAWorkbookWrittenIsReadBackAsWritten(t *testing.T) {
	day := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	at := time.Date(2026, 10, 23, 14, 30, 5, 0, time.UTC)
	texts := []string{"Ada", "_x0041_ stays as written", "two\nlines", "  padded  ", `<&>"'`, "=SUM(A1:A2)", "🙂 and ₹", "carriage\rreturn"}
	var rows [][]Cell
	for i, s := range texts {
		rows = append(rows, []Cell{
			{Text: s},
			numberCell(1200.5 + float64(i)),
			percentCell(0.125),
			moneyCell(-12.25, "$"),
			moneyCell(100, "₹"),
			BoolCell(i%2 == 0),
			DayCell(day.AddDate(0, 0, i)),
			DayCell(at),
			DurationCell(5415),
			DurationCell(5400),
			{Kind: CellError, Text: "#DIV/0!"},
		})
	}
	cols := []WorkbookColumn{{Name: "Name"}, {Name: "Amount"}, {Name: "Share"}, {Name: "Price"}, {Name: "Fee"}, {Name: "Done"}, {Name: "Due"}, {Name: "At"}, {Name: "Took"}, {Name: "Planned"}, {Name: "Ratio"}}
	notes, book := writeBook(t, WorkbookSheet{Name: "Deals", Columns: cols, Rows: rowsOfCells(rows)})
	if len(notes) != 0 {
		t.Errorf("a workbook within Excel's limits noted %q", notes)
	}
	sheets, err := ReadSheets(sheetOf("Deals.xlsx", book))
	if err != nil {
		t.Fatal(err)
	}
	if len(sheets) != 1 || sheets[0].Name != "Deals" || sheets[0].Hidden {
		t.Fatalf("the workbook's sheets are %+v", sheets)
	}
	got := cellsRead(t, sheets[0])
	if len(got) != len(rows)+1 {
		t.Fatalf("read %d rows, want the header and %d", len(got), len(rows))
	}
	var header []string
	for _, c := range got[0] {
		header = append(header, c.Text)
	}
	if want := "[Name Amount Share Price Fee Done Due At Took Planned Ratio]"; fmt.Sprint(header) != want {
		t.Errorf("the header is %v, want %s", header, want)
	}
	for r, row := range rows {
		for c, want := range row {
			if c >= len(got[r+1]) {
				t.Errorf("row %d has %d cells, want %d", r+1, len(got[r+1]), len(row))
				break
			}
			have := got[r+1][c]
			if have.Kind != want.Kind || have.Text != want.Text || have.Number != want.Number || have.Bool != want.Bool ||
				have.Percent != want.Percent || have.Currency != want.Currency || !have.Time.Equal(want.Time) || have.Formula {
				t.Errorf("row %d, %s: read %+v, want %+v", r+1, cols[c].Name, have, want)
			}
		}
	}
}

// A workbook keeps within what Excel holds, and its Notes sheet says what
// was cut: a sheet's name that Excel can't take, two sheets named alike, and
// a cell longer than Excel's.
func TestAWorkbookKeepsWithinExcelsLimits(t *testing.T) {
	long := strings.Repeat("ab", excelCellChars)
	one := func(name string, cells ...Cell) WorkbookSheet {
		return WorkbookSheet{Name: name, Columns: []WorkbookColumn{{Name: "Note"}}, Rows: rowsOfCells([][]Cell{cells})}
	}
	notes, book := writeBook(t,
		one("Q3/Q4: plan [draft] for the whole of the sales team", Cell{Text: long}),
		one("deals", Cell{Text: "a"}),
		one("Deals", Cell{Text: "b"}),
		one("History", Cell{Text: "c"}),
		one("'quoted'", Cell{Text: "d\x00e\x1ff"}),
	)
	sheets, err := ReadSheets(sheetOf("Book.xlsx", book))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range sheets {
		names = append(names, s.Name)
	}
	if want := "[Q3 Q4  plan  draft  for the who deals Deals (2) History (2) quoted Notes]"; fmt.Sprint(names) != want {
		t.Errorf("the sheets are named %q, want %s", names, want)
	}
	if got := cellsRead(t, sheets[0])[1][0].Text; got != long[:excelCellChars] {
		t.Errorf("the long cell holds %d characters, want %d", len(got), excelCellChars)
	}
	if got := cellsRead(t, sheets[4])[1][0].Text; got != "def" {
		t.Errorf("the cell with control characters holds %q, want them left out", got)
	}
	if len(notes) != 5 {
		t.Fatalf("the notes are %q, want one for each of 4 names and the long cell", notes)
	}
	var said []string
	for _, row := range cellsRead(t, sheets[len(sheets)-1])[1:] {
		said = append(said, row[0].Text)
	}
	if !slices.Equal(said, notes) {
		t.Errorf("the Notes sheet says %q, want %q", said, notes)
	}
	if !strings.HasPrefix(notes[4], "A cell held more than the 32,767 characters") {
		t.Errorf("the note for the long cell is %q", notes[4])
	}
}

// A sheet holds the rows Excel does, its header among them: the rest aren't
// written, and that's said.
func TestASheetHoldsTheRowsExcelDoes(t *testing.T) {
	rows := func(row func([]Cell) error) error {
		for i := range maxSheetRows + 5 {
			if err := row([]Cell{{Kind: CellNumber, Number: float64(i), Text: fmt.Sprint(i)}}); err != nil {
				return err
			}
		}
		return nil
	}
	notes, book := writeBook(t, WorkbookSheet{Name: "Many", Columns: []WorkbookColumn{{Name: "N"}}, Rows: rows})
	if len(notes) != 1 || !strings.Contains(notes[0], "more rows than the 1,048,576 Excel holds") {
		t.Fatalf("the notes are %q", notes)
	}
	sheets, err := ReadSheets(sheetOf("Many.xlsx", book))
	if err != nil {
		t.Fatal(err)
	}
	n, last := 0, Cell{}
	if err := sheets[0].Rows(func(cells []Cell) error {
		n++
		last = cells[0]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n != maxSheetRows || last.Number != maxSheetRows-2 {
		t.Errorf("read %d rows ending at %v, want %d ending at %d", n, last.Number, maxSheetRows, maxSheetRows-2)
	}
}

// A sheet's header stays in sight, its columns are as wide as asked, and a
// column of a select's options offers them as a dropdown: written in the
// rule when they fit there, else from a hidden sheet of lists.
func TestASheetsHeaderStaysAndItsSelectsOfferTheirOptions(t *testing.T) {
	var many []string
	for i := range 60 {
		many = append(many, fmt.Sprintf("Country %d", i))
	}
	cols := []WorkbookColumn{
		{Name: "Name", Width: 30},
		{Name: "Stage", Choices: []string{"Lead", "Won", "Lost"}},
		{Name: "Country", Choices: many},
		{Name: "Tag", Choices: []string{"a,b", "c"}},
		{Name: "Story", Width: 400},
	}
	_, book := writeBook(t, WorkbookSheet{Name: "Deals", Columns: cols, Rows: rowsOfCells([][]Cell{{{Text: "Ada"}, {Text: "Won"}, {Text: "Country 3"}, {Text: "c"}, {}}})})
	sheet := partOf(t, book, "xl/worksheets/sheet1.xml")
	for _, want := range []string{
		`<pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/>`,
		`<col min="1" max="1" width="30.00" customWidth="1"/>`,
		`<col min="5" max="5" width="255.00" customWidth="1"/>`, // as wide as Excel goes
		`<dataValidations count="3">`,
		`sqref="B2:B1048576"><formula1>"Lead,Won,Lost"</formula1>`,
		`sqref="C2:C1048576"><formula1>'Lists'!$A$1:$A$60</formula1>`,
		`sqref="D2:D1048576"><formula1>'Lists'!$B$1:$B$2</formula1>`,
	} {
		if !strings.Contains(sheet, want) {
			t.Errorf("the sheet doesn't hold %s", want)
		}
	}
	if strings.Contains(sheet, "<f>") {
		t.Error("the sheet holds a formula")
	}
	sheets, err := ReadSheets(sheetOf("Deals.xlsx", book))
	if err != nil {
		t.Fatal(err)
	}
	if len(sheets) != 2 || sheets[1].Name != "Lists" || !sheets[1].Hidden {
		t.Fatalf("the sheets are %+v, want Deals and a hidden Lists", sheets)
	}
	lists := cellsRead(t, sheets[1])
	if len(lists) != 60 || lists[0][0].Text != "Country 0" || lists[0][1].Text != "a,b" || lists[1][1].Text != "c" || lists[59][0].Text != "Country 59" {
		t.Errorf("the lists hold %v", lists[:2])
	}
}

// A day is the serial number Excel gives it, and back: Excel's days count a
// 29 February 1900 that never was, and a day it has no serial for is
// written as text.
func TestAnExcelSerialIsWrittenForTheDayItNames(t *testing.T) {
	for _, c := range []struct {
		day  time.Time
		want float64
		ok   bool
	}{
		{time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC), 1, true},
		{time.Date(1900, 2, 28, 0, 0, 0, 0, time.UTC), 59, true},
		{time.Date(1900, 3, 1, 0, 0, 0, 0, time.UTC), 61, true},
		{time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC), 46318.5, true},
		{time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), 2958465, true},
		{time.Date(1899, 12, 31, 0, 0, 0, 0, time.UTC), 0, false},
		{time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), 0, false},
	} {
		got, ok := excelSerial(c.day)
		if got != c.want || ok != c.ok {
			t.Errorf("excelSerial(%s) = %v, %t; want %v, %t", c.day.Format(time.DateTime), got, ok, c.want, c.ok)
		}
		if back, _ := excelDay(got, false); ok && !back.Equal(c.day) {
			t.Errorf("serial %v reads back as %s, want %s", got, back, c.day)
		}
	}
	_, book := writeBook(t, WorkbookSheet{Name: "Old", Columns: []WorkbookColumn{{Name: "Day"}}, Rows: rowsOfCells([][]Cell{{DayCell(time.Date(1850, 6, 1, 0, 0, 0, 0, time.UTC))}})})
	sheets, err := ReadSheets(sheetOf("Old.xlsx", book))
	if err != nil {
		t.Fatal(err)
	}
	if got := cellsRead(t, sheets[0])[1][0]; got.Kind != CellText || got.Text != "1850-06-01" {
		t.Errorf("a day before 1900 is read as %+v, want its text", got)
	}
}

// A choice holding what XML can't (a control character) is listed on the
// hidden Lists sheet, written as the column's cells are, never raw into the
// dropdown's rule: every part of the workbook stays XML, and it reads back.
func TestAChoiceXMLCantHoldIsListedAsItsCellsAreWritten(t *testing.T) {
	cols := []WorkbookColumn{{Name: "Stage", Choices: []string{"On\x01hold", "Done"}}}
	_, book := writeBook(t, WorkbookSheet{Name: "Deals", Columns: cols, Rows: rowsOfCells([][]Cell{{{Text: "On\x01hold"}}})})
	zr, err := zip.NewReader(bytes.NewReader(book), int64(len(book)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		d := xml.NewDecoder(strings.NewReader(partOf(t, book, f.Name)))
		for {
			_, err := d.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("%s isn't XML: %v", f.Name, err)
			}
		}
	}
	if sheet := partOf(t, book, "xl/worksheets/sheet1.xml"); !strings.Contains(sheet, `<formula1>'Lists'!$A$1:$A$2</formula1>`) {
		t.Error("the dropdown's rule doesn't name its column of the Lists sheet")
	}
	sheets, err := ReadSheets(sheetOf("Deals.xlsx", book))
	if err != nil {
		t.Fatal(err)
	}
	if len(sheets) != 2 {
		t.Fatalf("read %d sheets, want Deals and Lists", len(sheets))
	}
	if got := cellsRead(t, sheets[1]); len(got) != 2 || got[0][0].Text != "Onhold" || got[1][0].Text != "Done" {
		t.Errorf("the Lists sheet holds %v, want Onhold and Done", got)
	}
	if got := cellsRead(t, sheets[0])[1][0].Text; got != "Onhold" {
		t.Errorf("the cell holds %q, want Onhold, as its choice is listed", got)
	}
}

// A length of time with seconds is written under the workbook's own
// [h]:mm:ss, so hours past a day stay hours (a reader may take builtin 46 for
// a time of day), and it reads back as written.
func TestALengthOfTimeWithSecondsCountsHoursPastADay(t *testing.T) {
	cols := []WorkbookColumn{{Name: "Took", Seconds: true}}
	_, book := writeBook(t, WorkbookSheet{Name: "Runs", Columns: cols, Rows: rowsOfCells([][]Cell{{DurationCell(90061)}, {DurationCell(5415)}})})
	if styles := partOf(t, book, "xl/styles.xml"); !strings.Contains(styles, `formatCode="[h]:mm:ss"`) || strings.Contains(styles, `numFmtId="46"`) {
		t.Errorf("the styles are %s, want the workbook's own [h]:mm:ss, not builtin 46", styles)
	}
	sheets, err := ReadSheets(sheetOf("Runs.xlsx", book))
	if err != nil {
		t.Fatal(err)
	}
	got := cellsRead(t, sheets[0])
	for i, want := range []float64{90061, 5415} {
		if c := got[i+1][0]; c.Kind != CellDuration || c.Number != want {
			t.Errorf("row %d reads %+v, want a length of time of %v s", i+1, c, want)
		}
	}
}
