package business

// A set's summaries (QUERY_STACK_CONTRACT.md item 8): what a grid's summary
// bar shows under each column, over every row a spec keeps, never over the
// page a reader holds. The one query reads them from the cell index beside
// the first window (summarySQL); the evaluator folds the same entries
// (entryOf, as the index writes them) over the rows it kept (foldSummaries).
// Both fill a totals for each column, which summariesOf writes out, so a
// column sums the same either way.

import (
	"database/sql"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// totals is one column's over a set: how many of its rows have a value, how
// many different values they have, and what a number's or a date's
// functions fold.
type totals struct {
	filled, unique   int
	sum              float64
	min, max, median *float64
	earliest, latest *moment
}

// moment is a date as a summary writes it: a day as its date, any other time
// at UTC.
type moment struct {
	at  time.Time
	day bool
}

func (m moment) MarshalJSON() ([]byte, error) {
	if m.day {
		return json.Marshal(m.at.UTC().Format(dayLayout))
	}
	return json.Marshal(m.at.UTC().Format(time.RFC3339Nano))
}

// earlier is whether a comes before b, a day before the moment of its own
// midnight (as the index sorts days and moments together).
func earlier(a, b moment) bool {
	return a.at.Before(b.at) || (a.at.Equal(b.at) && a.day && !b.day)
}

// later is whether a comes after b, a day again winning a tie.
func later(a, b moment) bool {
	return a.at.After(b.at) || (a.at.Equal(b.at) && a.day && !b.day)
}

// manyValued is whether a column's cell holds a set (tags, people, links):
// its different values are its sets, which come with its groups.
func manyValued(c *column, fam family) bool {
	return c.col == colNone && (fam == famMulti || fam == famPeople || fam == famRefs || fam == famRowLink)
}

// fits is whether fn can be worked out over c, read as fam: count, empty,
// filled and their percentages over any column; unique over a single-valued
// one; the rest over numbers, ticks or dates.
func fits(fn string, c *column, fam family) bool {
	switch fn {
	case "count", "empty", "filled", "pct_empty", "pct_filled":
		return true
	case "unique":
		return !manyValued(c, fam)
	case "sum", "average", "median", "min", "max":
		return fam == famNumber
	case "checked", "unchecked":
		return fam == famCheckbox
	case "earliest", "latest":
		return fam == famDate
	case "range":
		return fam == famNumber || fam == famDate
	}
	return false
}

// checkSummaries is reqs checked against a table's columns: each names a
// field the table has, once, by functions that field's kind can be worked
// out by.
func checkSummaries(reqs []query.SummaryRequest, cols map[string]*column, kinds computedKinds) error {
	asked := map[*column]bool{}
	for _, s := range reqs {
		c := cols[s.Field]
		if c == nil {
			return errNoField(s.Field)
		}
		if asked[c] {
			return cantQuery("%s is summed up twice", c.field.Name)
		}
		asked[c] = true
		for _, fn := range s.Fns {
			if !fits(fn, c, effective(c, kinds)) {
				return cantQuery("%s can't be summed up by %q", c.field.Name, fn)
			}
		}
	}
	return nil
}

// asks is whether a request wants any of fns.
func asks(s query.SummaryRequest, fns ...string) bool {
	for _, fn := range s.Fns {
		for _, w := range fns {
			if fn == w {
				return true
			}
		}
	}
	return false
}

// summariesOf writes reqs out from their columns' totals over a set of rows:
// a count or a percentage of none is 0; a number's or a date's function of
// none is null, as is a sum too big to write.
func summariesOf(reqs []query.SummaryRequest, rows int, by map[string]*totals) (query.Summaries, error) {
	out := make(query.Summaries, len(reqs))
	pct := func(n int) float64 {
		if rows == 0 {
			return 0
		}
		return 100 * float64(n) / float64(rows)
	}
	for _, s := range reqs {
		t := by[s.Field]
		fns := make(map[string]json.RawMessage, len(s.Fns))
		for _, fn := range s.Fns {
			var v interface{}
			switch fn {
			case "count":
				v = rows
			case "empty", "unchecked":
				v = rows - t.filled
			case "filled", "checked":
				v = t.filled
			case "pct_empty":
				v = pct(rows - t.filled)
			case "pct_filled":
				v = pct(t.filled)
			case "unique":
				v = t.unique
			case "sum":
				if t.filled > 0 {
					v = number(t.sum)
				}
			case "average":
				if t.filled > 0 {
					v = number(t.sum / float64(t.filled))
				}
			case "median":
				v = numberOrNull(t.median)
			case "min":
				v = numberOrNull(t.min)
			case "max":
				v = numberOrNull(t.max)
			case "earliest":
				v = momentOrNull(t.earliest)
			case "latest":
				v = momentOrNull(t.latest)
			case "range":
				switch {
				case t.min != nil && t.max != nil:
					v = number(*t.max - *t.min)
				case t.earliest != nil && t.latest != nil:
					v = []moment{*t.earliest, *t.latest}
				}
			}
			b, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			fns[fn] = b
		}
		out[s.Field] = fns
	}
	return out, nil
}

// number is n as JSON writes it: null for a sum past what a float holds.
func number(n float64) interface{} {
	if math.IsInf(n, 0) || math.IsNaN(n) {
		return nil
	}
	return n
}

func numberOrNull(n *float64) interface{} {
	if n == nil {
		return nil
	}
	return number(*n)
}

func momentOrNull(m *moment) interface{} {
	if m == nil {
		return nil
	}
	return *m
}

// rowEntry is a row's cell in c as an index entry would hold it, read as
// fam; false for none. A row's own columns (its number, when it was made or
// changed, and by whom) and a link's rows are read from the row.
func rowEntry(c *column, fam family, r *evalRow) (model.IndexCell, bool) {
	e := model.IndexCell{Field: c.field.Id}
	switch {
	case c.col == colRowNo:
		if r.rowNo == 0 {
			return e, false
		}
		n := float64(r.rowNo)
		e.N = &n
		return e, true
	case c.col == colCreatedAt || c.col == colUpdatedAt:
		at := r.created
		if c.col == colUpdatedAt {
			at = r.updated
		}
		e.D = &at
		return e, true
	case c.col != colNone || fam == famRowLink:
		e.A = idsOf(c, r)
		return e, len(e.A) > 0
	}
	as := *c
	as.family = fam
	return entryOf(&as, r.values[c.key])
}

// folding is one column's totals as the evaluator works them out, a row at
// a time: the different values it has met, and a number column's values
// for its median.
type folding struct {
	totals
	seen map[interface{}]bool
	nums []float64
}

// add folds a row's entry, read as fam, into f; ok false is a row with no
// value there.
func (f *folding) add(fam family, e model.IndexCell, ok bool) {
	if !ok {
		return
	}
	f.filled++
	switch fam {
	case famNumber:
		n := *e.N
		f.sum += n
		if f.min == nil || n < *f.min {
			f.min = &n
		}
		if f.max == nil || n > *f.max {
			f.max = &n
		}
		f.nums = append(f.nums, n)
		f.seen[n] = true
	case famDate:
		// Postgres keeps a time to the microsecond.
		m := moment{at: e.D.UTC().Round(time.Microsecond), day: e.T != nil}
		if f.earliest == nil || earlier(m, *f.earliest) {
			f.earliest = &m
		}
		if f.latest == nil || later(m, *f.latest) {
			f.latest = &m
		}
		f.seen[m] = true
	case famCheckbox:
		f.seen[true] = true
	case famText, famChoice:
		f.seen[*e.T] = true
	default:
		f.seen[strings.Join(e.A, "\x00")] = true
	}
}

// medianOf is the middle of nums, or between the middle two as Postgres's
// percentile_cont(0.5) takes it; nil for none.
func medianOf(nums []float64) *float64 {
	if len(nums) == 0 {
		return nil
	}
	sort.Float64s(nums)
	mid := len(nums) / 2
	m := nums[mid]
	if len(nums)%2 == 0 {
		lo := nums[mid-1]
		m = lo + (m-lo)*0.5
	}
	return &m
}

// foldSummaries is reqs over the rows the evaluator kept.
func foldSummaries(reqs []query.SummaryRequest, cols map[string]*column, kinds computedKinds, kept []*evalRow) (query.Summaries, error) {
	by := make(map[string]*totals, len(reqs))
	for _, s := range reqs {
		c := cols[s.Field]
		fam := effective(c, kinds)
		f := &folding{seen: map[interface{}]bool{}}
		for _, r := range kept {
			e, ok := rowEntry(c, fam, r)
			f.add(fam, e, ok)
		}
		f.unique = len(f.seen)
		f.median = medianOf(f.nums)
		by[s.Field] = &f.totals
	}
	return summariesOf(reqs, len(kept), by)
}

// summaryRead is what the one query selects for a spec's summaries, where
// it scans each into, and how those make the summaries.
type summaryRead struct {
	selects []string
	dest    []interface{}
	rows    sql.NullInt64
	reads   []func() (string, *totals)
	reqs    []query.SummaryRequest
}

// pick selects expr and scans it into v.
func (s *summaryRead) pick(expr string, v interface{}) {
	s.selects = append(s.selects, expr)
	s.dest = append(s.dest, v)
}

// summarySQL is what the one query selects for reqs over the rows a where
// keeps, each column's entry joined to r once through a (as a sort's is). A
// column is read as its family, a computed one never: those the evaluator
// answers (readsComputed).
func summarySQL(reqs []query.SummaryRequest, cols map[string]*column, a *sqlArgs) *summaryRead {
	s := &summaryRead{reqs: reqs}
	s.pick(`COUNT(*)`, &s.rows)
	for _, req := range reqs {
		c := cols[req.Field]
		val, present, ident, day := columnSQL(c, a)
		var filled, unique sql.NullInt64
		var sum, lo, hi, mid sql.NullFloat64
		var dayLo, momentLo, dayHi, momentHi sql.NullTime
		s.pick(`COUNT(*) FILTER (WHERE `+present+`)`, &filled)
		if asks(req, "unique") {
			s.pick(`COUNT(DISTINCT `+ident+`) FILTER (WHERE `+present+`)`, &unique)
		}
		numbers := c.family == famNumber && asks(req, "sum", "average", "min", "max", "range")
		if numbers {
			s.pick(`SUM(`+val+`)`, &sum)
			s.pick(`MIN(`+val+`)`, &lo)
			s.pick(`MAX(`+val+`)`, &hi)
		}
		if c.family == famNumber && asks(req, "median") {
			s.pick(`PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY (`+val+`)::float8)`, &mid)
		}
		dates := c.family == famDate && asks(req, "earliest", "latest", "range")
		if dates {
			s.pick(`MIN(`+val+`) FILTER (WHERE `+day+`)`, &dayLo)
			s.pick(`MIN(`+val+`) FILTER (WHERE NOT `+day+`)`, &momentLo)
			s.pick(`MAX(`+val+`) FILTER (WHERE `+day+`)`, &dayHi)
			s.pick(`MAX(`+val+`) FILTER (WHERE NOT `+day+`)`, &momentHi)
		}
		field := req.Field
		s.reads = append(s.reads, func() (string, *totals) {
			t := &totals{filled: int(filled.Int64), unique: int(unique.Int64)}
			if numbers && sum.Valid {
				t.sum, t.min, t.max = sum.Float64, &lo.Float64, &hi.Float64
			}
			if mid.Valid {
				t.median = &mid.Float64
			}
			if dates {
				t.earliest = edge(dayLo, momentLo, earlier)
				t.latest = edge(dayHi, momentHi, later)
			}
			return field, t
		})
	}
	return s
}

// summaries is what the one query read, written out.
func (s *summaryRead) summaries() (query.Summaries, error) {
	by := make(map[string]*totals, len(s.reads))
	for _, read := range s.reads {
		field, t := read()
		by[field] = t
	}
	return summariesOf(s.reqs, int(s.rows.Int64), by)
}

// edge is the earliest (or latest, by first) of a column's day and moment
// that the one query found, a day winning a tie as the evaluator's does.
func edge(day, other sql.NullTime, first func(a, b moment) bool) *moment {
	var out *moment
	for _, m := range []struct {
		t   sql.NullTime
		day bool
	}{{day, true}, {other, false}} {
		if !m.t.Valid {
			continue
		}
		at := moment{at: m.t.Time.UTC(), day: m.day}
		if out == nil || first(at, *out) {
			out = &at
		}
	}
	return out
}

// columnSQL is c's value in a row of r, as SQL: the value a function folds,
// whether the row has one, what tells two values apart, and whether a date
// is a day. An indexed column reads its entry, joined to r through a.
func columnSQL(c *column, a *sqlArgs) (val, present, ident, day string) {
	day = `FALSE`
	switch c.col {
	case colRowNo:
		val = `NULLIF(r.row_no, 0)::numeric`
	case colCreatedAt:
		val = `r.created_at`
	case colUpdatedAt:
		val = `r.updated_at`
	case colCreatedBy:
		val = `r.created_by`
	case colUpdatedBy:
		val = `COALESCE(r.updated_by, r.created_by)`
	}
	if val != "" {
		ident = val
		if c.family == famDate {
			ident = `ROW(` + val + `, FALSE)`
		}
		return val, val + ` IS NOT NULL`, ident, day
	}
	if c.family == famRowLink {
		side := `from_row`
		if c.reverse {
			side = `to_row`
		}
		return `NULL`, `EXISTS (SELECT 1 FROM data_table_links l WHERE l.field_id = ` + a.add(c.linkField.String()) + `::uuid AND l.` + side + ` = r.id)`, `NULL`, day
	}
	m := a.joined(c)
	switch c.family {
	case famNumber:
		return m + `.n`, m + `.n IS NOT NULL`, m + `.n`, day
	case famDate:
		return m + `.d`, m + `.d IS NOT NULL`, `ROW(` + m + `.d, ` + m + `.t IS NOT NULL)`, m + `.t IS NOT NULL`
	case famCheckbox:
		return m + `.b`, `COALESCE(` + m + `.b, FALSE)`, m + `.b`, day
	case famText, famChoice:
		return m + `.t`, m + `.t IS NOT NULL`, m + `.t`, day
	}
	return m + `.a`, m + `.a IS NOT NULL`, m + `.a`, day
}
