package helpers

import (
	"strings"
	"testing"
)

// A cell that starts with what a spreadsheet reads as a formula is kept as
// text; anything else is written as it is.
func TestCSVCellKeepsFormulasText(t *testing.T) {
	for _, s := range []string{"=HYPERLINK(\"x\")", "+1", "-2+3", "@SUM(A1)", "\t=1", "\r=1"} {
		if got := CSVCell(s); got != "'"+s {
			t.Errorf("%q: got %q", s, got)
		}
	}
	for _, s := range []string{"", "Launch", "1-2", " =1", "Zoë"} {
		if got := CSVCell(s); got != s {
			t.Errorf("%q is written as it is: got %q", s, got)
		}
	}
}

// A CSV starts with UTF-8's byte order mark, so Excel reads "Zoë" as typed.
func TestNewCSVWriterStartsWithTheByteOrderMark(t *testing.T) {
	var b strings.Builder
	cw, err := NewCSVWriter(&b)
	if err != nil {
		t.Fatal(err)
	}
	if err := cw.Write([]string{"Person", "Zoë"}); err != nil {
		t.Fatal(err)
	}
	cw.Flush()
	if got := b.String(); got != "\xef\xbb\xbfPerson,Zoë\n" {
		t.Errorf("got %q", got)
	}
}

func TestHoursAndMoney(t *testing.T) {
	if Hours(5400) != "1.50" || Hours(0) != "0.00" {
		t.Errorf("hours: %s %s", Hours(5400), Hours(0))
	}
	if Money(123456) != "1234.56" || Money(5) != "0.05" || Money(-250) != "-2.50" {
		t.Errorf("money: %s %s %s", Money(123456), Money(5), Money(-250))
	}
}

// What CSVCell kept from being a formula reads back as it was typed; an
// apostrophe before anything else is the person's own.
func TestCSVValueReadsBackWhatCSVCellWrote(t *testing.T) {
	for _, s := range []string{"=HYPERLINK(\"x\")", "+1", "-2+3", "@SUM(A1)", "\t=1", "\r=1", "", "Launch", "1-2"} {
		if got := CSVValue(CSVCell(s)); got != s {
			t.Errorf("%q: got %q", s, got)
		}
	}
	for _, s := range []string{"'tis the season", "'", "''=1"} {
		if got := CSVValue(s); got != s {
			t.Errorf("%q is the person's own: got %q", s, got)
		}
	}
}

// A file a spreadsheet saved reads as its rows: the byte order mark gone,
// a quoted cell's line breaks and quotes kept, a stray quote taken as it is,
// and a short row read as short.
func TestNewCSVReaderReadsASpreadsheetsFile(t *testing.T) {
	in := "\xef\xbb\xbfTitle,Notes\nA,\"two\nlines\"\nB,\"say \"\"hi\"\"\"\nC,a \"quoted\" word\nD\n"
	r := NewCSVReader(strings.NewReader(in))
	got, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"Title", "Notes"}, {"A", "two\nlines"}, {"B", `say "hi"`}, {"C", `a "quoted" word`}, {"D"}}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if strings.Join(got[i], "|") != strings.Join(want[i], "|") {
			t.Errorf("row %d: got %q, want %q", i, got[i], want[i])
		}
	}
	if rows, err := NewCSVReader(strings.NewReader("")).ReadAll(); err != nil || len(rows) != 0 {
		t.Errorf("an empty file has no rows: %q %v", rows, err)
	}
}
