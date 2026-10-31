package csv

import (
	"errors"
	"strings"
	"testing"
)

func mustRead(t *testing.T, in string) *table {
	t.Helper()
	tb, err := readTable(strings.NewReader(in), maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

func col(tb *table, name string) *column {
	for _, c := range tb.cols {
		if c.name == name {
			return c
		}
	}
	return nil
}

// A header is read by our export's name or a rival's, a header written twice
// is one column, an address column stands beside its people, and every other
// column is a field.
func TestColumnsAreReadByOurNamesAndTheRivals(t *testing.T) {
	tb := mustRead(t, "\xef\xbb\xbfIssue key,Summary,Section/Column,Assignee,Assignee Email,Labels,Labels,Budget (USD),\n"+
		"ENG-1,Ship it,Doing,Ana,ana@acme.test,api,web,10,\n")
	want := map[string]property{"Issue key": propKey, "Summary": propTitle, "Section/Column": propStatus, "Assignee": propAssignee, "Labels": propTags, "Budget (USD)": "", "Column 9": ""}
	for name, p := range want {
		c := col(tb, name)
		if c == nil || c.prop != p {
			t.Errorf("%q: got %+v, want property %q", name, c, p)
		}
	}
	if got := tb.rows[0].all(col(tb, "Labels")); strings.Join(got, "|") != "api|web" {
		t.Errorf("a header written twice is one column: got %q", got)
	}
	if a := col(tb, "Assignee"); a.email != col(tb, "Assignee Email") || col(tb, "Assignee Email").emailOf != a {
		t.Error("Assignee Email stands beside Assignee")
	}
	if len(tb.id) != 12 {
		t.Errorf("the file's id: %q", tb.id)
	}
}

// What our export kept from being a formula reads back as typed; a blank row
// is no task; a quoted cell keeps its lines; the row's line is where it starts.
func TestRowsReadAsTyped(t *testing.T) {
	tb := mustRead(t, "Title,Description\n'=SUM(A1),\"two\nlines\"\n,\nSecond,x\n")
	if len(tb.rows) != 2 {
		t.Fatalf("got %d rows", len(tb.rows))
	}
	if got := tb.rows[0].cell(col(tb, "Title")); got != "=SUM(A1)" {
		t.Errorf("got %q", got)
	}
	if got := tb.rows[0].cell(col(tb, "Description")); got != "two\nlines" {
		t.Errorf("got %q", got)
	}
	if tb.rows[0].line != 2 || tb.rows[1].line != 5 {
		t.Errorf("lines: %d, %d", tb.rows[0].line, tb.rows[1].line)
	}
}

// Two files are two ids; one file read twice is one.
func TestTheFilesIDIsItsContent(t *testing.T) {
	a, b, c := mustRead(t, "Title\nA\n"), mustRead(t, "Title\nA\n"), mustRead(t, "Title\nB\n")
	if a.id != b.id || a.id == c.id {
		t.Errorf("ids: %s %s %s", a.id, b.id, c.id)
	}
}

// What an import can't bring is refused with what to do about it.
func TestAFileItCantBringIsRefused(t *testing.T) {
	if _, err := readTable(strings.NewReader(""), maxBytes); !errors.Is(err, errEmpty) {
		t.Errorf("empty: %v", err)
	}
	if _, err := readTable(strings.NewReader("Title\n"+strings.Repeat("a task\n", 20)), 64); !errors.Is(err, errTooBig) {
		t.Errorf("too big: %v", err)
	}
	if _, err := readTable(strings.NewReader("Title\n"+strings.Repeat("t\n", MaxRows+1)), maxBytes); !errors.Is(err, errTooMany) {
		t.Errorf("too many: %v", err)
	}
	if tb, err := readTable(strings.NewReader("Title\n"+strings.Repeat("t\n", MaxRows)), maxBytes); err != nil || len(tb.rows) != MaxRows {
		t.Errorf("the most it brings: %v", err)
	}
	if err := check(mustRead(t, "Title,Status\n")); !errors.Is(err, errNoRows) {
		t.Errorf("no rows: %v", err)
	}
	if err := check(mustRead(t, "Status,Assignee\nDone,Ana\n")); !errors.Is(err, errNoTitle) {
		t.Errorf("no title: %v", err)
	}
}
