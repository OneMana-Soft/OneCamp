package helpers

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// A CSV the server hands people opens in a spreadsheet, so it starts here,
// and one people hand the server (an import) is read here.

// csvBOM is UTF-8's byte order mark. Excel reads a CSV without it in the
// computer's own code page, so "Zoë" opened as "ZoÃ«"; Google Sheets, Numbers
// and LibreOffice read the file the same either way.
const csvBOM = "\uFEFF"

// NewCSVWriter starts a CSV for a spreadsheet: the byte order mark, then a
// writer.
func NewCSVWriter(w io.Writer) (*csv.Writer, error) {
	if _, err := io.WriteString(w, csvBOM); err != nil {
		return nil, err
	}
	return csv.NewWriter(w), nil
}

// CSVCell keeps a cell a person typed from being read as a formula: a
// spreadsheet runs a cell that starts with = + - or @, a tab or a carriage
// return when the file opens. An apostrophe first makes it text.
func CSVCell(s string) string {
	if s != "" && formulaStart(s[0]) {
		return "'" + s
	}
	return s
}

// CSVValue is a cell as it was typed: CSVCell's apostrophe taken off. An
// apostrophe before anything else is the person's own, and stays.
func CSVValue(s string) string {
	if len(s) > 1 && s[0] == '\'' && formulaStart(s[1]) {
		return s[1:]
	}
	return s
}

func formulaStart(b byte) bool { return strings.IndexByte("=+-@\t\r", b) >= 0 }

// NewCSVReader reads a CSV as spreadsheets save one: past a byte order mark,
// with rows of any length, and a stray quote inside a cell taken as it is.
// A file that can't be read says so on its first Read.
func NewCSVReader(r io.Reader) *csv.Reader {
	br := bufio.NewReader(r)
	if head, _ := br.Peek(len(csvBOM)); string(head) == csvBOM {
		_, _ = br.Discard(len(csvBOM))
	}
	cr := csv.NewReader(br)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	return cr
}

// Hours is seconds as hours to two places, the way an invoice reads them.
func Hours(secs int64) string {
	return strconv.FormatFloat(float64(secs)/3600, 'f', 2, 64)
}

// Money is minor units as a decimal amount: 123456 is "1234.56".
func Money(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}
