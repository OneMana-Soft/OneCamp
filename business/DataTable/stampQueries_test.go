package business

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// What a row's own fields read (its number, when it was made and last
// changed, and by whom) isn't stored: a query filtering or grouping by one
// works it out first, and never narrows the rows it reads by looking for it
// in the stored cells, which would leave out every row.
func TestAQueryWorksOutWhatIsReadFromTheRow(t *testing.T) {
	name := fld("Name", model.FieldText)
	fields := []*model.Field{name}
	for kind, value := range map[string]string{model.FieldAutonumber: "17", model.FieldCreatedTime: "2026-10-17", model.FieldModifiedTime: "2026-10-17",
		model.FieldCreatedBy: "Maya", model.FieldModifiedBy: "Maya", model.FieldPeople: "Maya"} {
		f := fld("Stamp "+kind, kind)
		all := append(slices.Clone(fields), f)
		filters := []Filter{{Field: f.Name, Op: string(FilterEq), Value: value}}
		if terms := termsOf(t, all, filters...); len(terms) != 0 {
			t.Errorf("%s: pushed down to the stored cells as %v", kind, terms)
		}
		if reads := computedReads(all, filters); !slices.Contains(reads, f.Id.String()) {
			t.Errorf("%s: not worked out for the query, which reads %v", kind, reads)
		}
	}
	// A stored text cell is still pushed down.
	if terms := termsOf(t, fields, Filter{Field: "Name", Op: string(FilterEq), Value: "Acme"}); !slices.Equal(terms, []string{"Acme"}) {
		t.Errorf("a text filter pushed down as %v", terms)
	}
}

// Grouped by when rows were made, rows fall on the day each was made in the
// reader's zone, in order of days, and the result says it's dates (a chart
// draws a trend); a date filter compares days, a date's or a time's alike.
func TestRowsMadeGroupByTheDayInTheReadersZone(t *testing.T) {
	made := fld("Made", model.FieldCreatedTime)
	due := fld("Due", model.FieldDate)
	fields := []*model.Field{made, due}
	// When a row was made is the row's own, which a read also writes in.
	mk := func(at, day string) *model.Row {
		r := row(fmt.Sprintf(`{%q:%q,%q:%q}`, made.Id, at, due.Id, day))
		r.CreatedAt, _ = time.Parse(time.RFC3339, at)
		return r
	}
	rows := []*model.Row{
		mk("2026-10-17T20:00:00Z", "2026-10-30"), // 18 Oct, 01:30 in India
		mk("2026-10-17T09:00:00Z", "2026-10-02"),
		mk("2026-10-16T23:00:00Z", "2026-09-30"), // 17 Oct, 04:30 in India
	}
	india := time.FixedZone("IST", 19800)
	res, err := Aggregate(fields, rows, QuerySpec{GroupBy: "Made", Op: OpCount, at: reading{zone: india}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range res.Buckets {
		got = append(got, fmt.Sprintf("%s:%d", b.Label, b.Count))
	}
	if want := []string{"2026-10-17:2", "2026-10-18:1"}; !slices.Equal(got, want) {
		t.Errorf("by the day made in India %v, want %v", got, want)
	}
	if res.GroupByType != model.FieldDate {
		t.Errorf("the group's type %q, want date", res.GroupByType)
	}
	res, _ = Aggregate(fields, rows, QuerySpec{GroupBy: "Made", Op: OpCount})
	got = nil
	for _, b := range res.Buckets {
		got = append(got, fmt.Sprintf("%s:%d", b.Label, b.Count))
	}
	if want := []string{"2026-10-16:1", "2026-10-17:2"}; !slices.Equal(got, want) {
		t.Errorf("by the day made in UTC %v, want %v", got, want)
	}

	count := func(filters ...Filter) int {
		res, err := Aggregate(fields, rows, QuerySpec{Op: OpCount, Filters: filters, at: reading{zone: india}})
		if err != nil {
			t.Fatal(err)
		}
		return res.MatchedRows
	}
	if n := count(Filter{Field: "Made", Op: "eq", Value: "2026-10-17"}); n != 2 {
		t.Errorf("made on 17 Oct: %d rows, want 2", n)
	}
	if n := count(Filter{Field: "Made", Op: "gt", Value: "2026-10-17"}); n != 1 {
		t.Errorf("made after 17 Oct: %d rows, want 1", n)
	}
	// A date's filter compares days too: before, it read the date as a
	// number, and no row was ever after anything.
	if n := count(Filter{Field: "Due", Op: "gte", Value: "2026-10-02"}); n != 2 {
		t.Errorf("due from 2 Oct: %d rows, want 2", n)
	}
	if _, err := Aggregate(fields, rows, QuerySpec{Op: OpCount, Filters: []Filter{{Field: "Due", Op: "lt", Value: "soon"}}}); err == nil {
		t.Error("due before a word: no row, where it's refused now, saying why")
	}
	if n := count(Filter{Field: "Due", Op: "ne", Value: "2026-10-30"}); n != 2 {
		t.Errorf("due other than 30 Oct: %d rows, want 2", n)
	}
}

// A rollup can't add up who made or changed the linked rows (people, named
// for each reader), as it can't people; it can add up when they were made
// and their numbers, which a linked row's read works out. Neither names a
// table's rows, nor can a formula reading who made a row.
func TestWhatIsReadFromTheRowInRollupsAndNames(t *testing.T) {
	madeBy := fld("Made by", model.FieldCreatedBy)
	made := fld("Made", model.FieldCreatedTime)
	id := fld("ID", model.FieldAutonumber)
	whoFormula := &model.Field{Id: uuid.New(), Name: "Who", Type: model.FieldFormula, Config: fmt.Sprintf(`{"formula":"{#%s}"}`, madeBy.Id)}
	whenFormula := &model.Field{Id: uuid.New(), Name: "When", Type: model.FieldFormula, Config: fmt.Sprintf(`{"formula":"YEAR({#%s})"}`, made.Id)}
	fields := []*model.Field{madeBy, made, id, whoFormula, whenFormula}
	if why := rollupProblem("unique", madeBy, fields); why == "" {
		t.Error("a rollup adds up who made the linked rows")
	}
	if why := rollupProblem("latest", made, fields); why != "" {
		t.Errorf("a rollup of when the linked rows were made: %s", why)
	}
	if why := rollupProblem("max", id, fields); why != "" {
		t.Errorf("a rollup of the linked rows' numbers: %s", why)
	}
	if why := rollupProblem("unique", whoFormula, fields); why == "" {
		t.Error("a rollup adds up a formula reading who made the linked rows")
	}
	if why := rollupProblem("max", whenFormula, fields); why != "" {
		t.Errorf("a rollup of a formula reading when the linked rows were made: %s", why)
	}
	for _, f := range []*model.Field{madeBy, made, id, whoFormula} {
		if namesRows(f, fields) {
			t.Errorf("%s names the table's rows", f.Name)
		}
	}
	if !namesRows(whenFormula, fields) {
		t.Error("a formula reading when a row was made can't name rows")
	}
}

// Nobody writes what is read from the row: a write naming a row's number,
// when it was made or changed or by whom stores none of them, and a patch of
// those alone names no cell the row stores.
func TestNobodyWritesWhatIsReadFromTheRow(t *testing.T) {
	name := fld("Name", model.FieldText)
	fields := []*model.Field{name}
	values := map[string]interface{}{name.Id.String(): "Acme"}
	for _, kind := range []string{model.FieldAutonumber, model.FieldCreatedTime, model.FieldCreatedBy, model.FieldModifiedTime, model.FieldModifiedBy} {
		f := fld(kind, kind)
		fields = append(fields, f)
		values[f.Id.String()] = "forged"
	}
	stored, err := storedCells(fields, values, fieldTypes.Input{})
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf(`{%q:"Acme"}`, name.Id); stored != want {
		t.Errorf("stored %s, want %s", stored, want)
	}
}

// An id as it is shown ("DEAL-12") finds the row numbered 12, in any case;
// its number alone does too, and another prefix is refused, saying what an
// id here looks like.
func TestAnIDAsItIsShownFindsItsRow(t *testing.T) {
	id := &model.Field{Id: uuid.New(), Name: "ID", Type: model.FieldAutonumber, Config: `{"prefix":"DEAL"}`}
	fields := []*model.Field{id}
	numbered := func(n int64) *model.Row {
		r := row(fmt.Sprintf(`{%q:%d}`, id.Id, n))
		r.RowNo = n
		return r
	}
	rows := []*model.Row{numbered(12), numbered(13)}
	for value, want := range map[string]int{"DEAL-12": 1, "deal-12": 1, "12": 1} {
		res, err := Aggregate(fields, rows, QuerySpec{Op: OpCount, Filters: []Filter{{Field: "ID", Op: "eq", Value: value}}})
		if err != nil {
			t.Fatal(err)
		}
		if res.MatchedRows != want {
			t.Errorf("ID is %q: %d rows, want %d", value, res.MatchedRows, want)
		}
	}
	for _, value := range []string{"OPS-12", "DEAL-"} {
		if _, err := Aggregate(fields, rows, QuerySpec{Op: OpCount, Filters: []Filter{{Field: "ID", Op: "eq", Value: value}}}); err == nil || !strings.Contains(err.Error(), "DEAL-12") {
			t.Errorf("ID is %q: %v, want it refused, saying ids look like DEAL-12", value, err)
		}
	}
	res, _ := Aggregate(fields, rows, QuerySpec{Op: OpCount, Filters: []Filter{{Field: "ID", Op: "gt", Value: "DEAL-12"}}})
	if res.MatchedRows != 1 {
		t.Errorf("ID after DEAL-12: %d rows, want 1", res.MatchedRows)
	}
}
