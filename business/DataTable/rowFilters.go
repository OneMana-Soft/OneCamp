package business

// The AI's filters (query_table, query_plan) and a rollup's conditions, read
// as the grid's views read them (tables 22). Whether a row meets one is Row
// Query's evaluator's to say (rowQuery.Matcher), so a filter means one thing
// wherever it runs; this maps their plain words onto it. A filter names a
// field by its id or its name, compares with eq, ne, contains, gt, gte, lt,
// lte, empty or not_empty, and gives its value as text, as people write it:
//   - a number as people write numbers ("1,000", "$40"), and an id as it's
//     shown ("DEAL-12") as its row's number;
//   - a date as the day it falls on where the reader is (dayOf), or a month
//     or a year ("2026-10"); more and less than are after and before;
//   - a checkbox as ticked or not (FieldTypes' Truth); empty is unticked;
//   - a person or a link by the name each row's cell gives them for the
//     reader, by id, or @me.
// A blank value is empty (eq) or not (ne). A filter that can't compare its
// field (contains on a date, more than on text, a number that isn't one) is
// refused, saying why. One on a field the table hasn't got, or with an
// operator nothing knows, is left out, as it always was.

import (
	"context"
	"fmt"
	"strings"
	"time"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	formula "github.com/akashc777/OneCamp/business/Formula"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// reading is how a query's filters read: in the reader's zone (a time falls
// on a day there), @me as who asks, as of now, and each formula and rollup
// they name as what it gives (rowQuery.NewMatcher's kinds). The zero value
// reads in UTC, for no one, with formulas and rollups as text.
type reading struct {
	zone  *time.Location
	me    uuid.UUID
	now   time.Time
	kinds map[string]string
}

// readingFor is how filters over a table with fields read for actor, asking
// in ctx: a formula they name as a read plans it, with the fields its links
// read.
func readingFor(ctx context.Context, actor Actor, fields []*model.Field, filters []Filter) reading {
	var named []*model.Field
	var formulas []string
	for _, f := range filters {
		fld := resolveField(fields, f.Field)
		switch {
		case fld == nil:
		case fld.Type == model.FieldFormula:
			formulas = append(formulas, fld.Id.String())
			named = append(named, fld)
		case fld.Type == model.FieldRollup:
			named = append(named, fld)
		}
	}
	var program *formula.Program
	if len(formulas) > 0 {
		program = planComputedFor(asViewer(ctx, actor), fields, computedNeeds(fields, formulas)).program
	}
	return reading{zone: zoneFrom(ctx), me: actor.UserID, now: time.Now(), kinds: resultKinds(program, named)}
}

// resultKinds is what each formula and rollup among fields gives ("number",
// "text", "date" or "checkbox"), by id: a formula's as program compiled it,
// a rollup's by how it adds up. program may be nil when fields hold no
// formula.
func resultKinds(program *formula.Program, fields []*model.Field) map[string]string {
	kinds := map[string]string{}
	for _, f := range fields {
		switch f.Type {
		case model.FieldFormula:
			kinds[f.Id.String()] = program.Kind(f.Id.String()).Name()
		case model.FieldRollup:
			kinds[f.Id.String()] = kindOfField(f).Name()
		}
	}
	return kinds
}

// rowFilter is one filter as Row Query reads it: its condition, or, for a
// person or a link given by name, the condition each row's cell (at key)
// fills with the ids it gives that name (or, containing, a name with it in
// it).
type rowFilter struct {
	cond       query.Filter
	byName     bool
	key, name  string
	containing bool
}

// rowFilters is filters a row meets all of, as m reads them.
type rowFilters struct {
	m    *rowQuery.Matcher
	list []rowFilter
}

// newRowFilters maps filters over a table with fields as at reads them, or
// says why one can't be.
func newRowFilters(fields []*model.Field, filters []Filter, at reading) (*rowFilters, error) {
	rf := &rowFilters{m: rowQuery.NewMatcher(fields, at.kinds, at.me, at.now, at.zone)}
	for _, f := range filters {
		one, ok, err := rf.mapFilter(fields, f, at)
		if err != nil {
			return nil, err
		}
		if ok {
			rf.list = append(rf.list, one)
		}
	}
	return rf, nil
}

// The operators each comparison takes, as a refusal names them.
const (
	numberOps = "eq, ne, gt, gte, lt, lte, empty or not_empty"
	dateOps   = "eq, ne, gt (after), gte, lt (before), lte, empty or not_empty"
	tickOps   = "eq or ne with true or false, empty or not_empty"
	idOps     = "eq, ne, contains (a name), empty or not_empty"
	textOps   = "eq, ne, contains, empty or not_empty"
)

// mapFilter is f as Row Query reads it; false for a filter left out.
func (rf *rowFilters) mapFilter(fields []*model.Field, f Filter, at reading) (rowFilter, bool, error) {
	fld := resolveField(fields, f.Field)
	if fld == nil {
		return rowFilter{}, false, nil
	}
	ref := "field:" + fld.Id.String()
	compares, _ := rf.m.Compares(ref)
	op := FilterOp(strings.ToLower(strings.TrimSpace(f.Op)))
	v := strings.TrimSpace(f.Value)
	out := rowFilter{cond: query.Filter{Field: ref}, key: fld.Id.String()}
	set := func(operator string, vals ...query.Value) (rowFilter, bool, error) {
		out.cond.Operator, out.cond.Values = operator, vals
		return out, true, nil
	}
	refuse := func(as, ops string) (rowFilter, bool, error) {
		return rowFilter{}, false, fmt.Errorf("%q compares %s, so a filter on it can't use %s: use %s", fld.Name, as, op, ops)
	}
	switch {
	case op == FilterEmpty || (op == FilterEq && v == ""):
		if compares == rowQuery.ComparesCheckbox {
			return set(query.OperatorUnchecked)
		}
		return set(query.OperatorEmpty)
	case op == FilterNotEmpty || (op == FilterNe && v == ""):
		if compares == rowQuery.ComparesCheckbox {
			return set(query.OperatorChecked)
		}
		return set(query.OperatorNotEmpty)
	}
	ordered := map[FilterOp]string{FilterGt: query.OperatorGt, FilterGte: query.OperatorGte, FilterLt: query.OperatorLt, FilterLte: query.OperatorLte}
	if _, ok := ordered[op]; !ok && op != FilterEq && op != FilterNe && op != FilterContains {
		return rowFilter{}, false, nil
	}
	switch compares {
	case rowQuery.ComparesNumber:
		if op == FilterContains {
			return refuse("numbers", numberOps)
		}
		n, err := numberValue(fld, v)
		if err != nil {
			return rowFilter{}, false, err
		}
		switch op {
		case FilterEq:
			return set(query.OperatorIs, query.Value{Number: &n})
		case FilterNe:
			return set(query.OperatorIsNot, query.Value{Number: &n})
		}
		return set(ordered[op], query.Value{Number: &n})
	case rowQuery.ComparesDate:
		if op == FilterContains {
			return refuse("dates", dateOps)
		}
		day := query.Value{Text: dayOf(v, at.zone)}
		if rf.m.Check(&query.Filter{Field: ref, Operator: query.OperatorIs, Values: []query.Value{day}}) != nil {
			return rowFilter{}, false, fmt.Errorf("%q compares dates, and %q isn't one: write a day (2026-10-16), a month (2026-10) or a year", fld.Name, v)
		}
		switch op {
		case FilterEq:
			return set(query.OperatorIs, day)
		case FilterNe:
			out.cond = query.Filter{Op: query.OpNot, Filters: []query.Filter{{Field: ref, Operator: query.OperatorIs, Values: []query.Value{day}}}}
			return out, true, nil
		}
		return set(map[FilterOp]string{FilterGt: query.OperatorAfter, FilterGte: query.OperatorOnOrAfter, FilterLt: query.OperatorBefore, FilterLte: query.OperatorOnOrBefore}[op], day)
	case rowQuery.ComparesCheckbox:
		if op != FilterEq && op != FilterNe {
			return refuse("ticks", tickOps)
		}
		ticked, ok := fieldTypes.Truth(v)
		if !ok {
			return rowFilter{}, false, fmt.Errorf("%q compares ticks, and %q isn't one: use true or false", fld.Name, v)
		}
		if ticked == (op == FilterEq) {
			return set(query.OperatorChecked)
		}
		return set(query.OperatorUnchecked)
	case rowQuery.ComparesIDs:
		if _, isOrdered := ordered[op]; isOrdered {
			return refuse("people or links", idOps)
		}
		operator := query.OperatorIs
		if op == FilterNe {
			operator = query.OperatorIsNot
		}
		if _, err := uuid.Parse(v); op != FilterContains && (err == nil || v == query.Me) {
			return set(operator, query.Value{Text: v})
		}
		out.byName, out.name, out.containing = true, v, op == FilterContains
		return set(operator)
	}
	switch op {
	case FilterEq:
		return set(query.OperatorIs, query.Value{Text: v})
	case FilterNe:
		return set(query.OperatorIsNot, query.Value{Text: v})
	case FilterContains:
		return set(query.OperatorContains, query.Value{Text: v})
	}
	return refuse("text", textOps)
}

// numberValue is a number filter's value as people write numbers, an id's
// as it's shown ("DEAL-12" is 12).
func numberValue(fld *model.Field, v string) (float64, error) {
	prefix := ""
	if fld.Type == model.FieldAutonumber {
		prefix = fieldTypes.IDPrefix(fld)
	}
	if prefix != "" && len(v) > len(prefix)+1 && strings.EqualFold(v[:len(prefix)+1], prefix+"-") {
		v = v[len(prefix)+1:]
	}
	n, ok := formula.ParseNumber(v, formula.PlainSigns)
	switch {
	case ok:
		return n, nil
	case prefix != "":
		return 0, fmt.Errorf("%q compares ids such as %s-12, and %q isn't one", fld.Name, prefix, v)
	}
	return 0, fmt.Errorf("%q compares numbers, and %q isn't one", fld.Name, v)
}

// meets is whether a row, its cells as read in values, meets every filter.
func (rf *rowFilters) meets(r *model.Row, values map[string]interface{}) (bool, error) {
	for i := range rf.list {
		f := &rf.list[i]
		cond := f.cond
		if f.byName {
			cond.Values = namedIDs(values[f.key], f.name, f.containing)
		}
		ok, err := rf.m.Matches(&cond, r, values)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// namedIDs is the ids of the people or rows a cell, as read, gives the name
// (ignoring case), or, containing, a name with it in it. A link the reader
// can't open, or past those the cell shows, has no name.
func namedIDs(cell interface{}, name string, containing bool) []query.Value {
	want := strings.ToLower(name)
	items, _ := cell.([]interface{})
	var out []query.Value
	for _, it := range items {
		m, _ := it.(map[string]interface{})
		id, _ := m["id"].(string)
		label, _ := m["label"].(string)
		l := strings.ToLower(strings.TrimSpace(label))
		if id != "" && l != "" && (l == want || (containing && strings.Contains(l, want))) {
			out = append(out, query.Value{ID: id})
		}
	}
	return out
}

// terms is text each row meeting the filters holds in its stored cells,
// ignoring case (rowQuery.Matcher.StoredText), for the database to narrow
// the rows a query reads to those holding it, a superset of those the
// filters keep. A term the stored text may not hold as written isn't one:
// anything but printable ASCII, where ILIKE's case folding and Go's can
// differ, and a quote or a backslash, which a stored cell's JSON escapes.
func (rf *rowFilters) terms() []string {
	var out []string
	for i := range rf.list {
		if t := rf.m.StoredText(&rf.list[i].cond); t != "" && isASCIIPrintable(t) && !strings.ContainsAny(t, `"\`) {
			out = append(out, t)
		}
	}
	return out
}
