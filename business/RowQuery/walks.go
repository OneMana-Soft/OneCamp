package business

// A sort on one field as walks of the cell index (tables A3, perf-mobile's
// budget: a window at any depth of 100,000 rows in 20 ms). One SQL query
// sorting every matching row would read them all for each window, so a
// spec with no sort, or one sort on a number, a text, a choice or a tick,
// is answered as walks in order, each along one index, stopping at the
// window's end:
//
//   - the cells that have a value, in the index on (field, value, place,
//     row): the value in the sort's direction, ties in the table's order;
//   - then the rows with no value, in the table's own order (place, row),
//     as every empty value sorts last (order.go). A tick sorts no tick
//     first, so its walks go the other way round.
//
// A cursor names its walk and its row's keys there. After it, a walk reads
// the rest of the cursor's tie (same value, later place), then what comes
// after the value; both are ranges of the index. Rows with no place yet
// (a server from before the keys made them) and every other sort take
// compile.go's one query, which means the same.

import (
	"context"
	"strings"
	"time"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// walk is one part of a spec's order.
type walk struct {
	// field is the sort's field whose entries the walk reads (nil: the
	// rows themselves); col the entry's column that holds its value.
	field *column
	col   string
	// vals are the value keys (the entry's), in the sort's direction;
	// none for a tick, whose entries all hold true.
	vals []string
	desc bool
	// empty is the walk of the field's empty cells (the rows with no value,
	// after its values), along the index's empty part: every live row has
	// an entry of a walked field (cellIndex.go cellEntry).
	empty bool
	// eq is the one value field's entries hold on this walk (a filter's
	// "is", with no sort: those cells in the table's order); nil for any.
	eq *string
	// in is the first sort's value the walk keeps to, when two sorts are
	// walked; nil for none.
	in *group
}

// walksOf is a spec's order (specOrder: its groups, then its sorts) as
// walks, or nil where it can't be walked. With no sort, a filter that a text
// or a choice "is" one value walks that value's entries, in the table's
// order, as only its rows can meet it. One sort walks its field
// (sortWalks). Two, the first a choice or a tick, walk the second's index
// within each of the first's values in turn (an "is" of the first's on
// each), then the rows with no first value: a choice's option places are
// its order, so labels no option has can't be walked, and unknown names the
// choice whose entries must have none (the runner asks). So does a choice's
// group level alone, which orders such labels by their text too. A date's
// group level orders by its period, and a choice's second level by its
// text: no index has either order.
func walksOf(sorts []sortBy, filter *query.Filter, cols map[string]*column) (walks []walk, unknown *column, err error) {
	if len(sorts) == 0 {
		if c, v := equalityOf(filter, cols); c != nil {
			return []walk{{field: c, col: "t", eq: &v}}, nil, nil
		}
		return []walk{{}}, nil, nil
	}
	if len(sorts) > 2 {
		return nil, nil, nil
	}
	for i, s := range sorts {
		c := cols[s.Field]
		if c == nil {
			return nil, nil, errNoField(s.Field)
		}
		if s.group && (c.family == famDate || (i == 1 && c.family == famChoice)) {
			return nil, nil, nil
		}
	}
	if len(sorts) == 1 {
		if c := cols[sorts[0].Field]; sorts[0].group && c.family == famChoice {
			unknown = c
		}
		return sortWalks(sorts[0].Sort, cols), unknown, nil
	}
	first, by := cols[sorts[0].Field], sortWalks(sorts[1].Sort, cols)
	if by == nil || first.col != colNone || first == cols[sorts[1].Field] {
		return nil, nil, nil
	}
	var groups []group
	switch first.family {
	case famChoice:
		if len(first.options) == 0 || len(first.options) > maxGroups {
			return nil, nil, nil
		}
		seen := map[string]bool{}
		for _, o := range first.options {
			l := strings.ToLower(strings.TrimSpace(o))
			if l == "" || seen[l] { // a label sorts at its first option's place
				continue
			}
			seen[l] = true
			groups = append(groups, group{field: first, label: &l})
		}
		if sorts[0].Desc {
			for a, b := 0, len(groups)-1; a < b; a, b = a+1, b-1 {
				groups[a], groups[b] = groups[b], groups[a]
			}
		}
		groups = append(groups, group{field: first}) // no value: last either way
		unknown = first
	case famCheckbox:
		ticked, none := group{field: first, ticked: true}, group{field: first}
		groups = []group{none, ticked}
		if sorts[0].Desc {
			groups = []group{ticked, none}
		}
	default:
		return nil, nil, nil
	}
	for _, g := range groups {
		for _, w := range by {
			g := g
			w.in = &g
			walks = append(walks, w)
		}
	}
	return walks, unknown, nil
}

// maxGroups is the most values of a first sort walked one by one; past it,
// the one query sorts.
const maxGroups = 50

// group is one value of a first sort, which a walk keeps to: a choice's
// label (label nil: no value), or a tick (ticked) or none.
type group struct {
	field  *column
	label  *string
	ticked bool
}

// cond is the group's condition on a row.
func (g group) cond(a *sqlArgs) string {
	switch {
	case g.label != nil:
		return a.entry(g.field, `ci.t = `+a.add(*g.label)+`::text COLLATE "C"`)
	case g.ticked:
		return a.entry(g.field, "")
	}
	return not(a.entry(g.field, ""))
}

// groupHolds is whether any live row is in g (the first sort's value): one
// lookup of an index the field leads, its value's or its empty cells'. A seam
// for tests.
var groupHolds = func(ctx context.Context, g group) (bool, error) {
	switch {
	case g.label != nil:
		return model.FieldHolds(ctx, g.field.field.Id, "t", *g.label)
	case g.ticked:
		return model.FieldHolds(ctx, g.field.field.Id, "b", "")
	}
	return model.FieldHolds(ctx, g.field.field.Id, "empty", "")
}

// sortWalks is one sort as walks: its field's values, then the rows with
// none; nil where its field has no index order.
func sortWalks(s query.Sort, cols map[string]*column) []walk {
	c := cols[s.Field]
	if c == nil || c.col != colNone {
		return nil
	}
	var on walk
	switch c.family {
	case famNumber, famChoice:
		// A choice's place, then its label's key; a number's s is always
		// NULL, so (n, s) orders as n alone.
		on = walk{field: c, col: "n", vals: []string{"s.n", "s.s"}, desc: s.Desc}
	case famText:
		on = walk{field: c, col: "s", vals: []string{"s.s"}, desc: s.Desc}
	case famDate:
		on = walk{field: c, col: "d", vals: []string{"s.d"}, desc: s.Desc}
	case famCheckbox:
		on = walk{field: c, col: "b"}
	default:
		return nil
	}
	rest := walk{field: c, empty: true}
	if c.family == famCheckbox && !s.Desc {
		return []walk{rest, on}
	}
	return []walk{on, rest}
}

// equalityOf is a filter's condition that a text or a choice is one value,
// at its top or under ANDs only (so every row the filter takes meets it):
// its column and the value its entries hold. nil for none, and for a value
// longer than the t index holds (model.IndexedTextBytes).
func equalityOf(f *query.Filter, cols map[string]*column) (*column, string) {
	if f == nil {
		return nil, ""
	}
	if f.Op == query.OpAnd {
		for i := range f.Filters {
			if c, v := equalityOf(&f.Filters[i], cols); c != nil {
				return c, v
			}
		}
		return nil, ""
	}
	if f.Op != "" || (f.Operator != query.OperatorIs && f.Operator != query.OperatorIn) || len(f.Values) != 1 {
		return nil, ""
	}
	c := cols[f.Field]
	if c == nil || c.col != colNone {
		return nil, ""
	}
	var v string
	switch c.family {
	case famText:
		v = strings.ToLower(strings.TrimSpace(f.Values[0].Text))
	case famChoice:
		if want := wantLabels(c, f.Operator, f.Values); len(want) == 1 {
			v = want[0]
		}
	}
	if v == "" || len(v) > model.IndexedTextBytes {
		return nil, ""
	}
	return c, v
}

// onWalk is the filter's conditions on the walk's own field, at its top or
// under ANDs only, as conditions on the walk's entry s: a date range the walk
// is sorted by bounds the index it walks, rather than probing every entry
// before the range. "" for none.
//
// Only a condition a row meets by a value: on s a missing value is NULL, and
// NOT of NULL drops the empty cell a negation keeps, so a negation is left to
// the filter. What is kept stands bare, so the planner can bound the index by
// it; NULL and false are one in a WHERE's AND.
func onWalk(f *query.Filter, c *column, who asker, a *sqlArgs) string {
	if f == nil || c == nil {
		return ""
	}
	if f.Op == query.OpAnd {
		var parts []string
		for i := range f.Filters {
			if p := onWalk(&f.Filters[i], c, who, a); p != "" {
				parts = append(parts, p)
			}
		}
		return strings.Join(parts, " AND ")
	}
	if f.Op != "" || f.Field != "field:"+c.key && f.Field != c.key {
		return ""
	}
	on := &sqlArgs{base: a.base, vals: a.vals, on: "s"}
	sql, err := conditionSQL(c, f.Operator, f.Values, who, on)
	if err != nil || strings.HasPrefix(sql, "NOT ") {
		return ""
	}
	if bound := dayBound(c, f, who, on); bound != "" {
		sql += " AND " + bound
	}
	a.vals = on.vals
	return "(" + sql + ")"
}

// dayBound is a date range as a range of s.d alone, which an index on d can
// be bounded by: the range's own test is days' dates OR moments' instants,
// and an OR bounds no index. A day's d is its UTC midnight, within two days
// of the instants of its date anywhere, so the range widened by two days at
// each end holds every entry the range does. "" when there is no range.
func dayBound(c *column, f *query.Filter, who asker, a *sqlArgs) string {
	if c.family != famDate || c.col != colNone {
		return ""
	}
	from, to, err := dateWindow(f.Operator, f.Values, who)
	if err != nil {
		return ""
	}
	const margin = 48 * time.Hour
	var parts []string
	if !from.IsZero() {
		parts = append(parts, `s.d >= `+a.add(from.Add(-margin))+`::timestamptz`)
	}
	if !to.IsZero() {
		parts = append(parts, `s.d < `+a.add(to.Add(margin))+`::timestamptz`)
	}
	return strings.Join(parts, " AND ")
}

// shortWalkT is that a walk's entry's t is one the t index holds, in the words
// of its predicate (model.ShortText), worked out once.
var shortWalkT = model.ShortText("s.t")

// walkKeys is a walk's keys as it orders by them: its values, then the
// place and the row.
func (w walk) walkKeys() []string {
	if w.field == nil {
		return []string{"r.position_key", "r.id"}
	}
	return append(append([]string{}, w.vals...), "s.p", "s.row_id")
}

// sql is the walk's joins, its condition and its ORDER BY, each in the
// direction its index is built (so no NULLS clause: a value is never NULL
// on its walk, and a number's s always is).
func (w walk) sql(a *sqlArgs) (joins, cond, order string) {
	if w.field == nil {
		cond = "TRUE"
		if w.in != nil {
			cond += " AND COALESCE((" + w.in.cond(a) + "), FALSE)"
		}
		return "", cond, "ORDER BY r.position_key, r.id"
	}
	joins = `JOIN data_table_cell_index s ON s.row_id = r.id AND s.field_id = ` + a.add(w.field.key) + `::uuid`
	parts := make([]string, 0, len(w.vals)+2)
	for _, v := range w.vals {
		if w.desc {
			v += " DESC"
		}
		parts = append(parts, v)
	}
	parts = append(parts, "s.p", "s.row_id")
	cond = "s." + w.col + " IS NOT NULL"
	if w.empty {
		cond = emptyEntry
	}
	if w.eq != nil {
		// In the words of the t index's predicate, so the walk reads it
		// (equalityOf walks only a value short enough to be there).
		cond = `s.t = ` + a.add(*w.eq) + `::text COLLATE "C" AND ` + shortWalkT
	}
	if w.in != nil {
		cond += " AND COALESCE((" + w.in.cond(a) + "), FALSE)"
	}
	return joins, cond, "ORDER BY " + strings.Join(parts, ", ")
}

// afterIn is one condition after a cursor's keys on this walk: part 0 the
// rest of its tie (same value, later place), part 1 what comes after its
// value (a walk with no value has part 0 alone). Only the parameters the
// part reads are added, as Postgres can't type one it never sees.
func (w walk) afterIn(at []interface{}, part int, a *sqlArgs) string {
	cast := func(v string) string {
		switch {
		case strings.HasSuffix(v, ".s"), strings.HasSuffix(v, ".t"):
			return `::text COLLATE "C"`
		case strings.HasSuffix(v, ".d"):
			return "::timestamptz"
		}
		return "::numeric"
	}
	if part == 1 {
		var cols, vals []string
		for i, v := range w.vals {
			if at[i] != nil { // a number's t is NULL: n alone orders it
				cols, vals = append(cols, v), append(vals, a.add(at[i])+cast(v))
			}
		}
		cmp := ">"
		if w.desc {
			cmp = "<"
		}
		return `(` + strings.Join(cols, ", ") + `) ` + cmp + ` (` + strings.Join(vals, ", ") + `)`
	}
	var eq []string
	for i, v := range w.vals {
		if at[i] == nil {
			eq = append(eq, v+" IS NULL")
		} else {
			eq = append(eq, v+" = "+a.add(at[i])+cast(v))
		}
	}
	place, row := "r.position_key", "r.id"
	if w.field != nil {
		place, row = "s.p", "s.row_id"
	}
	n := len(at)
	tail := `(` + place + `, ` + row + `) > (` + a.add(at[n-2]) + `::text COLLATE "C", ` + a.add(at[n-1]) + `::uuid)`
	return strings.Join(append(eq, tail), " AND ")
}

// walked is a row found on a walk, with the walk and its keys there.
type walked struct {
	id   uuid.UUID
	walk int
	keys []interface{}
}

// runWalks answers a window of a spec's walks after a cursor: where is the
// spec's condition, its parameters in a.
func runWalks(ctx context.Context, table uuid.UUID, walks []walk, where string, a *sqlArgs, filter *query.Filter, who asker, after query.Cursor, size int, budget time.Duration) ([]walked, error) {
	started := time.Now()
	start, at := 0, []interface{}(nil)
	if after != "" {
		v, err := readCursor(after, "walk", -1)
		if err != nil || len(v) < 3 {
			return nil, ErrBadCursor
		}
		i, ok := v[0].(float64)
		if !ok || i != float64(int(i)) || int(i) < 0 || int(i) >= len(walks) || len(v)-1 != len(walks[int(i)].walkKeys()) {
			return nil, ErrBadCursor
		}
		start, at = int(i), v[1:]
	}
	var out []walked
	held := map[group]bool{} // whether a group holds any row, asked once
	for wi := start; wi < len(walks) && len(out) <= size; wi++ {
		w := walks[wi]
		if w.in != nil {
			// A group with no row is passed over by one lookup: walking it
			// would read the second sort's every entry to find none.
			has, asked := held[*w.in]
			if !asked {
				var err error
				if has, err = groupHolds(ctx, *w.in); err != nil {
					return nil, err
				}
				held[*w.in] = has
			}
			if !has {
				continue
			}
		}
		resuming := wi == start && at != nil
		parts := 1 // after a cursor: the rest of its tie, then past its value
		if resuming && len(w.vals) > 0 {
			parts = 2
		}
		for part := 0; part < parts && len(out) <= size; part++ {
			q := &sqlArgs{base: a.base, vals: append([]interface{}{}, a.vals...)}
			joins, cond, order := w.sql(q)
			// The filter and the group as conditions of each walked row, never
			// joined: a join would read every row the filter takes before the
			// first one walked (COALESCE keeps an EXISTS a probe).
			conds := "COALESCE((" + where + "), FALSE) AND (" + cond + ")"
			if own := onWalk(filter, w.field, who, q); own != "" {
				conds += " AND (" + own + ")"
			}
			if resuming {
				conds += " AND (" + w.afterIn(at, part, q) + ")"
			}
			left := time.Duration(0)
			if budget > 0 {
				if left = budget - time.Since(started); left <= 0 {
					return nil, model.ErrWalkTooLong
				}
			}
			found, err := model.QueryRowWalk(ctx, table, joins, conds, w.walkKeys(), order, q.vals, size+1-len(out), left)
			if err != nil {
				return nil, err
			}
			for _, f := range found {
				out = append(out, walked{id: f.ID, walk: wi, keys: f.Keys})
			}
		}
	}
	return out, nil
}
