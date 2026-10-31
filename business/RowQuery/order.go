package business

// The order of a spec's sorts, in Go, as compile.go's ORDER BY gives it:
// text by its natural key (sortKey.go, the cell index's s), numbers and
// moments by value, a choice by its options' order and then its label's
// natural key, a tick after no tick; every empty value last either way, and
// rows that tie in the table's own order (place, then id).

import (
	"sort"
	"strings"
	"time"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// sortValue is one row's value for one sort: nil when empty.
type sortValue interface{}

// sortValuesOf is a row's values for a spec's order (specOrder); a choice
// takes two, as in SQL (its option's place, then its label's natural key),
// and a choice's or a date's group level what groupOrder orders by.
func sortValuesOf(sorts []sortBy, cols map[string]*column, kinds computedKinds, r *evalRow, who asker) ([]sortValue, []bool, error) {
	var vals []sortValue
	var desc []bool
	for _, s := range sorts {
		c := cols[s.Field]
		if c == nil {
			return nil, nil, errNoField(s.Field)
		}
		switch fam := effective(c, kinds); {
		case s.group && (fam == famChoice || fam == famDate):
			vals = append(vals, groupValues(c, fam, s, r, who)...)
		case fam == famText:
			// Blank as the index has it: no entry (cellIndex.go entryOf).
			if t, _ := textOf(r.values[c.key]); strings.TrimSpace(t) == "" {
				vals = append(vals, nil)
			} else {
				vals = append(vals, naturalKey(t))
			}
		case fam == famNumber:
			if n, ok := numberOf(c, r); ok {
				vals = append(vals, n)
			} else {
				vals = append(vals, nil)
			}
		case fam == famDate:
			// A day sorts as its midnight at UTC, whoever reads it, as the
			// index keeps it (cellIndex.go), so the order is one index's.
			if t, ok := momentOf(c, r, time.UTC); ok {
				vals = append(vals, t)
			} else {
				vals = append(vals, nil)
			}
		case fam == famCheckbox:
			b, _ := r.values[c.key].(bool)
			vals = append(vals, b)
		case fam == famChoice:
			s, _ := r.values[c.key].(string)
			label := strings.ToLower(strings.TrimSpace(s))
			if label == "" {
				vals = append(vals, nil, nil)
			} else {
				vals = append(vals, optionPlace(c, label), naturalKey(s))
			}
		default:
			return nil, nil, cantQuery("sorting by %s isn't built yet", c.field.Name)
		}
		// Each value this sort added sorts in its direction.
		for len(desc) < len(vals) {
			desc = append(desc, s.Desc)
		}
	}
	return vals, desc, nil
}

// compareSort orders two values of one sort, ascending, a nil last.
func compareSort(a, b sortValue) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	switch x := a.(type) {
	case string:
		return strings.Compare(x, b.(string))
	case float64:
		y := b.(float64)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case time.Time:
		return x.Compare(b.(time.Time))
	case bool:
		y := b.(bool)
		switch {
		case x == y:
			return 0
		case !x:
			return -1
		}
		return 1
	}
	return 0
}

// sortRows puts rows in a spec's order (specOrder), as the SQL would.
func sortRows(rows []*evalRow, sorts []sortBy, cols map[string]*column, kinds computedKinds, who asker) error {
	type keyed struct {
		r    *evalRow
		vals []sortValue
		desc []bool
	}
	ks := make([]keyed, len(rows))
	for i, r := range rows {
		v, d, err := sortValuesOf(sorts, cols, kinds, r, who)
		if err != nil {
			return err
		}
		ks[i] = keyed{r, v, d}
	}
	sort.SliceStable(ks, func(i, j int) bool {
		a, b := ks[i], ks[j]
		if c := compareKeys(a.vals, b.vals, a.desc); c != 0 {
			return c < 0
		}
		if a.r.positionKey != b.r.positionKey {
			switch {
			case a.r.positionKey == "":
				return false
			case b.r.positionKey == "":
				return true
			}
			return a.r.positionKey < b.r.positionKey
		}
		return a.r.id.String() < b.r.id.String()
	})
	for i := range ks {
		rows[i] = ks[i].r
	}
	return nil
}

// compareKeys orders two rows' values for an order (sortValuesOf's), each in
// its direction: below 0 when a comes first, 0 when they tie. NULLS LAST
// either way: only a non-nil pair turns round.
func compareKeys(a, b []sortValue, desc []bool) int {
	for k := range a {
		c := compareSort(a[k], b[k])
		if c == 0 {
			continue
		}
		if desc[k] && a[k] != nil && b[k] != nil {
			c = -c
		}
		return c
	}
	return 0
}

// Sorter orders rows a caller holds by one field of their table, in the
// order a view sorted by that field shows them (sortValuesOf, compareKeys).
// A rollup that sorts its linked rows reads it (business/DataTable), so an
// order means one thing wherever it runs.
type Sorter struct {
	by    []sortBy
	cols  map[string]*column
	kinds computedKinds
}

// NewSorter orders a table with fields by field (its id), desc largest or
// latest first: a formula or a rollup as what kinds says it gives
// (NewMatcher's).
func NewSorter(fields []*model.Field, kinds map[string]string, field string, desc bool) *Sorter {
	return &Sorter{by: []sortBy{{Sort: query.Sort{Field: field, Desc: desc}}}, cols: columns(fields), kinds: kindsOf(kinds)}
}

// SortKey is a row's place in a Sorter's order.
type SortKey struct {
	vals []sortValue
	desc []bool
}

// Key is a row's place in the order: what it reads of the row itself (its
// number, when it was made and changed) from r, and its cells from values,
// as the caller read them (Matcher.Matches').
func (s *Sorter) Key(r *model.Row, values map[string]interface{}) (SortKey, error) {
	er := rowOf(r)
	er.values = values
	vals, desc, err := sortValuesOf(s.by, s.cols, s.kinds, er, asker{zone: time.UTC})
	return SortKey{vals: vals, desc: desc}, err
}

// Check is why the order can't be read over the table, or nil: a field it
// hasn't got, or one no view sorts by (Matcher.Check's way, over a row with
// nothing in it).
func (s *Sorter) Check() error {
	_, err := s.Key(&model.Row{}, map[string]interface{}{})
	return err
}

// Compare orders two keys of one Sorter: below 0 when k comes first, 0 when
// they tie (the caller keeps its own order then), an empty value last either
// way.
func (k SortKey) Compare(o SortKey) int { return compareKeys(k.vals, o.vals, k.desc) }

// optionPlace is where label (lower case, trimmed) sorts among a choice's
// options, from 1; a label no option has after them all. The evaluator's
// sort and the index's entry (cellIndex.go) both read it.
func optionPlace(c *column, label string) float64 {
	for i, o := range c.options {
		if strings.ToLower(strings.TrimSpace(o)) == label {
			return float64(i + 1)
		}
	}
	return float64(len(c.options) + 1)
}
