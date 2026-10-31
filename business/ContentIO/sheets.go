package business

// Spreadsheet files as an import reads them (tables S1, design/
// tables-vs-airtable/PROGRAM.md §15): a CSV or TSV file is one sheet of text
// cells; an Excel workbook (xlsx.go) or an OpenDocument spreadsheet (ods.go)
// is its sheets of cells that say what they hold. Each sheet is read a row at
// a time from the stored file, so a big one never sits in memory whole. The
// tables importer (business/DataTable) guesses each column's kind from its
// cells and makes a table of them.

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// Sheet is one sheet of a spreadsheet file.
type Sheet struct {
	Name string
	// Hidden is a sheet the file keeps out of sight: a workbook's lists and
	// lookups rather than its data.
	Hidden bool
	// Rows reads the sheet from its first row, handing each row's cells to
	// row in order, until row returns an error, which Rows returns. Each call
	// reads the file again, one read at a time, as an import makes them. A
	// row's cells end at its last value, and a workbook's rows with nothing
	// in them are left out. The cells are the reader's: row reads them, and
	// keeps and changes none of them (a row an OpenDocument file repeats is
	// handed the same cells again).
	Rows func(row func(cells []Cell) error) error
}

// Cell is one cell as the file holds it. A CSV's cells are all text; a
// workbook's say what they hold, and Text is that written out: as the file
// shows it where it says (an OpenDocument cell's text), else plainly
// (1200.5, 12.5%, 2026-10-23 14:30, 1:30).
type Cell struct {
	Text string
	Kind CellKind
	// Number is a number's value, a percent's as it shows (12.5 for 12.5%),
	// or a duration's seconds.
	Number float64
	// Percent and Currency are how a number shows: Currency is "USD" or
	// "INR" for the signs a table's number shows, "other" for another sign,
	// and "" for none.
	Percent  bool
	Currency string
	// Bool is a checkbox's value.
	Bool bool
	// Time is a date's day and time of day as written, in no zone (UTC's
	// terms).
	Time time.Time
	// Formula says the value is the one the cell's formula last worked out.
	Formula bool
	// Choices, Link and People are what a spreadsheet says of a cell past
	// its value, as a Google Sheet does: the values its dropdown offers, in
	// order; the address it links to; and the emails of the people its chips
	// name. A column of them is a select, a url and people.
	Choices []string
	Link    string
	People  []string
}

// CellKind is what a cell holds.
type CellKind uint8

const (
	// CellText is text: every cell of a CSV, and a workbook's text.
	CellText CellKind = iota
	CellNumber
	CellBool
	// CellDate is a day, with its time of day (midnight for none).
	CellDate
	// CellDuration is a length of time, or a time of day (since midnight).
	CellDuration
	// CellError is a formula that couldn't be worked out (#N/A, #DIV/0!):
	// it holds no value.
	CellError
)

// empty is a cell with nothing in it.
func (c Cell) empty() bool { return c.Kind == CellText && c.Text == "" }

// What a sheet is read for. No sheet holds more columns than
// maxSheetColumns or more rows than maxSheetRows, the most Excel and
// LibreOffice hold: a CSV row of more columns, and a sheet of more rows, are
// refused, and a workbook's cells past its last column, which no
// spreadsheet writes, are left out. A row is read out to its last value, so
// a sheet's cells are counted as its rows are: one of more than
// maxSheetCells, twice what Google Sheets holds in a whole spreadsheet, is
// refused, whatever its rows hold (a value at the last column of each of a
// million rows would be 17 billion cells). A row, or a workbook's cell, that
// runs past maxSheetRowBytes is the file's error, as no sheet's is that
// long.
const (
	maxSheetColumns  = 1 << 14
	maxSheetRowBytes = 4 << 20
	maxSheetRows     = 1 << 20
	maxSheetCells    = 20_000_000
)

// delimiters are what a CSV's cells may be separated by, the likeliest first:
// Excel writes a semicolon where a comma is the decimal sign.
var delimiters = []rune{',', ';', '\t', '|'}

// ErrNotASheet is a file that isn't one ReadSheets reads.
var ErrNotASheet = errors.New("content io: not a spreadsheet file")

// sheetReaders read the files ReadSheets reads, by their extension. A
// macro-enabled workbook (.xlsm) is read as any other: its macros are never
// read or run.
var sheetReaders = map[string]func(Input) ([]Sheet, error){
	".csv":  func(in Input) ([]Sheet, error) { return delimitedSheet(in, 0) },
	".tsv":  func(in Input) ([]Sheet, error) { return delimitedSheet(in, '\t') },
	".xlsx": readXLSX,
	".xlsm": readXLSX,
	".ods":  readODS,
}

// IsSheetFile says whether ReadSheets reads a file of this name.
func IsSheetFile(filename string) bool {
	_, ok := sheetReaders[strings.ToLower(path.Ext(filename))]
	return ok
}

// ReadSheets reads in as its sheets: a .csv or .tsv file is one sheet named
// after the file; an Excel workbook (.xlsx, .xlsm) or an OpenDocument
// spreadsheet (.ods) is its sheets, in its order. Every read of a
// workbook's sheets unpacks within the one budget it was opened with.
func ReadSheets(in Input) ([]Sheet, error) {
	read, ok := sheetReaders[strings.ToLower(path.Ext(in.Filename))]
	if !ok {
		return nil, ErrNotASheet
	}
	sheets, err := read(in)
	for i := range sheets {
		sheets[i].Rows = rowsWithin(sheets[i].Name, sheets[i].Rows)
	}
	return sheets, err
}

// rowsWithin is a sheet's rows, refused past maxSheetRows rows or
// maxSheetCells cells whatever the file: the one place every format's rows
// and cells are counted.
func rowsWithin(name string, rows func(func([]Cell) error) error) func(func([]Cell) error) error {
	return func(row func([]Cell) error) error {
		n, cells := 0, 0
		return rows(func(c []Cell) error {
			n, cells = n+1, cells+len(c)
			switch {
			case n > maxSheetRows:
				return readError{fmt.Sprintf("The sheet %q holds more rows than a spreadsheet can (%d).", name, maxSheetRows)}
			case cells > maxSheetCells:
				return readError{fmt.Sprintf("The sheet %q holds more cells than an import reads (%d).", name, maxSheetCells)}
			}
			return row(c)
		})
	}
}

// delimitedSheet is a CSV or TSV file as one sheet, its cells separated by
// delim, or by what its first lines say (a comma, a semicolon, a tab or a
// pipe) for 0. The text may be UTF-8 with or without a BOM, UTF-16 with a
// BOM, or else Windows-1252, as older Excel writes it.
func delimitedSheet(in Input, delim rune) ([]Sheet, error) {
	if delim == 0 {
		head, err := textHead(in)
		if err != nil {
			return nil, err
		}
		delim = sniffDelimiter(head)
	}
	name := strings.TrimSuffix(path.Base(in.Filename), path.Ext(in.Filename))
	return []Sheet{{Name: name, Rows: func(row func([]Cell) error) error { return readDelimited(in, delim, row) }}}, nil
}

// sheetHeadBytes is how much of a file its delimiter is sniffed from.
const sheetHeadBytes = 64 << 10

// textHead is the first sheetHeadBytes of in as text, cut at its last line.
func textHead(in Input) ([]byte, error) {
	r, err := sheetText(in)
	if err != nil {
		return nil, err
	}
	head, err := io.ReadAll(io.LimitReader(r, sheetHeadBytes))
	if err != nil {
		return nil, fmt.Errorf("the file could not be read: %w", err)
	}
	if len(head) == sheetHeadBytes {
		if i := bytes.LastIndexByte(head, '\n'); i > 0 {
			head = head[:i+1]
		}
	}
	return head, nil
}

// sniffDelimiter is the delimiter that splits head's first rows into the
// same number of cells, the most of them, where the rows have more than one;
// a comma when none does. Pure.
func sniffDelimiter(head []byte) rune {
	best, bestCells := ',', 1
	for _, d := range delimiters {
		r := csv.NewReader(bytes.NewReader(head))
		r.Comma, r.LazyQuotes, r.FieldsPerRecord = d, true, -1
		cells, rows := 0, 0
		for rows < 20 {
			rec, err := r.Read()
			if err != nil {
				break
			}
			if rows == 0 {
				cells = len(rec)
			} else if len(rec) != cells {
				cells = 0
				break
			}
			rows++
		}
		if cells > bestCells {
			best, bestCells = d, cells
		}
	}
	return best
}

// readDelimited reads in's rows, delim between cells, handing each to row
// up to its last cell with a value. A row may have fewer or more cells than
// another; a cell that isn't valid text keeps what is.
func readDelimited(in Input, delim rune, row func([]Cell) error) error {
	text, err := sheetText(in)
	if err != nil {
		return err
	}
	r := csv.NewReader(bufio.NewReaderSize(&runCap{r: text, sep: '\n', what: "a row"}, 64<<10))
	r.Comma, r.LazyQuotes, r.FieldsPerRecord, r.ReuseRecord = delim, true, -1, true
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("the file could not be read as a sheet: %w", err)
		}
		n := len(rec)
		for n > 0 && strings.TrimSpace(rec[n-1]) == "" {
			n--
		}
		if n > maxSheetColumns {
			return readError{fmt.Sprintf("A row has more columns than a spreadsheet can (%d).", maxSheetColumns)}
		}
		cells := make([]Cell, n)
		for i := range cells {
			cells[i] = Cell{Text: strings.ToValidUTF8(rec[i], "�")}
		}
		if err := row(cells); err != nil {
			return err
		}
	}
}

// sheetText is in's text as UTF-8, read in what its head says it was saved
// in (textEncoding, as DecodeText reads a file), its byte-order mark
// dropped.
func sheetText(in Input) (io.Reader, error) {
	br := bufio.NewReaderSize(in.Reader(), sheetHeadBytes)
	head, err := br.Peek(sheetHeadBytes)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, fmt.Errorf("the file could not be read: %w", err)
	}
	if len(head) == sheetHeadBytes {
		head = trimPartialRune(head) // the text goes on past it
	}
	return transform.NewReader(br, unicode.BOMOverride(textEncoding(head).NewDecoder())), nil
}

// trimPartialRune is b without a last rune cut short at its end. Pure.
func trimPartialRune(b []byte) []byte {
	for i := 1; i < utf8.UTFMax && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if !utf8.FullRune(b[len(b)-i:]) {
				return b[:len(b)-i]
			}
			break
		}
	}
	return b
}

// runCap is a reader that fails once more than maxSheetRowBytes pass without
// a sep: a CSV's row ('\n'), or a workbook's XML token ('<'), so a file that
// isn't a sheet (a quote that never closes, text without markup) is an error
// rather than read into memory whole.
type runCap struct {
	r    io.Reader
	sep  byte
	what string
	run  int
}

func (c *runCap) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	for _, b := range p[:n] {
		if b == c.sep {
			c.run = 0
			continue
		}
		c.run++
		if c.run > maxSheetRowBytes {
			return 0, runError{c.what}
		}
	}
	return n, err
}

// runError is a run past maxSheetRowBytes: "a row" or "a cell" runs past it.
type runError struct{ what string }

func (e runError) Error() string {
	return fmt.Sprintf("%s runs past %d MB", e.what, maxSheetRowBytes>>20)
}
