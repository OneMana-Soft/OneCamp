package business

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode/utf16"
)

// sheetOf is a file of data as an import is given it.
func sheetOf(name string, data []byte) Input {
	return Input{Filename: name, Size: int64(len(data)), File: bytes.NewReader(data)}
}

// rowsOf reads a sheet's rows as text.
func rowsOf(t *testing.T, s Sheet) [][]string {
	t.Helper()
	var out [][]string
	if err := s.Rows(func(cells []Cell) error {
		row := make([]string, len(cells))
		for i, c := range cells {
			row[i] = c.Text
		}
		out = append(out, row)
		return nil
	}); err != nil {
		t.Fatalf("reading %s: %v", s.Name, err)
	}
	return out
}

func utf16LE(s string) []byte {
	out := []byte{0xFF, 0xFE}
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// A CSV or TSV file is one sheet, named after the file, read as Excel,
// Google Sheets and LibreOffice write them.
func TestASpreadsheetFileIsReadAsOneSheet(t *testing.T) {
	cases := []struct {
		name, file string
		data       []byte
		want       [][]string
	}{
		{"a CSV with a BOM, a comma and a line break in quoted cells", "Accounts.csv",
			[]byte("\uFEFFName,Notes\nAda,\"likes, commas\"\nBo,\"two\nlines\"\n"),
			[][]string{{"Name", "Notes"}, {"Ada", "likes, commas"}, {"Bo", "two\nlines"}}},
		{"Excel's semicolons, where a comma is the decimal sign", "Prices.csv",
			[]byte("Item;Price\r\nTea;1,50\r\nCake;3,25\r\n"),
			[][]string{{"Item", "Price"}, {"Tea", "1,50"}, {"Cake", "3,25"}}},
		{"a TSV", "Export.tsv", []byte("a\tb\n1\t2\n"), [][]string{{"a", "b"}, {"1", "2"}}},
		{"pipes", "Pipes.csv", []byte("a|b|c\n1|2|3\n"), [][]string{{"a", "b", "c"}, {"1", "2", "3"}}},
		{"UTF-16 with a BOM", "Wide.csv", utf16LE("Name,City\r\nZoë,Köln\r\n"), [][]string{{"Name", "City"}, {"Zoë", "Köln"}}},
		{"Windows-1252, as older Excel writes", "Old.csv", []byte("Name,City\nCaf\xe9,Z\xfcrich\n"), [][]string{{"Name", "City"}, {"Café", "Zürich"}}},
		{"rows of different lengths", "Ragged.csv", []byte("a,b,c\n1,2\n3,4,5,6\n"), [][]string{{"a", "b", "c"}, {"1", "2"}, {"3", "4", "5", "6"}}},
		{"one column", "Names.csv", []byte("Name\nAda\nBo\n"), [][]string{{"Name"}, {"Ada"}, {"Bo"}}},
		{"a row ends at its last value, as a workbook's does", "Trailing.csv", []byte("a,b,\n1,2, ,\n,,\n"), [][]string{{"a", "b"}, {"1", "2"}, {}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sheets, err := ReadSheets(sheetOf(c.file, c.data))
			if err != nil || len(sheets) != 1 {
				t.Fatalf("%d sheets, %v", len(sheets), err)
			}
			if want := strings.TrimSuffix(c.file, c.file[strings.LastIndex(c.file, "."):]); sheets[0].Name != want {
				t.Errorf("the sheet is named %q, want %q", sheets[0].Name, want)
			}
			got := rowsOf(t, sheets[0])
			if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", c.want) {
				t.Errorf("rows %q, want %q", got, c.want)
			}
			if again := rowsOf(t, sheets[0]); fmt.Sprintf("%q", again) != fmt.Sprintf("%q", got) {
				t.Errorf("read again, the rows are %q", again)
			}
		})
	}
}

// said says whether err is a read error that says words to the person
// importing.
func said(err error, words string) bool {
	var r readError
	return errors.As(err, &r) && strings.Contains(r.msg, words)
}

// A sheet's row is read whole, out to the last column a spreadsheet has
// (how many of them a table takes is the tables importer's to say); a row
// past it, or one no sheet has (a quote that never closes over megabytes),
// is an error, never read into memory whole.
func TestASheetIsReadWithinItsLimits(t *testing.T) {
	wide := strings.Repeat("x,", 600) + "x\n"
	sheets, err := ReadSheets(sheetOf("Wide.csv", []byte(wide)))
	if err != nil {
		t.Fatal(err)
	}
	if rows := rowsOf(t, sheets[0]); len(rows) != 1 || len(rows[0]) != 601 {
		t.Errorf("a row of 601 cells reads as %d", len(at(rows, 0)))
	}
	tooWide := strings.Repeat("x,", maxSheetColumns) + "x\n"
	sheets, err = ReadSheets(sheetOf("TooWide.csv", []byte(tooWide)))
	if err != nil {
		t.Fatal(err)
	}
	if err := sheets[0].Rows(func([]Cell) error { return nil }); !said(err, "more columns than a spreadsheet can") {
		t.Errorf("a row of %d cells: %v", maxSheetColumns+1, err)
	}
	endless := append([]byte("a,\""), bytes.Repeat([]byte("y"), maxSheetRowBytes+1)...)
	sheets, err = ReadSheets(sheetOf("Endless.csv", endless))
	if err != nil {
		t.Fatal(err)
	}
	if err := sheets[0].Rows(func([]Cell) error { return nil }); err == nil {
		t.Error("a row past the limit read")
	}
}

// A sheet of more rows than a spreadsheet holds is refused alike, whatever
// its file (an OpenDocument sheet's repeated rows are tested with its other
// repeats), and a sheet of as many as it holds reads to its end.
func TestASheetPastTheRowsASpreadsheetHoldsIsRefused(t *testing.T) {
	var rows strings.Builder
	for range maxSheetRows + 1 {
		rows.WriteString(`<row><c><v>1</v></c></row>`)
	}
	for _, c := range []struct {
		in      Input
		refused bool
	}{
		{sheetOf("Full.csv", []byte(strings.Repeat("1\n", maxSheetRows))), false},
		{sheetOf("Long.csv", []byte(strings.Repeat("1\n", maxSheetRows+1))), true},
		{sheetOf("Long.xlsx", zipOf(t, excelParts(rows.String()))), true},
	} {
		sheets, err := ReadSheets(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in.Filename, err)
		}
		read := 0
		err = sheets[0].Rows(func([]Cell) error { read++; return nil })
		if c.refused != said(err, fmt.Sprintf("more rows than a spreadsheet can (%d)", maxSheetRows)) || (!c.refused && err != nil) {
			t.Errorf("%s: %d rows read, then %v", c.in.Filename, read, err)
		}
	}
}

// Other files aren't sheets ReadSheets reads.
func TestReadSheetsReadsOnlySheets(t *testing.T) {
	for _, name := range []string{"notes.md", "photo.png", "archive.zip"} {
		if _, err := ReadSheets(sheetOf(name, []byte("a,b\n"))); !errors.Is(err, ErrNotASheet) {
			t.Errorf("%s: %v, want ErrNotASheet", name, err)
		}
	}
}

// The web's import dialog offers exactly these files (lib/tables/sheetImport.test.ts "the files a spreadsheet import
// offers" pins the same five). On 26 Oct the dialog filtered them by the doc importers' extensions, which never name
// a sheet, and refused every sheet: a reader added or dropped here changes that test's list too.
func TestSheetReadersReadTheFilesTheWebOffers(t *testing.T) {
	got := make([]string, 0, len(sheetReaders))
	for ext := range sheetReaders {
		got = append(got, ext)
	}
	sort.Strings(got)
	want := []string{".csv", ".ods", ".tsv", ".xlsm", ".xlsx"}
	if !slices.Equal(got, want) {
		t.Fatalf("sheet readers %v, want %v (the web's SHEET_EXTENSIONS)", got, want)
	}
	for _, ext := range want {
		if !IsSheetFile("budget" + strings.ToUpper(ext)) {
			t.Errorf("IsSheetFile refuses %q", "budget"+strings.ToUpper(ext))
		}
	}
}
