package business

// A spec's groups (QUERY_STACK_CONTRACT.md item 7): its rows in the order of
// up to three group levels, ahead of its sorts; each group's count and
// summaries at every level, over every row the spec keeps; and the reader's
// collapsed groups left out of the windows, never out of the counts.
//
// A choice groups by its option's label as the field names it (a label no
// option has by its text, in lower case), a tick by whether it is one, a
// number by its value, and a date by the first day of its period where the
// reader is: a day by its own date, a week from its Monday (as Postgres's
// date_trunc). An empty cell is the group "", last either way.
//
// The one query reads each key from the entry the cell index holds
// (groupKey), orders the rows by the groups as leading sorts, and counts
// every level beside the first window in one statement, GROUP BY ROLLUP
// (aggregateSQL). The evaluator works the same keys out from the entry a row
// would have (keyOf, from rowEntry), so the two group alike, row for row.

import (
	"database/sql"
	"math"
	"math/bits"
	"strconv"
	"strings"
	"time"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// sortBy is one key of a spec's order: a group level's (the groups come
// first) or a sort's.
type sortBy struct {
	query.Sort
	// group is a group level's: a choice orders by its entry's text after its
	// natural key, so two labels no option has that sort alike stay apart
	// (they are two groups), and a date by its period, granularity's.
	group       bool
	granularity string
}

// specOrder is a spec's order: its groups, then its sorts.
func specOrder(spec query.Spec) []sortBy {
	order := make([]sortBy, 0, len(spec.Groups)+len(spec.Sorts))
	for _, g := range spec.Groups {
		order = append(order, levelOf(g))
	}
	return append(order, bySorts(spec.Sorts)...)
}

// levelOf is a group level as a key of its spec's order.
func levelOf(g query.Group) sortBy {
	return sortBy{Sort: query.Sort{Field: g.Field, Desc: g.Desc}, group: true, granularity: g.Granularity}
}

// bySorts is sorts as keys of an order.
func bySorts(sorts []query.Sort) []sortBy {
	order := make([]sortBy, len(sorts))
	for i, s := range sorts {
		order[i] = sortBy{Sort: s}
	}
	return order
}

// period is a date group's: by day unless it says otherwise.
func (s sortBy) period() string {
	if s.granularity == "" {
		return query.GranularityDay
	}
	return s.granularity
}

// checkGroups is a spec's groups checked against a table's columns, read as
// kinds says (a formula or a rollup as what it gives): each names a field
// the table has, of a kind that groups, and only a date takes a period.
func checkGroups(groups []query.Group, cols map[string]*column, kinds computedKinds) error {
	for _, g := range groups {
		c := cols[g.Field]
		if c == nil {
			return errNoField(g.Field)
		}
		switch fam := effective(c, kinds); {
		case fam != famChoice && fam != famCheckbox && fam != famNumber && fam != famDate:
			return cantQuery("grouping by %s isn't built yet", c.field.Name)
		case g.Granularity != "" && fam != famDate:
			return cantQuery("%s isn't a date, so it can't be grouped by %s", c.field.Name, g.Granularity)
		}
	}
	return nil
}

// tooManyGroups is a spec whose rows fall into more groups than its counts
// answer.
func tooManyGroups() error {
	return cantQuery("grouped this way, the rows fall into more than %s groups: group by a field with fewer values, or a date by a longer period", helpers.Thousands(query.MaxGroupCounts))
}

// groupOrder is the keys a choice's or a date's group level orders by, its
// entry joined through a (a row's own column read from r): a choice's option
// place, its label's natural key, then its text; a date's period. Any other
// level orders as a sort by its field does.
func groupOrder(c *column, s sortBy, who asker, a *sqlArgs) []sortKey {
	if c.family == famDate {
		m := ""
		if c.col == colNone {
			m = a.joined(c)
		}
		return []sortKey{{expr: periodSQL(c, s, m, who, a), desc: s.Desc, kind: "text"}}
	}
	j := a.joined(c)
	return []sortKey{{expr: j + `.n`, desc: s.Desc, kind: "num"}, {expr: j + `.s`, desc: s.Desc, kind: "text"}, {expr: j + `.t`, desc: s.Desc, kind: "text"}}
}

// groupKey is a group level's key as SQL, read from c's entry as m (a row's
// own column from r): NULL for an empty cell, but a tick's none is false.
func groupKey(c *column, s sortBy, m string, who asker, a *sqlArgs) string {
	switch c.family {
	case famChoice:
		return `COALESCE((` + a.add(c.options) + `::text[])[` + m + `.n::int], ` + m + `.t)`
	case famCheckbox:
		return `COALESCE(` + m + `.b, FALSE)`
	case famDate:
		return periodSQL(c, s, m, who, a)
	}
	if c.col == colRowNo {
		return `NULLIF(r.row_no, 0)::numeric`
	}
	return m + `.n`
}

// periodSQL is the first day of the period a date is in, where the reader
// is, as text (2026-10-01, which sorts as the date does): a day's own date,
// any other moment's in the reader's zone. m is the entry's alias; a row's
// own column is read from r.
func periodSQL(c *column, s sortBy, m string, who asker, a *sqlArgs) string {
	var at string
	switch c.col {
	case colCreatedAt:
		at = `r.created_at AT TIME ZONE ` + a.add(who.zone.String()) + `::text`
	case colUpdatedAt:
		at = `r.updated_at AT TIME ZONE ` + a.add(who.zone.String()) + `::text`
	default:
		at = `COALESCE(` + m + `.t::date::timestamp, ` + m + `.d AT TIME ZONE ` + a.add(who.zone.String()) + `::text)`
	}
	return `to_char(date_trunc(` + a.add(s.period()) + `::text, ` + at + `), 'YYYY-MM-DD')`
}

// probedKey is a group level's key read by a probe of c's entry by (row,
// field), joined to nothing: a condition of a window's, where a walk reads
// only the rows it walks.
func probedKey(c *column, s sortBy, who asker, a *sqlArgs) string {
	if c.col != colNone {
		return groupKey(c, s, "", who, a)
	}
	probe := `(SELECT ` + groupKey(c, s, "ci", who, a) + ` FROM data_table_cell_index ci WHERE ci.row_id = r.id AND ci.field_id = ` + a.add(c.key) + `::uuid)`
	if c.family == famCheckbox {
		return `COALESCE(` + probe + `, FALSE)`
	}
	return probe
}

// keyValue is a key as a value of its level's kind, as a parameter and its
// cast, written the one way keyOf writes it; false for a key no row can
// have (a word at a number's level). An empty key is NULL.
func keyValue(fam family, key string) (value interface{}, cast, canonical string, ok bool) {
	switch fam {
	case famCheckbox:
		if key != "true" && key != "false" {
			return nil, "", "", false
		}
		return key == "true", "::boolean", key, true
	case famNumber:
		if key == "" {
			return nil, "::numeric", "", true
		}
		n, err := strconv.ParseFloat(key, 64)
		if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
			return nil, "", "", false
		}
		k := numberKey(n)
		return k, "::numeric", k, true
	}
	if key == "" {
		return nil, "::text", "", true
	}
	return key, "::text", key, true
}

// collapsedSQL is the condition that a row is in none of the groups the
// reader collapsed; "" for none. Each level's key down to the deepest
// collapsed group is probed once a row, in a sub-select of its own, and
// compared with each collapsed path as values of its levels' kinds.
func collapsedSQL(groups []query.Group, cols map[string]*column, who asker, a *sqlArgs) string {
	q := &sqlArgs{base: a.base, vals: append([]interface{}{}, a.vals...)}
	var ors []string
	for level, g := range groups {
		paths, _ := g.CollapsedPaths(level) // Validate has read them
	path:
		for _, path := range paths {
			vals, casts := make([]interface{}, len(path)), make([]string, len(path))
			for i, key := range path {
				var ok bool
				if vals[i], casts[i], _, ok = keyValue(cols[groups[i].Field].family, key); !ok {
					continue path
				}
			}
			ands := make([]string, len(path))
			for i := range path {
				ands[i] = `g.k` + strconv.Itoa(i) + ` IS NOT DISTINCT FROM ` + q.add(vals[i]) + casts[i]
			}
			ors = append(ors, "("+strings.Join(ands, " AND ")+")")
		}
	}
	if len(ors) == 0 {
		return ""
	}
	// Every level's key, to the deepest named: a path never runs past it.
	deepest := 0
	for level, g := range groups {
		if len(g.Collapsed) > 0 {
			deepest = level
		}
	}
	keys := make([]string, deepest+1)
	for i := range keys {
		keys[i] = probedKey(cols[groups[i].Field], levelOf(groups[i]), who, q) + ` AS k` + strconv.Itoa(i)
	}
	a.vals = q.vals
	return `NOT EXISTS (SELECT 1 FROM (SELECT ` + strings.Join(keys, ", ") + `) g WHERE ` + strings.Join(ors, " OR ") + `)`
}

// numberKey is a number as a group's key: as few digits as tell it apart,
// never an exponent, and no negative zero, which Postgres's numeric hasn't
// got.
func numberKey(n float64) string {
	if n == 0 {
		n = 0 // -0 is 0
	}
	return strconv.FormatFloat(n, 'f', -1, 64)
}

// keyOf is a row's key at a group level, as the one query reads it from the
// entry the index holds (groupKey), the row's cell read as fam.
func keyOf(c *column, fam family, s sortBy, r *evalRow, zone *time.Location) string {
	e, ok := rowEntry(c, fam, r)
	switch {
	case fam == famCheckbox:
		return strconv.FormatBool(ok && e.B)
	case !ok:
		return ""
	case fam == famChoice:
		if n := int(*e.N); n <= len(c.options) {
			return c.options[n-1]
		}
		return *e.T
	case fam == famNumber:
		return numberKey(*e.N)
	}
	return periodOf(e, s.period(), zone)
}

// periodOf is the first day of the period a date entry is in, where the
// reader is, as Postgres's date_trunc finds it: a day's own date, any other
// moment's in zone; a week from its Monday.
func periodOf(e model.IndexCell, period string, zone *time.Location) string {
	var y int
	var m time.Month
	var d int
	if e.T != nil {
		day, _ := time.Parse(dayLayout, *e.T) // an entry keeps a day only once it parses
		y, m, d = day.Date()
	} else {
		y, m, d = e.D.In(zone).Date()
	}
	switch period {
	case query.GranularityWeek:
		at := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		y, m, d = at.AddDate(0, 0, -((int(at.Weekday()) + 6) % 7)).Date()
	case query.GranularityMonth:
		d = 1
	case query.GranularityQuarter:
		m, d = m-(m-1)%3, 1
	case query.GranularityYear:
		m, d = time.January, 1
	}
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Format(dayLayout)
}

// groupValues is a row's values for a choice's or a date's group level, as
// groupOrder orders by them: nil for none.
func groupValues(c *column, fam family, s sortBy, r *evalRow, who asker) []sortValue {
	if fam == famDate {
		if key := keyOf(c, fam, s, r, who.zone); key != "" {
			return []sortValue{key}
		}
		return []sortValue{nil}
	}
	e, ok := rowEntry(c, fam, r)
	if !ok {
		return []sortValue{nil, nil, nil}
	}
	return []sortValue{*e.N, *e.S, *e.T}
}

// pathOf is a row's keys, one a group level.
func pathOf(groups []query.Group, cols map[string]*column, kinds computedKinds, r *evalRow, zone *time.Location) []string {
	path := make([]string, len(groups))
	for i, g := range groups {
		c := cols[g.Field]
		path[i] = keyOf(c, effective(c, kinds), levelOf(g), r, zone)
	}
	return path
}

// collapsedOf is the reader's collapsed groups as keyOf writes their paths,
// each path's keys joined by NUL (no key holds one: Postgres's text can't).
func collapsedOf(groups []query.Group, cols map[string]*column, kinds computedKinds) map[string]bool {
	out := map[string]bool{}
	for level, g := range groups {
		paths, _ := g.CollapsedPaths(level) // Validate has read them
	path:
		for _, path := range paths {
			keys := make([]string, len(path))
			for i, key := range path {
				c := cols[groups[i].Field]
				_, _, canonical, ok := keyValue(effective(c, kinds), key)
				if !ok {
					continue path
				}
				keys[i] = canonical
			}
			out[strings.Join(keys, "\x00")] = true
		}
	}
	return out
}

// hidden is whether a row whose keys are path is in a collapsed group, at
// any level.
func hidden(path []string, collapsed map[string]bool) bool {
	for i := range path {
		if collapsed[strings.Join(path[:i+1], "\x00")] {
			return true
		}
	}
	return false
}

// foldGroups is the groups of the rows the evaluator kept, in their order (a
// group's rows are together, its keys leading the order), paths their keys:
// every level's, depth first, each with its count and its summaries.
func foldGroups(reqs []query.SummaryRequest, cols map[string]*column, kinds computedKinds, kept []*evalRow, paths [][]string) ([]query.GroupCount, error) {
	var groups []query.GroupCount
	var members [][]*evalRow
	var open []int // the group each level is in, at the row
	for i, path := range paths {
		d := 0
		for i > 0 && d < len(path) && path[d] == paths[i-1][d] {
			d++
		}
		open = open[:d]
		for level := d; level < len(path); level++ {
			if len(groups) == query.MaxGroupCounts {
				return nil, tooManyGroups()
			}
			groups = append(groups, query.GroupCount{Key: path[:level+1]})
			members = append(members, nil)
			open = append(open, len(groups)-1)
		}
		for _, g := range open {
			members[g] = append(members[g], kept[i])
		}
	}
	for i := range groups {
		groups[i].Count = len(members[i])
		if len(reqs) > 0 {
			sums, err := foldSummaries(reqs, cols, kinds, members[i])
			if err != nil {
				return nil, err
			}
			groups[i].Summaries = sums
		}
	}
	return groups, nil
}

// aggregateRead is the one statement that reads a first window's set beside
// it: its summaries, and when the spec groups, every group's count and
// summaries.
type aggregateRead struct {
	selects, grouped string
	scan             func(pgexec.Row) error
	summaries        query.Summaries
	groups           []query.GroupCount
}

// aggregateSQL is the statement for a spec's summaries and groups over the
// rows a where keeps, through a: one row of aggregates, or GROUP BY ROLLUP
// of the levels' keys (each level's entry joined to r once), the whole set's
// row first and then every level's groups, depth first, in their order. A
// column is read as its family: the evaluator answers computed ones.
func aggregateSQL(spec query.Spec, cols map[string]*column, who asker, a *sqlArgs) *aggregateRead {
	read := &aggregateRead{}
	sums := summarySQL(spec.Summaries, cols, a)
	summaries := func() (query.Summaries, error) {
		if len(spec.Summaries) == 0 {
			return nil, nil
		}
		return sums.summaries()
	}
	if len(spec.Groups) == 0 {
		read.selects = strings.Join(sums.selects, ", ")
		read.scan = func(r pgexec.Row) error {
			if err := r.Scan(sums.dest...); err != nil {
				return err
			}
			var err error
			read.summaries, err = summaries()
			return err
		}
		return read
	}
	n := len(spec.Groups)
	keys := make([]string, n)
	var order []string
	dest := make([]interface{}, 0, n+1+len(sums.dest))
	var mask int64
	dest = append(dest, &mask)
	texts := make([]func() string, n)
	for i, g := range spec.Groups {
		c, s := cols[g.Field], levelOf(g)
		m := ""
		if c.col == colNone {
			m = a.joined(c)
		}
		keys[i] = groupKey(c, s, m, who, a)
		dir := "ASC"
		if s.Desc {
			dir = "DESC"
		}
		order = append(order, `GROUPING(`+keys[i]+`) DESC`)
		if c.family == famChoice {
			// A group's entries share their place, natural key and text.
			for _, part := range []string{`MIN(` + m + `.n)`, `MIN(` + m + `.s) COLLATE "C"`, `MIN(` + m + `.t) COLLATE "C"`} {
				order = append(order, part+` `+dir+` NULLS LAST`)
			}
		} else {
			key := sortKey{expr: keys[i]}
			if c.family == famDate {
				key.kind = "text"
			}
			order = append(order, keyExpr(key)+` `+dir+` NULLS LAST`)
		}
		switch c.family {
		case famCheckbox:
			var v sql.NullBool
			dest, texts[i] = append(dest, &v), func() string { return strconv.FormatBool(v.Bool) }
		case famNumber:
			var v sql.NullFloat64
			dest, texts[i] = append(dest, &v), func() string {
				if !v.Valid {
					return ""
				}
				return numberKey(v.Float64)
			}
		default:
			var v sql.NullString
			dest, texts[i] = append(dest, &v), func() string { return v.String }
		}
	}
	dest = append(dest, sums.dest...)
	read.selects = `GROUPING(` + strings.Join(keys, ", ") + `), ` + strings.Join(keys, ", ") + `, ` + strings.Join(sums.selects, ", ")
	read.grouped = `GROUP BY ROLLUP (` + strings.Join(keys, ", ") + `) ORDER BY ` + strings.Join(order, ", ") + ` LIMIT ` + strconv.Itoa(query.MaxGroupCounts+2)
	read.scan = func(r pgexec.Row) error {
		if err := r.Scan(dest...); err != nil {
			return err
		}
		s, err := summaries()
		if err != nil {
			return err
		}
		depth := n - bits.OnesCount64(uint64(mask))
		if depth == 0 {
			read.summaries = s
			return nil
		}
		path := make([]string, depth)
		for i := range path {
			path[i] = texts[i]()
		}
		read.groups = append(read.groups, query.GroupCount{Key: path, Count: int(sums.rows.Int64), Summaries: s})
		return nil
	}
	return read
}

// result is what the statement read (the set's own row comes first, so the
// LIMIT keeps it): past the groups' cap, refused.
func (r *aggregateRead) result() (query.Summaries, []query.GroupCount, error) {
	if len(r.groups) > query.MaxGroupCounts {
		return nil, nil, tooManyGroups()
	}
	return r.summaries, r.groups, nil
}
