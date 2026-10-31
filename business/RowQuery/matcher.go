package business

// Filters read over rows a caller holds (tables 22): business/DataTable's
// queries for the AI (query_table, query_plan) and a rollup's conditions
// read their filters with the evaluator itself, as the grid's views do, so a
// filter means one thing wherever it runs.

import (
	"strings"
	"time"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// Matcher reads filters over one table's rows as the evaluator does.
type Matcher struct {
	cols  map[string]*column
	kinds computedKinds
	who   asker
}

// NewMatcher reads filters over a table with fields: a formula or a rollup
// as what kinds says it gives ("number", "date", "checkbox", or text, by
// id), @me as me, and dates as of now in zone (UTC when nil).
func NewMatcher(fields []*model.Field, kinds map[string]string, me uuid.UUID, now time.Time, zone *time.Location) *Matcher {
	if zone == nil {
		zone = time.UTC
	}
	return &Matcher{cols: columns(fields), kinds: kindsOf(kinds), who: asker{user: me, now: now, zone: zone}}
}

// Matches is whether a row meets f: what it reads of the row itself (its
// number, when it was made and changed, and by whom) from r, and its cells
// from values, as the caller read them (formulas worked out, links named).
func (m *Matcher) Matches(f *query.Filter, r *model.Row, values map[string]interface{}) (bool, error) {
	er := rowOf(r)
	er.values = values
	return matches(f, er, m.cols, m.kinds, m.who)
}

// Check is why the evaluator can't read f over this table, or nil: f read
// once over a row with nothing in it, which reads its values as each
// condition does (a date's as the days it covers, a number's as one).
func (m *Matcher) Check(f *query.Filter) error {
	_, err := matches(f, &evalRow{values: map[string]interface{}{}}, m.cols, m.kinds, m.who)
	return err
}

// Comparison is what a field's conditions compare: text (a text's, or a
// choice's or a list's labels), a number, a date, a tick, or ids (people,
// and links to items or to rows).
type Comparison int

// The comparisons.
const (
	ComparesText Comparison = iota
	ComparesNumber
	ComparesDate
	ComparesCheckbox
	ComparesIDs
)

// Compares is what conditions on a field compare, by its ref ("field:<id>"
// or its id); false for a field the table hasn't got.
func (m *Matcher) Compares(ref string) (Comparison, bool) {
	c := m.cols[ref]
	if c == nil {
		return ComparesText, false
	}
	switch effective(c, m.kinds) {
	case famNumber:
		return ComparesNumber, true
	case famDate:
		return ComparesDate, true
	case famCheckbox:
		return ComparesCheckbox, true
	case famPeople, famRefs, famRowLink:
		return ComparesIDs, true
	}
	return ComparesText, true
}

// StoredText is text a row's stored cells hold, ignoring case, wherever the
// condition f holds, for a reader that narrows rows by their stored text
// before it reads them: the value "is" or "contains" compares a text, a
// choice or a list with (a choice's option as its label), or a date "is"
// (a stored date is its day). "" when there's none: any other condition, or
// a cell nothing stores (a formula, a rollup, people, links, the row's own
// columns).
func (m *Matcher) StoredText(f *query.Filter) string {
	c := m.cols[f.Field]
	if c == nil || c.col != colNone || len(f.Values) != 1 {
		return ""
	}
	is := f.Operator == query.OperatorIs
	switch c.family {
	case famText:
		if is || f.Operator == query.OperatorContains {
			return strings.TrimSpace(f.Values[0].Text)
		}
	case famChoice, famMulti:
		if is || f.Operator == query.OperatorContains {
			return strings.TrimSpace(valueLabel(c, f.Values[0]))
		}
	case famDate:
		if is {
			return f.Values[0].Text
		}
	}
	return ""
}
