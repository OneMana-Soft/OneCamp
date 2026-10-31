package business

// The meaning of every condition and sort, in Go (tables A3): what the SQL
// compiler (compile.go) must agree with, row for row, as the property test
// holds over random tables; and how a spec naming a formula or a rollup is
// answered until A4 stores their values, over the rows one read can work out.
//
// Text compares ignoring case. Numbers and dates are typed: a cell that isn't
// one is empty. A choice compares by label (an option's id is its label). A
// person, an item or a linked row compares by id, and @me is the asker. "Is
// not" and "doesn't contain" take in the empty cells, as Airtable's do. An
// empty cell sorts last either way, and rows that tie keep the table's order.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	"github.com/google/uuid"
)

// evalRow is one row as the evaluator reads it.
type evalRow struct {
	id          uuid.UUID
	positionKey string
	values      map[string]interface{}
	rowNo       int64
	created     time.Time
	updated     time.Time
	createdBy   uuid.UUID
	updatedBy   uuid.UUID
	// links are the rows each table link links this row to, by the link
	// field's id (for the other side too, by its own id).
	links map[string][]uuid.UUID
}

// asker is what a query reads of who asks and where: @me, today and their
// zone.
type asker struct {
	user  uuid.UUID
	admin bool
	agent uuid.UUID
	now   time.Time
	zone  *time.Location
}

// computedKinds is what each formula and rollup gives, so it compares as that
// family; their values are in the row's values, worked out for the read.
type computedKinds map[string]family

// ErrCantQuery is a condition or a sort that can't be answered (yet): its
// message says which and why.
var ErrCantQuery = errors.New("row query: that can't be asked of this table yet")

func cantQuery(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCantQuery, fmt.Sprintf(format, args...))
}

// effective is the family a column compares as: a formula or a rollup as
// what it gives.
func effective(c *column, kinds computedKinds) family {
	if c.family == famComputed {
		if k, ok := kinds[c.key]; ok {
			return k
		}
		return famText
	}
	return c.family
}

// matches is whether a row meets a filter.
func matches(f *query.Filter, r *evalRow, cols map[string]*column, kinds computedKinds, who asker) (bool, error) {
	if f == nil {
		return true, nil
	}
	switch f.Op {
	case query.OpAnd, query.OpOr:
		for i := range f.Filters {
			ok, err := matches(&f.Filters[i], r, cols, kinds, who)
			if err != nil {
				return false, err
			}
			if ok == (f.Op == query.OpOr) {
				return ok, nil
			}
		}
		return f.Op == query.OpAnd, nil
	case query.OpNot:
		ok, err := matches(&f.Filters[0], r, cols, kinds, who)
		return !ok, err
	}
	c := cols[f.Field]
	if c == nil {
		return false, errNoField(f.Field)
	}
	return condition(c, effective(c, kinds), f.Operator, f.Values, r, who)
}

// --- reading a cell -------------------------------------------------------

// textOf is a cell as text, as Postgres's ->> gives it: a string as it is,
// anything else as JSON, nothing for no cell.
func textOf(raw interface{}) (string, bool) {
	switch v := raw.(type) {
	case nil:
		return "", false
	case string:
		return v, true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(v), true
	default:
		b, _ := json.Marshal(v)
		return string(b), true
	}
}

func numberOf(c *column, r *evalRow) (float64, bool) {
	if c.col == colRowNo {
		return float64(r.rowNo), r.rowNo > 0
	}
	f, ok := r.values[c.key].(float64)
	return f, ok && !math.IsNaN(f) && !math.IsInf(f, 0)
}

// dayLayout is a date cell with no time of day.
const dayLayout = "2006-01-02"

// momentOf is a date cell as the moment it starts: a day at midnight where
// the asker is, a time as it is.
func momentOf(c *column, r *evalRow, zone *time.Location) (time.Time, bool) {
	switch c.col {
	case colCreatedAt:
		return r.created, !r.created.IsZero()
	case colUpdatedAt:
		return r.updated, !r.updated.IsZero()
	}
	s, ok := r.values[c.key].(string)
	if !ok {
		return time.Time{}, false
	}
	return parseMoment(s, zone)
}

func parseMoment(s string, zone *time.Location) (time.Time, bool) {
	if len(s) == len(dayLayout) {
		t, err := time.ParseInLocation(dayLayout, s, zone)
		return t, err == nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// labelsOf is a choice's or a multi-select's labels.
func labelsOf(raw interface{}) []string {
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{v}
	case []interface{}:
		var out []string
		for _, it := range v {
			if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
			// A file, by its name (a files cell).
			if m, ok := it.(map[string]interface{}); ok {
				if s, _ := m["name"].(string); strings.TrimSpace(s) != "" {
					out = append(out, s)
				}
			}
		}
		return out
	}
	return nil
}

// idsOf is the people or items a cell names, or a row column's person: who
// last changed a row is whoever made it until someone does.
func idsOf(c *column, r *evalRow) []string {
	switch c.col {
	case colCreatedBy, colUpdatedBy:
		by := r.createdBy
		if c.col == colUpdatedBy && r.updatedBy != uuid.Nil {
			by = r.updatedBy
		}
		if by != uuid.Nil {
			return []string{by.String()}
		}
		return nil
	}
	if c.family == famRowLink && r.links != nil {
		var out []string
		for _, id := range r.links[c.key] {
			out = append(out, id.String())
		}
		return out
	}
	// A link's cell as a read works it out: a ref for each linked row.
	items, _ := r.values[c.key].([]interface{})
	var out []string
	for _, it := range items {
		switch v := it.(type) {
		case map[string]interface{}:
			if id, ok := v["id"].(string); ok && id != "" {
				out = append(out, strings.ToLower(id))
			}
		case string:
			if v != "" {
				out = append(out, strings.ToLower(v))
			}
		}
	}
	return out
}

// --- the values a condition compares with ---------------------------------

func valueNumber(v query.Value) (float64, error) {
	if v.Number != nil {
		return *v.Number, nil
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v.Text), 64)
	if err != nil {
		return 0, cantQuery("%q isn't a number", v.Text)
	}
	return f, nil
}

// valueLabel is a choice's value as a label: an option's id as its label.
func valueLabel(c *column, v query.Value) string {
	if l, ok := c.labelOf[v.ID]; ok {
		return l
	}
	if l, ok := c.labelOf[v.Text]; ok {
		return l
	}
	if v.Text != "" {
		return v.Text
	}
	return v.ID
}

// valueID is a person's or an item's value as an id: @me as the asker.
func valueID(v query.Value, who asker) string {
	if v.Text == query.Me || v.ID == query.Me {
		return who.user.String()
	}
	if v.ID != "" {
		return strings.ToLower(v.ID)
	}
	return strings.ToLower(strings.TrimSpace(v.Text))
}

// --- conditions -----------------------------------------------------------

func condition(c *column, fam family, op string, vals []query.Value, r *evalRow, who asker) (bool, error) {
	switch fam {
	case famText:
		s, _ := textOf(r.values[c.key])
		return textCondition(op, s, vals)
	case famNumber:
		n, ok := numberOf(c, r)
		return numberCondition(op, n, ok, vals)
	case famDate:
		t, ok := momentOf(c, r, who.zone)
		return dateCondition(op, t, ok, vals, who)
	case famCheckbox:
		b, _ := r.values[c.key].(bool)
		switch op {
		case query.OperatorChecked:
			return b, nil
		case query.OperatorUnchecked:
			return !b, nil
		}
	case famChoice:
		// A choice is its label: text, as the index reads it.
		s, _ := r.values[c.key].(string)
		return choiceCondition(c, op, labelsOf(s), vals)
	case famMulti:
		return choiceCondition(c, op, labelsOf(r.values[c.key]), vals)
	case famPeople, famRefs, famRowLink:
		return idCondition(op, idsOf(c, r), fieldTypes.HasMoreRef(r.values[c.key]), vals, who)
	}
	return false, cantQuery("%s can't be compared with %s", c.field.Name, op)
}

func textCondition(op, cell string, vals []query.Value) (bool, error) {
	low := strings.ToLower(strings.TrimSpace(cell))
	anyOf := func(test func(want string) bool) bool {
		for _, v := range vals {
			if test(strings.ToLower(strings.TrimSpace(v.Text))) {
				return true
			}
		}
		return false
	}
	switch op {
	case query.OperatorEmpty:
		return low == "", nil
	case query.OperatorNotEmpty:
		return low != "", nil
	case query.OperatorIs, query.OperatorIn:
		return anyOf(func(w string) bool { return low == w }), nil
	case query.OperatorIsNot, query.OperatorNotIn:
		return !anyOf(func(w string) bool { return low == w }), nil
	case query.OperatorContains:
		return anyOf(func(w string) bool { return strings.Contains(low, w) }), nil
	case query.OperatorNotContains:
		return !anyOf(func(w string) bool { return strings.Contains(low, w) }), nil
	case query.OperatorStartsWith:
		return anyOf(func(w string) bool { return strings.HasPrefix(low, w) }), nil
	case query.OperatorEndsWith:
		return anyOf(func(w string) bool { return strings.HasSuffix(low, w) }), nil
	}
	return false, cantQuery("text can't be compared with %s", op)
}

func numberCondition(op string, n float64, ok bool, vals []query.Value) (bool, error) {
	switch op {
	case query.OperatorEmpty:
		return !ok, nil
	case query.OperatorNotEmpty:
		return ok, nil
	}
	nums := make([]float64, len(vals))
	for i, v := range vals {
		f, err := valueNumber(v)
		if err != nil {
			return false, err
		}
		nums[i] = f
	}
	switch op {
	case query.OperatorIs, query.OperatorIn:
		for _, x := range nums {
			if ok && n == x {
				return true, nil
			}
		}
		return false, nil
	case query.OperatorIsNot, query.OperatorNotIn:
		for _, x := range nums {
			if ok && n == x {
				return false, nil
			}
		}
		return true, nil
	case query.OperatorGt:
		return ok && n > nums[0], nil
	case query.OperatorGte:
		return ok && n >= nums[0], nil
	case query.OperatorLt:
		return ok && n < nums[0], nil
	case query.OperatorLte:
		return ok && n <= nums[0], nil
	case query.OperatorBetween:
		return ok && n >= nums[0] && n <= nums[1], nil
	}
	return false, cantQuery("a number can't be compared with %s", op)
}

// dateWindow is the moments a condition on a date covers: [from, to), either
// end open when zero.
func dateWindow(op string, vals []query.Value, who asker) (from, to time.Time, err error) {
	switch op {
	case query.OperatorEmpty, query.OperatorNotEmpty:
		return
	}
	if len(vals) == 0 {
		return from, to, cantQuery("%s needs a date", op)
	}
	f0, t0, err := vals[0].Window(who.now, who.zone, op, false)
	if err != nil {
		return from, to, cantQuery("%s", err)
	}
	switch op {
	case query.OperatorIs, query.OperatorWithin:
		return f0, t0, nil
	case query.OperatorBefore:
		return time.Time{}, f0, nil
	case query.OperatorAfter:
		return t0, time.Time{}, nil
	case query.OperatorOnOrBefore:
		return time.Time{}, t0, nil
	case query.OperatorOnOrAfter:
		return f0, time.Time{}, nil
	case query.OperatorBetween:
		_, t1, err := vals[1].Window(who.now, who.zone, op, false)
		if err != nil {
			return from, to, cantQuery("%s", err)
		}
		return f0, t1, nil
	}
	return from, to, cantQuery("a date can't be compared with %s", op)
}

func dateCondition(op string, t time.Time, ok bool, vals []query.Value, who asker) (bool, error) {
	switch op {
	case query.OperatorEmpty:
		return !ok, nil
	case query.OperatorNotEmpty:
		return ok, nil
	}
	from, to, err := dateWindow(op, vals, who)
	if err != nil {
		return false, err
	}
	return ok && (from.IsZero() || !t.Before(from)) && (to.IsZero() || t.Before(to)), nil
}

func choiceCondition(c *column, op string, labels []string, vals []query.Value) (bool, error) {
	have := map[string]bool{}
	for _, l := range labels {
		have[strings.ToLower(strings.TrimSpace(l))] = true
	}
	var want []string
	if op == query.OperatorStatusGroupIs {
		for _, v := range vals {
			for _, l := range c.group[strings.TrimSpace(v.Text)] {
				want = append(want, strings.ToLower(l))
			}
		}
	} else {
		for _, v := range vals {
			want = append(want, strings.ToLower(strings.TrimSpace(valueLabel(c, v))))
		}
	}
	anyOf := func() bool {
		for _, w := range want {
			if have[w] {
				return true
			}
		}
		return false
	}
	switch op {
	case query.OperatorEmpty:
		return len(have) == 0, nil
	case query.OperatorNotEmpty:
		return len(have) > 0, nil
	case query.OperatorIs, query.OperatorIn, query.OperatorStatusGroupIs:
		return anyOf(), nil
	case query.OperatorIsNot, query.OperatorNotIn:
		return !anyOf(), nil
	case query.OperatorAllOf:
		for _, w := range want {
			if !have[w] {
				return false, nil
			}
		}
		return true, nil
	case query.OperatorIsExactly:
		set := map[string]bool{}
		for _, w := range want {
			set[w] = true
		}
		if len(set) != len(have) {
			return false, nil
		}
		for w := range set {
			if !have[w] {
				return false, nil
			}
		}
		return true, nil
	case query.OperatorContains, query.OperatorNotContains:
		found := false
		for l := range have {
			if len(want) > 0 && strings.Contains(l, want[0]) {
				found = true
				break
			}
		}
		return found == (op == query.OperatorContains), nil
	}
	return false, cantQuery("%s can't be compared with %s", c.field.Name, op)
}

// idCondition compares the ids a cell names; more is a link cell that counts
// links past those it names, which has links whichever it names.
func idCondition(op string, ids []string, more bool, vals []query.Value, who asker) (bool, error) {
	have := map[string]bool{}
	for _, id := range ids {
		have[id] = true
	}
	var want []string
	for _, v := range vals {
		want = append(want, valueID(v, who))
	}
	anyOf := func() bool {
		for _, w := range want {
			if have[w] {
				return true
			}
		}
		return false
	}
	switch op {
	case query.OperatorEmpty:
		return len(have) == 0 && !more, nil
	case query.OperatorNotEmpty:
		return len(have) > 0 || more, nil
	case query.OperatorIs, query.OperatorIn:
		return anyOf(), nil
	case query.OperatorIsNot, query.OperatorNotIn:
		return !anyOf(), nil
	case query.OperatorAllOf:
		for _, w := range want {
			if !have[w] {
				return false, nil
			}
		}
		return true, nil
	}
	return false, cantQuery("people and links can't be compared with %s", op)
}
