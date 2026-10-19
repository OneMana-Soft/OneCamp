package models

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// A write's changes are the cells it changed, by field, before and after: a
// cell made has no before, one cleared away no after, and the AI-kept mark
// isn't a cell.
func TestTheCellsAWriteChanged(t *testing.T) {
	row := uuid.New()
	got := cellChanges(row, `{"a":1,"b":"x","c":[1,2],"_ai_kept":["a"]}`, `{"a":1.0,"b":"y","d":true,"_ai_kept":["a","b"]}`)
	want := []Change{
		{Row: row, Field: "b", Before: json.RawMessage(`"x"`), After: json.RawMessage(`"y"`)},
		{Row: row, Field: "c", Before: json.RawMessage(`[1,2]`)},
		{Row: row, Field: "d", After: json.RawMessage(`true`)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the changes are %+v", got)
	}
}

// A change set names each field it changed once, links by their field, and
// none for whole rows or moves; a change set of new rows names an empty list,
// which goes to Postgres as '{}': as NULL (a nil list) the NOT NULL column
// refused every row made.
func TestAChangeSetsRowsAndFields(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	rows, fields := rowsAndFields([]Change{
		{Row: a, Field: "f1"}, {Row: a, Field: "f2"}, {Row: b, Field: "f1"},
		{Row: b, Field: LinksPrefix + "f3"}, {Row: a, Field: FieldPosition}, {Row: b, Field: WholeRow},
	})
	if rows != 2 || !reflect.DeepEqual(fields, []string{"f1", "f2", "f3"}) {
		t.Errorf("%d rows, fields %v", rows, fields)
	}
	rows, fields = rowsAndFields([]Change{{Row: a, Field: WholeRow}, {Row: b, Field: WholeRow}})
	if rows != 2 || fields == nil || len(fields) != 0 {
		t.Fatalf("new rows: %d rows, fields %#v", rows, fields)
	}
	if v, err := pq.Array(fields).Value(); err != nil || v != "{}" {
		t.Errorf("new rows' fields go to Postgres as %#v (%v)", v, err)
	}
}

func TestTheSameJSON(t *testing.T) {
	cases := []struct {
		a, b string
		same bool
	}{
		{`1`, `1.0`, true},
		{`{"x":1,"y":2}`, `{"y":2,"x":1}`, true},
		{`"Open"`, `"open"`, false},
		{`null`, ``, false},
		{``, ``, true},
	}
	for _, c := range cases {
		var a, b json.RawMessage
		if c.a != "" {
			a = json.RawMessage(c.a)
		}
		if c.b != "" {
			b = json.RawMessage(c.b)
		}
		if sameJSON(a, b) != c.same {
			t.Errorf("%q and %q: same %v", c.a, c.b, !c.same)
		}
	}
}

// What a whole row's change did to its row; nothing for any other change.
func TestWhatAWholeRowsChangeDid(t *testing.T) {
	cases := []struct {
		c    Change
		want string
	}{
		{Change{Field: WholeRow, After: json.RawMessage(`{"values":{},"version":1}`)}, RowCreated},
		{Change{Field: WholeRow, Before: json.RawMessage(`{"version":2}`), After: json.RawMessage(`{"deleted":true,"version":3}`)}, RowDeleted},
		{Change{Field: WholeRow, Before: json.RawMessage(`{"deleted":true,"version":3}`), After: json.RawMessage(`{"version":4}`)}, RowRestored},
		{Change{Field: "status", After: json.RawMessage(`"Won"`)}, ""},
		{Change{Field: FieldPosition, After: json.RawMessage(`"a1"`)}, ""},
	}
	for _, c := range cases {
		if got := c.c.RowEvent(); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.c.Field, c.c.After, got, c.want)
		}
	}
}

// Only the tables' own insert-mostly tables are vacuumed here: a vacuum
// names its table in the statement, which can't be a parameter.
func TestOnlyTheTablesOwnAreVacuumed(t *testing.T) {
	for _, table := range InsertMostly {
		if !insertMostly(table) {
			t.Errorf("%s isn't vacuumed", table)
		}
	}
	for _, table := range []string{"users", "data_table_links; DROP TABLE users", ""} {
		if insertMostly(table) {
			t.Errorf("%q would be vacuumed", table)
		}
	}
}

func TestARefusedWriteSaysWhy(t *testing.T) {
	one := RowErrors{{Row: uuid.New(), Err: ErrVersion}}
	if one.Error() != ErrVersion.Error() {
		t.Errorf("one row: %q", one.Error())
	}
	two := append(one, RowError{Row: uuid.New(), Err: ErrRowTooLarge})
	if two.Error() != "2 rows couldn't be written" {
		t.Errorf("two rows: %q", two.Error())
	}
}
