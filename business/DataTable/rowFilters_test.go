package business

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	formula "github.com/akashc777/OneCamp/business/Formula"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// A table of deals as a read gives it: a name, an amount, a due day, a won
// tick, a stage, owners named for the reader, and an id ("DEAL-12") from
// the row's number. The third row is blank; the fourth's amount was written
// as text.
func filterTable(t *testing.T) ([]*model.Field, []*model.Row, map[string]string) {
	t.Helper()
	name := fld("Name", model.FieldText)
	amount := fld("Amount", model.FieldNumber)
	due := fld("Due", model.FieldDate)
	won := fld("Won", model.FieldCheckbox)
	stage := fld("Stage", model.FieldSelect)
	stage.Config = `{"options":[{"id":"o_won","label":"Won"},{"id":"o_lost","label":"Lost"}]}`
	owner := fld("Owner", model.FieldPeople)
	id := fld("ID", model.FieldAutonumber)
	id.Config = `{"prefix":"DEAL"}`
	fields := []*model.Field{name, amount, due, won, stage, owner, id}
	people := map[string]string{"Ada": uuid.NewString(), "Bob": uuid.NewString()}
	named := func(who ...string) []interface{} {
		out := []interface{}{}
		for _, w := range who {
			out = append(out, map[string]interface{}{"id": people[w], "type": "user", "label": w})
		}
		return out
	}
	cells := []map[*model.Field]interface{}{
		{name: "Booth", amount: 1000, due: "2026-10-12", won: true, stage: "Won", owner: named("Ada")},
		{name: "Banners", amount: 250, due: "2026-11-02", won: false, stage: "Lost", owner: named("Ada", "Bob")},
		{owner: named()},
		{name: "Cables", amount: "1,200", due: "2026-10-30", stage: "Won", owner: named("Bob")},
	}
	var rows []*model.Row
	for i, c := range cells {
		no := int64(12 + i)
		values := map[string]interface{}{id.Id.String(): float64(no)}
		for f, v := range c {
			values[f.Id.String()] = v
		}
		b, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, &model.Row{Id: uuid.New(), Values: string(b), RowNo: no})
	}
	return fields, rows, people
}

// matchedRows is the rows (by index) a count's filters keep, grouped by id.
func matchedRows(t *testing.T, fields []*model.Field, rows []*model.Row, filters ...Filter) string {
	t.Helper()
	res, err := Aggregate(fields, rows, QuerySpec{Op: OpCount, GroupBy: "ID", Filters: filters})
	if err != nil {
		t.Fatalf("%v: %v", filters, err)
	}
	var got []int
	for _, b := range res.Buckets {
		n, _ := strconv.Atoi(b.Label)
		got = append(got, n-12)
	}
	slices.Sort(got)
	out := make([]string, len(got))
	for i, n := range got {
		out[i] = strconv.Itoa(n)
	}
	return strings.Join(out, ",")
}

// The AI's filters read a table as the grid's views do (tables 22): numbers
// as numbers, typed as people write them, a cell written as text being
// none; dates by day, month or year where the reader is; a tick by yes or
// no; a choice by its label or its option; people by name or id.
func TestAQuerysFiltersMeanWhatTheGridsDo(t *testing.T) {
	fields, rows, people := filterTable(t)
	for _, c := range []struct {
		f    Filter
		want string
	}{
		{Filter{Field: "Amount", Op: "eq", Value: "1,000"}, "0"},
		{Filter{Field: "Amount", Op: "gte", Value: "$500"}, "0"},
		{Filter{Field: "Amount", Op: "ne", Value: "250"}, "0,2,3"},
		{Filter{Field: "Due", Op: "eq", Value: "2026-10"}, "0,3"},
		{Filter{Field: "Due", Op: "gt", Value: "2026-10-30"}, "1"},
		{Filter{Field: "Due", Op: "lte", Value: "2026-10-30"}, "0,3"},
		{Filter{Field: "Due", Op: "ne", Value: "2026-10-30"}, "0,1,2"},
		{Filter{Field: "Won", Op: "eq", Value: "yes"}, "0"},
		{Filter{Field: "Won", Op: "eq", Value: "false"}, "1,2,3"},
		{Filter{Field: "Won", Op: "ne", Value: "true"}, "1,2,3"},
		{Filter{Field: "Won", Op: "empty"}, "1,2,3"},
		{Filter{Field: "Stage", Op: "eq", Value: "won"}, "0,3"},
		{Filter{Field: "Stage", Op: "eq", Value: "o_lost"}, "1"},
		{Filter{Field: "Stage", Op: "ne", Value: "Won"}, "1,2"},
		{Filter{Field: "Owner", Op: "eq", Value: "ada"}, "0,1"},
		{Filter{Field: "Owner", Op: "eq", Value: people["Bob"]}, "1,3"},
		{Filter{Field: "Owner", Op: "contains", Value: "O"}, "1,3"},
		{Filter{Field: "Owner", Op: "ne", Value: "Ada"}, "2,3"},
		{Filter{Field: "Owner", Op: "empty"}, "2"},
		{Filter{Field: "ID", Op: "eq", Value: "deal-13"}, "1"},
		{Filter{Field: "ID", Op: "gt", Value: "DEAL-13"}, "2,3"},
		{Filter{Field: "Name", Op: "eq", Value: ""}, "2"},
		{Filter{Field: "Name", Op: "contains", Value: "AN"}, "1"},
		// What nothing knows is left out, as it always was.
		{Filter{Field: "Nothing", Op: "eq", Value: "x"}, "0,1,2,3"},
		{Filter{Field: "Name", Op: "resembles", Value: "x"}, "0,1,2,3"},
	} {
		if got := matchedRows(t, fields, rows, c.f); got != c.want {
			t.Errorf("%s %s %q: rows %s, want %s", c.f.Field, c.f.Op, c.f.Value, got, c.want)
		}
	}
}

// A filter that can't compare its field is refused, and says why, where it
// read as no row before: the AI hears what to ask instead.
func TestAQueryRefusesWhatItCantCompare(t *testing.T) {
	fields, rows, _ := filterTable(t)
	for _, c := range []struct {
		f    Filter
		says string
	}{
		{Filter{Field: "Amount", Op: "contains", Value: "1"}, `"Amount" compares numbers`},
		{Filter{Field: "Amount", Op: "gt", Value: "lots"}, `"lots" isn't one`},
		{Filter{Field: "Amount", Op: "gte", Value: "1,5"}, `"1,5" isn't one`},
		{Filter{Field: "Due", Op: "contains", Value: "2026"}, `"Due" compares dates`},
		{Filter{Field: "Due", Op: "lt", Value: "soon"}, `"soon" isn't one`},
		{Filter{Field: "Won", Op: "gt", Value: "0"}, `"Won" compares ticks`},
		{Filter{Field: "Won", Op: "eq", Value: "maybe"}, `"maybe" isn't one`},
		{Filter{Field: "Name", Op: "gte", Value: "b"}, `"Name" compares text`},
		{Filter{Field: "Stage", Op: "lt", Value: "Won"}, `"Stage" compares text`},
		{Filter{Field: "Owner", Op: "lt", Value: "Ada"}, `"Owner" compares people or links`},
		{Filter{Field: "ID", Op: "eq", Value: "OPS-12"}, `ids such as DEAL-12`},
	} {
		_, err := Aggregate(fields, rows, QuerySpec{Op: OpCount, Filters: []Filter{c.f}})
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s %s %q: %v, want it refused saying %s", c.f.Field, c.f.Op, c.f.Value, err, c.says)
		}
		if _, err := RunPlan(fields, rows, QueryPlan{Metrics: []PlanMetric{{Op: OpCount}}, Filters: []Filter{c.f}}); err == nil {
			t.Errorf("%s %s %q: a plan ran it", c.f.Field, c.f.Op, c.f.Value)
		}
	}
}

// A rollup's conditions are the grid's filters too: one that can't compare
// its field in the table it adds up isn't saved.
func TestARollupsConditionThatCantCompareIsntSaved(t *testing.T) {
	fields, _, _ := filterTable(t)
	for _, w := range []map[string]interface{}{
		{"field": "Amount", "op": "contains", "value": "1"},
		{"field": "Due", "op": "gt", "value": "next week"},
		{"field": "Won", "op": "eq", "value": "maybe"},
	} {
		if _, err := rollupWhere([]interface{}{w}, fields); err == nil {
			t.Errorf("the condition %v was saved", w)
		}
	}
	if where, err := rollupWhere([]interface{}{map[string]interface{}{"field": "Amount", "op": "gte", "value": "1,000"}}, fields); err != nil || len(where) != 1 {
		t.Errorf("a condition the grid reads: %v, %v", where, err)
	}
}

// @me is who asks, and a formula or a rollup compares as what it gives: a
// tick by yes or no, a number as people write one, a date by day.
func TestAQuerysFiltersReadForWhoAsksAndWhatFormulasGive(t *testing.T) {
	fields, rows, people := filterTable(t)
	big := &model.Field{Id: uuid.New(), Name: "Big", Type: model.FieldFormula, Config: `{"formula":"{Amount} > 500"}`}
	taxed := &model.Field{Id: uuid.New(), Name: "Taxed", Type: model.FieldFormula, Config: `{"formula":"{Amount} * 1.18"}`}
	fields = append(fields, big, taxed)
	// The cells as a read works them out.
	for i, r := range rows {
		var values map[string]interface{}
		_ = json.Unmarshal([]byte(r.Values), &values)
		values[big.Id.String()] = i == 0
		values[taxed.Id.String()] = []interface{}{1180.0, 295.0, nil, nil}[i]
		b, _ := json.Marshal(values)
		r.Values = string(b)
	}
	at := reading{me: uuid.MustParse(people["Bob"]), kinds: map[string]string{big.Id.String(): "checkbox", taxed.Id.String(): "number"}}
	for _, c := range []struct {
		f    Filter
		want int
	}{
		{Filter{Field: "Owner", Op: "eq", Value: "@me"}, 2},
		{Filter{Field: "Owner", Op: "ne", Value: "@me"}, 2},
		{Filter{Field: "Big", Op: "eq", Value: "yes"}, 1},
		{Filter{Field: "Taxed", Op: "eq", Value: "1,180"}, 1},
		{Filter{Field: "Taxed", Op: "lt", Value: "300"}, 1},
	} {
		res, err := Aggregate(fields, rows, QuerySpec{Op: OpCount, Filters: []Filter{c.f}, at: at})
		if err != nil || res.MatchedRows != c.want {
			t.Errorf("%s %s %q: %v (%v), want %d rows", c.f.Field, c.f.Op, c.f.Value, res, err, c.want)
		}
	}
	if _, err := Aggregate(fields, rows, QuerySpec{Op: OpCount, Filters: []Filter{{Field: "Taxed", Op: "contains", Value: "1"}}, at: at}); err == nil {
		t.Error("contains on a formula giving a number was read")
	}
}

// A rollup's conditions read the linked rows as the grid's filters do: what
// the row says of itself from the row, its cells raw. One that the fields
// there no longer fit (Amount turned to text) leaves the rollup saying why
// rather than adding up rows it can't judge.
func TestARollupsConditionsReadTheLinkedRowsAsTheGridDoes(t *testing.T) {
	fields, rows, _ := filterTable(t)
	values := func(r *model.Row) map[string]interface{} {
		var v map[string]interface{}
		_ = json.Unmarshal([]byte(r.Values), &v)
		return v
	}
	rc := &rollupCond{where: []Filter{{Field: "Amount", Op: "gte", Value: "1,000"}, {Field: "ID", Op: "lt", Value: "DEAL-14"}}}
	rc.ready(fields, reading{})
	var got []bool
	for _, r := range rows {
		got = append(got, rc.passes(r, values(r)))
	}
	if !slices.Equal(got, []bool{true, false, false, false}) || rc.err != nil {
		t.Errorf("the rows passing: %v (%v)", got, rc.err)
	}

	fields[1] = &model.Field{Id: fields[1].Id, Name: "Amount", Type: model.FieldText}
	rc = &rollupCond{where: []Filter{{Field: fields[1].Id.String(), Op: "gte", Value: "1,000"}}}
	rc.ready(fields, reading{})
	if rc.passes(rows[0], values(rows[0])) || rc.err == nil {
		t.Errorf("a condition its field no longer fits passed: %v", rc.err)
	}
	r := newLinkRead(1, 1, 1)
	r.t.conds = []*rollupCond{rc}
	r.t.rows[r.of[0]].passes = []bool{false}
	f := &rollupField{field: &model.Field{Id: uuid.New()}, cfg: rollupConfig{Aggregate: "count"}, link: r.l, cond: 1}
	if v := r.c.rollUp(f, r.row); v.Kind != formula.KindError || !strings.Contains(v.Str, `"Amount" compares text`) {
		t.Errorf("the rollup reads %#v, want it saying why", v.JSON())
	}
}
