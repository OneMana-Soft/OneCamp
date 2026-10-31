package business

// Rollups past adding up one way (tables B2, LK-8 and LK-10): a formula over
// the linked values, as Airtable's rollups have (SUM(values) * 1.18), and
// conditions, the grid's own filters, so only the linked rows that match are
// added up ("Spend on open orders"). And a formula's or a rollup's result
// format (FX-8), the number format a number field has, or a duration's.

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	formula "github.com/akashc777/OneCamp/business/Formula"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

const (
	// rollupByFormula is the way of adding up that's a formula over values.
	rollupByFormula = "formula"
	// maxRollupWhere is the most conditions a rollup has.
	maxRollupWhere = 20
)

// rollupDraftID names a rollup's formula to the engine.
const rollupDraftID = "rollup"

// rollupProgram is a rollup's formula, ready to run over a row's linked
// values, with the read's budget. Its error is why it can't.
func rollupProgram(src string) *formula.Program {
	return formula.Compile([]formula.Field{
		{ID: formula.RollupValues, Name: formula.RollupValues, Kind: formula.KindText},
		{ID: rollupDraftID, IsFormula: true, Formula: src},
	})
}

// checkRollupFormula is why a rollup's formula can't be saved, or nil.
func checkRollupFormula(src string) error {
	if strings.TrimSpace(src) == "" {
		return fmt.Errorf("Write the formula that adds up values, such as SUM(values)")
	}
	if _, err := formula.Check(src, []formula.Field{{ID: formula.RollupValues, Name: formula.RollupValues, Kind: formula.KindText}}, rollupDraftID); err != nil {
		return err
	}
	return nil
}

// rollupWhere checks a rollup's conditions against the fields of the table
// it adds up, as the grid's filters read them there (newRowFilters), and
// gives them as stored: each by field id. A condition on a field there
// that's worked out from other tables can't be checked.
func rollupWhere(raw interface{}, theirs []*model.Field) ([]Filter, error) {
	if raw == nil {
		return nil, nil
	}
	b, _ := json.Marshal(raw)
	var where []Filter
	if err := json.Unmarshal(b, &where); err != nil {
		return nil, fmt.Errorf("A rollup's conditions are a list of filters")
	}
	if len(where) > maxRollupWhere {
		return nil, fmt.Errorf("A rollup has at most %d conditions", maxRollupWhere)
	}
	var read []*model.Field
	for i, w := range where {
		f := resolveField(theirs, w.Field)
		if f == nil {
			return nil, fmt.Errorf("A condition's field isn't in the table this adds up")
		}
		if workedOutElsewhere(f, theirs) {
			return nil, fmt.Errorf("A condition can't read %q: it's worked out from other tables", f.Name)
		}
		switch FilterOp(strings.ToLower(strings.TrimSpace(w.Op))) {
		case FilterEq, FilterNe, FilterContains, FilterGt, FilterGte, FilterLt, FilterLte, FilterEmpty, FilterNotEmpty:
		default:
			return nil, fmt.Errorf("A condition compares with is, isn't, contains, more or less than, or is empty")
		}
		where[i].Field = f.Id.String()
		where[i].Op = strings.ToLower(strings.TrimSpace(w.Op))
		read = append(read, f)
	}
	if _, err := newRowFilters(theirs, where, reading{kinds: resultKinds(formula.Compile(formulaInputs(theirs)), read)}); err != nil {
		return nil, err
	}
	return where, nil
}

// workedOutElsewhere is whether a field of the table a rollup adds up is
// worked out from other tables (a link to a table, a rollup, a formula that
// reads them), which the rollup's read of that table doesn't work out.
func workedOutElsewhere(f *model.Field, fields []*model.Field) bool {
	_, isLink := linkOf(f)
	return isLink || f.Type == model.FieldRollup || (f.Type == model.FieldFormula && readsComputed(f, fields))
}

// rollupOrder checks how a rollup orders the linked rows it adds up, and how
// many of the first it takes (rollupConfig's Sort, Desc and Limit), against
// the fields of the table it adds up, and gives them as stored: the sort by
// field id, the limit a whole number.
func rollupOrder(cfg map[string]interface{}, theirs []*model.Field) (map[string]interface{}, error) {
	// As JSON reads it, whoever sent it (a Go caller's 3 is an int).
	b, _ := json.Marshal(map[string]interface{}{"sort": cfg["sort"], "desc": cfg["desc"], "limit": cfg["limit"]})
	var o struct {
		Sort  *string  `json:"sort"`
		Desc  *bool    `json:"desc"`
		Limit *float64 `json:"limit"`
	}
	if json.Unmarshal(b, &o) != nil {
		return nil, fmt.Errorf("A rollup sorts by a field, either way, and adds up a number of its first rows")
	}
	out := map[string]interface{}{}
	if o.Sort != nil && strings.TrimSpace(*o.Sort) != "" {
		f := resolveField(theirs, *o.Sort)
		if f == nil {
			return nil, fmt.Errorf("The field to sort by isn't in the table this adds up")
		}
		if why := sortProblem(f, theirs); why != "" {
			return nil, fmt.Errorf("%s", why)
		}
		out["sort"] = f.Id.String()
		if o.Desc != nil && *o.Desc {
			out["desc"] = true
		}
	}
	if o.Limit != nil {
		if n := *o.Limit; n != math.Trunc(n) || n < 1 || n > maxRolledUp {
			return nil, fmt.Errorf("A rollup adds up its first 1 to %d linked rows", maxRolledUp)
		}
		out["limit"] = int(*o.Limit)
	}
	return out, nil
}

// sortProblem is why a rollup can't order the linked rows it adds up by f, a
// field of their table, or "": the rollup's read works it out there, and a
// view can sort by it (rowQuery.Sorter, the tables' one order).
func sortProblem(f *model.Field, fields []*model.Field) string {
	if workedOutElsewhere(f, fields) {
		return fmt.Sprintf("A rollup can't sort by %q: it's worked out from other tables", f.Name)
	}
	if rowQuery.NewSorter([]*model.Field{f}, nil, f.Id.String(), false).Check() != nil {
		return fmt.Sprintf("A rollup can't sort by %q: sort by a number, a date, text, a choice or a tick", f.Name)
	}
	return ""
}

// resultFormat checks how a formula or a rollup shows a result: the number
// format a number field has (currency, decimals, percent, a bar), a
// duration ("h:mm" or "h:mm:ss", the seconds it gives as a clock), or a
// date's style. Nil when none is given.
func resultFormat(raw interface{}) (map[string]interface{}, error) {
	if raw == nil {
		return nil, nil
	}
	// As JSON reads it, whoever sent it (a Go caller's 2 is an int).
	b, err := json.Marshal(raw)
	var m map[string]interface{}
	if err != nil || json.Unmarshal(b, &m) != nil || m == nil {
		return nil, fmt.Errorf("A result's format is a set of choices")
	}
	out := map[string]interface{}{}
	for k, v := range m {
		switch k {
		case "currency":
			if v != "INR" && v != "USD" {
				return nil, fmt.Errorf("A result's currency is INR or USD")
			}
		case "decimals":
			n, ok := v.(float64)
			if !ok || n < 0 || n > 6 || n != float64(int(n)) {
				return nil, fmt.Errorf("A result shows 0 to 6 decimal places")
			}
		case "percent", "bar":
			if _, ok := v.(bool); !ok {
				return nil, fmt.Errorf("A result's %s is on or off", k)
			}
		case "duration":
			if v != "h:mm" && v != "h:mm:ss" {
				return nil, fmt.Errorf("A result shows a duration as h:mm or h:mm:ss")
			}
		case "date":
			if v != "short" && v != "long" && v != "iso" && v != "relative" {
				return nil, fmt.Errorf("A result shows a date short, long, as ISO or relative to today")
			}
		default:
			return nil, fmt.Errorf("A result's format has no %q", k)
		}
		out[k] = v
	}
	return out, nil
}

// rollupCond is a rollup's conditions as a read checks them in the table it
// adds up: as the grid's filters read them there, readied once a read
// (ready), or why they can't be, which the rollup then says (rollUp).
type rollupCond struct {
	where   []Filter
	filters *rowFilters
	err     error
}

// ready readies the conditions for a read of the linked rows, whose fields
// read as at says.
func (rc *rollupCond) ready(fields []*model.Field, at reading) {
	rc.filters, rc.err = newRowFilters(fields, rc.where, at)
}

// passes is whether a linked row meets every condition: what it says of
// itself from r, its stored cells raw in values, and its formulas as worked
// out.
func (rc *rollupCond) passes(r *model.Row, values map[string]interface{}) bool {
	if rc.err != nil {
		return false
	}
	ok, err := rc.filters.meets(r, values)
	if err != nil {
		rc.err = err
	}
	return ok
}

// rollupSort is an order rollups put the linked rows they add up in, by a
// field of their table, either way, as a view sorted by it shows them:
// readied once a read knows what that table's formulas give (readySorts), or
// why it can't be, which the rollups then say (rollUp).
type rollupSort struct {
	by     *model.Field
	desc   bool
	sorter *rowQuery.Sorter
	err    error
}

// sortingBy is where the order by field by, desc either way, is among t's
// sorts, counted from 1: one for every rollup that orders the rows so.
func (t *target) sortingBy(by *model.Field, desc bool) int {
	for i, s := range t.sorts {
		if s.by.Id == by.Id && s.desc == desc {
			return i + 1
		}
	}
	t.sorts = append(t.sorts, &rollupSort{by: by, desc: desc})
	return len(t.sorts)
}

// readySorts readies t's sorts for a read of its linked rows, whose formulas
// give what kinds says (resultKinds).
func (t *target) readySorts(kinds map[string]string) {
	for _, s := range t.sorts {
		s.sorter = rowQuery.NewSorter([]*model.Field{s.by}, kinds, s.by.Id.String(), s.desc)
	}
}

// key puts a linked row's place in each of t's sorts into lr: what it says of
// itself from r, its stored cells raw in values, and its formulas as worked
// out (rollupCond.passes').
func (t *target) key(lr *linkedRow, r *model.Row, values map[string]interface{}) {
	if len(t.sorts) == 0 {
		return
	}
	lr.keys = make([]rowQuery.SortKey, len(t.sorts))
	for i, s := range t.sorts {
		if s.err == nil && s.sorter != nil {
			lr.keys[i], s.err = s.sorter.Key(r, values)
		}
	}
}
