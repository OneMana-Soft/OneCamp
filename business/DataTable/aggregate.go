package business

// aggregate.go — a safe, deterministic aggregation engine over a table's rows.
//
// This is OneCamp's answer to "answer straight from the data": instead of
// letting a model emit raw SQL or run arbitrary code (an unbounded security and
// cost surface), the agent/assistant describes WHAT it wants as a small typed
// QuerySpec — group by a column, aggregate another (count/sum/avg/min/max),
// with optional filters — and this engine computes it in memory over rows the
// acting user is already permitted to see. No query language, no code
// execution, no injection surface: the spec can only ever read and fold the
// user's own typed cells.
//
// The core Aggregate function is pure (fields + rows + spec -> result) so it is
// exhaustively unit-testable without a database; AggregateTable wraps it with
// permission-checked, bounded row loading.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/akashc777/OneCamp/business/DataTable/formula"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// AggOp is a supported aggregation. count needs no value field; the rest fold a
// numeric column.
type AggOp string

const (
	OpCount AggOp = "count"
	OpSum   AggOp = "sum"
	OpAvg   AggOp = "avg"
	OpMin   AggOp = "min"
	OpMax   AggOp = "max"
)

// ValidAggOp reports whether s is a supported op (empty defaults to count).
func ValidAggOp(s string) bool {
	switch AggOp(strings.ToLower(strings.TrimSpace(s))) {
	case OpCount, OpSum, OpAvg, OpMin, OpMax:
		return true
	default:
		return false
	}
}

// FilterOp is a supported row filter comparison.
type FilterOp string

const (
	FilterEq       FilterOp = "eq"
	FilterNe       FilterOp = "ne"
	FilterContains FilterOp = "contains"
	FilterGt       FilterOp = "gt"
	FilterGte      FilterOp = "gte"
	FilterLt       FilterOp = "lt"
	FilterLte      FilterOp = "lte"
	FilterEmpty    FilterOp = "empty"
	FilterNotEmpty FilterOp = "not_empty"
)

// Filter is a single row-level predicate. Field is a column id or name.
type Filter struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

// QuerySpec describes an aggregation. GroupBy/ValueField/Filter.Field may each
// be a column id (uuid) or a case-insensitive column name.
type QuerySpec struct {
	GroupBy    string   `json:"group_by"`
	Op         AggOp    `json:"aggregate"`
	ValueField string   `json:"value_field"`
	Filters    []Filter `json:"filters"`
	Limit      int      `json:"limit"`
	Ascending  bool     `json:"ascending"`
}

// AggBucket is one group in the result. For count, Value == Count.
type AggBucket struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
	Count int     `json:"count"`
}

// AggResult is the full aggregation outcome, ready to summarize or chart.
type AggResult struct {
	Op              AggOp       `json:"aggregate"`
	GroupByLabel    string      `json:"group_by"`
	GroupByType     string      `json:"group_by_type,omitempty"` // the group-by column's field type (e.g. date)
	ValueFieldLabel string      `json:"value_field,omitempty"`
	Buckets         []AggBucket `json:"buckets"`
	MatchedRows     int         `json:"matched_rows"`
	ScannedRows     int         `json:"scanned_rows"`
	DistinctGroups  int         `json:"distinct_groups"`
	// Truncated is true when the answer leaves something out: groups past
	// the limit, rows past the scan cap, or formula values that ran out of
	// working out (formula.Unfinished).
	Truncated bool `json:"truncated"`
}

// Caps keep a single aggregation bounded regardless of the spec.
const (
	defaultBucketLimit = 50
	maxBucketLimit     = 200
	maxDistinctGroups  = 1000 // guard against grouping on a high-cardinality column
	maxFilters         = 25   // a spec can't run an unbounded predicate list per row
)

// resolveField finds a field by id or case-insensitive name. Returns nil when
// ref is empty or unmatched.
func resolveField(fields []*model.Field, ref string) *model.Field {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	lower := strings.ToLower(ref)
	for _, f := range fields {
		if f.Id.String() == ref || strings.ToLower(f.Name) == lower {
			return f
		}
	}
	return nil
}

// parseRowValues decodes a row's JSON values blob to a map. Never errors: an
// unparseable/empty blob yields an empty map so the row simply contributes no
// cells.
func parseRowValues(raw string) map[string]interface{} {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return map[string]interface{}{}
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return map[string]interface{}{}
	}
	return m
}

// cellLabels reduces a cell value to zero or more display labels. Scalars yield
// one label; arrays (multi-select / person / relation) yield one per element so
// grouping on them explodes correctly. Empty/nil yields none.
func cellLabels(v interface{}) []string { return labelsWith(v, formatFloat) }

// labelsWith is cellLabels with numbers written by format.
func labelsWith(v interface{}, format func(float64) string) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		return []string{t}
	case bool:
		return []string{strconv.FormatBool(t)}
	case float64:
		return []string{format(t)}
	case json.Number:
		return []string{t.String()}
	case []interface{}:
		var out []string
		for _, e := range t {
			switch el := e.(type) {
			case string:
				if strings.TrimSpace(el) != "" {
					out = append(out, el)
				}
			case map[string]interface{}:
				if lbl := refLabel(el); lbl != "" {
					out = append(out, lbl)
				}
			case float64:
				out = append(out, format(el))
			case bool:
				out = append(out, strconv.FormatBool(el))
			}
		}
		return out
	case map[string]interface{}:
		if lbl := refLabel(t); lbl != "" {
			return []string{lbl}
		}
		return nil
	default:
		return nil
	}
}

// refLabel extracts a human label from a relation/person ref object, preferring
// label, then name, then value/id.
func refLabel(m map[string]interface{}) string {
	for _, k := range []string{"label", "name", "value", "title", "id"} {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// maxNumberText is the longest text cellNumber reads as a number.
const maxNumberText = 64

// cellNumber attempts to read a numeric value from a cell (number column, or a
// numeric string). Returns (value, true) only on success.
func cellNumber(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f, true
		}
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		// No number people write is longer, and a long cell is copied to
		// find that out.
		if len(t) > maxNumberText {
			return 0, false
		}
		s := strings.TrimSpace(strings.ReplaceAll(t, ",", ""))
		if s == "" {
			return 0, false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// formatFloat writes a number for a label: in full, as people write numbers,
// except from 1e21 up, which is 1e+21 as the web app shows it, and below
// 1e-30. Written in full, one of those takes over 32 characters, and 1e308
// takes 309: a list of them grouped 5,000 rows deep took a minute.
func formatFloat(f float64) string {
	if a := math.Abs(f); a >= 1e21 || (a != 0 && a < 1e-30) {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// firstLabel returns the single label of a cell (for filter comparison), or "".
func firstLabel(v interface{}) string {
	ls := cellLabels(v)
	if len(ls) == 0 {
		return ""
	}
	return strings.Join(ls, ", ")
}

// matchFilter evaluates one filter against a row's decoded values.
func matchFilter(values map[string]interface{}, fields []*model.Field, f Filter) bool {
	fld := resolveField(fields, f.Field)
	if fld == nil {
		return true // unknown filter field is ignored (no-op), never excludes silently on typo
	}
	cell := values[fld.Id.String()]
	op := FilterOp(strings.ToLower(strings.TrimSpace(f.Op)))

	switch op {
	case FilterEmpty:
		return firstLabel(cell) == ""
	case FilterNotEmpty:
		return firstLabel(cell) != ""
	case FilterGt, FilterGte, FilterLt, FilterLte:
		cv, ok1 := cellNumber(cell)
		fv, ok2 := cellNumber(f.Value)
		if !ok1 || !ok2 {
			return false
		}
		switch op {
		case FilterGt:
			return cv > fv
		case FilterGte:
			return cv >= fv
		case FilterLt:
			return cv < fv
		default:
			return cv <= fv
		}
	case FilterContains:
		needle := strings.ToLower(strings.TrimSpace(f.Value))
		for _, l := range cellLabels(cell) {
			if strings.Contains(strings.ToLower(l), needle) {
				return true
			}
		}
		return false
	case FilterNe:
		return !labelEquals(cell, f.Value)
	case FilterEq:
		return labelEquals(cell, f.Value)
	default:
		return true // unknown op: no-op
	}
}

// labelEquals reports whether any label of the cell equals value (case-insensitive).
func labelEquals(cell interface{}, value string) bool {
	want := strings.ToLower(strings.TrimSpace(value))
	for _, l := range cellLabels(cell) {
		if strings.ToLower(strings.TrimSpace(l)) == want {
			return true
		}
	}
	// An empty cell equals an empty target.
	if want == "" && firstLabel(cell) == "" {
		return true
	}
	return false
}

// accumulator folds numeric values for a bucket per the chosen op.
type accumulator struct {
	count     int     // rows (exploded) in the bucket
	sum       float64 // sum of numeric value-field values seen
	numSeen   int     // rows that contributed a numeric value
	min       float64
	max       float64
	hasMinMax bool
}

func (a *accumulator) addValue(v float64) {
	a.sum += v
	a.numSeen++
	if !a.hasMinMax {
		a.min, a.max, a.hasMinMax = v, v, true
		return
	}
	if v < a.min {
		a.min = v
	}
	if v > a.max {
		a.max = v
	}
}

func (a *accumulator) result(op AggOp) float64 {
	switch op {
	case OpCount:
		return float64(a.count)
	case OpSum:
		return a.sum
	case OpAvg:
		if a.numSeen == 0 {
			return 0
		}
		return a.sum / float64(a.numSeen)
	case OpMin:
		if a.hasMinMax {
			return a.min
		}
		return 0
	case OpMax:
		if a.hasMinMax {
			return a.max
		}
		return 0
	default:
		return float64(a.count)
	}
}

// Aggregate is the pure aggregation core. It validates the spec against the
// table's fields, applies filters, groups, and folds — returning a bounded,
// sorted result. It never errors on data shape (bad cells are simply skipped);
// it errors only on an unusable spec (e.g. a value field required by the op is
// missing or not found).
func Aggregate(fields []*model.Field, rows []*model.Row, spec QuerySpec) (*AggResult, error) {
	op := AggOp(strings.ToLower(strings.TrimSpace(string(spec.Op))))
	if op == "" {
		op = OpCount
	}
	if !ValidAggOp(string(op)) {
		return nil, fmt.Errorf("unsupported aggregate %q (use count, sum, avg, min or max)", spec.Op)
	}
	if len(spec.Filters) > maxFilters {
		return nil, fmt.Errorf("too many filters (max %d)", maxFilters)
	}

	var groupField *model.Field
	if strings.TrimSpace(spec.GroupBy) != "" {
		if groupField = resolveField(fields, spec.GroupBy); groupField == nil {
			return nil, fmt.Errorf("group-by column %q not found", spec.GroupBy)
		}
	}

	var valueField *model.Field
	if op != OpCount {
		if strings.TrimSpace(spec.ValueField) == "" {
			return nil, fmt.Errorf("%s needs a value_field (a number column to aggregate)", op)
		}
		if valueField = resolveField(fields, spec.ValueField); valueField == nil {
			return nil, fmt.Errorf("value column %q not found", spec.ValueField)
		}
	}

	buckets := map[string]*accumulator{}
	order := []string{} // first-seen order for stable tie-breaks
	matched := 0
	reads := formulaReads(fields, spec.Filters, groupField, valueField)
	short := false

	for _, r := range rows {
		values := parseRowValues(r.Values)
		short = short || readsUnfinished(values, reads)

		// Row-level filters (AND).
		skip := false
		for _, f := range spec.Filters {
			if !matchFilter(values, fields, f) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		matched++

		// Determine the bucket label(s) for this row.
		var labels []string
		if groupField == nil {
			labels = []string{"Total"}
		} else {
			labels = cellLabels(values[groupField.Id.String()])
			if len(labels) == 0 {
				labels = []string{"(empty)"}
			}
		}

		// Numeric value for this row (for non-count ops).
		var num float64
		var hasNum bool
		if valueField != nil {
			num, hasNum = cellNumber(values[valueField.Id.String()])
		}

		for _, lbl := range labels {
			acc := buckets[lbl]
			if acc == nil {
				if len(order) >= maxDistinctGroups {
					continue // cardinality guard
				}
				acc = &accumulator{}
				buckets[lbl] = acc
				order = append(order, lbl)
			}
			acc.count++
			if hasNum {
				acc.addValue(num)
			}
		}
	}

	// Materialize buckets in first-seen order, then sort.
	out := make([]AggBucket, 0, len(order))
	for _, lbl := range order {
		acc := buckets[lbl]
		out = append(out, AggBucket{Label: lbl, Value: acc.result(op), Count: acc.count})
	}
	// A date group-by is a time series: order it chronologically (labels are
	// ISO "YYYY-MM-DD", so lexical order == chronological) so a trend reads
	// left-to-right, rather than by magnitude. Everything else is a breakdown,
	// ordered by value so the biggest slice leads.
	chronological := groupField != nil && groupField.Type == model.FieldDate
	sortBuckets(out, spec.Ascending, chronological)

	distinct := len(out)

	limit := spec.Limit
	if limit <= 0 {
		limit = defaultBucketLimit
	}
	if limit > maxBucketLimit {
		limit = maxBucketLimit
	}
	truncated := false
	if len(out) > limit {
		if chronological {
			// Keep the MOST RECENT window of a time series, not the earliest.
			out = out[len(out)-limit:]
		} else {
			out = out[:limit]
		}
		truncated = true
	}

	res := &AggResult{
		Op:             op,
		Buckets:        out,
		MatchedRows:    matched,
		ScannedRows:    len(rows),
		DistinctGroups: distinct,
		Truncated:      truncated || short,
	}
	if groupField != nil {
		res.GroupByLabel = groupField.Name
		res.GroupByType = groupField.Type
	}
	if valueField != nil {
		res.ValueFieldLabel = valueField.Name
	}
	return res, nil
}

// sortBuckets orders buckets. When byLabel is set (a time series) it sorts by
// label ascending — ISO dates sort chronologically — so a trend reads
// left-to-right. Otherwise it sorts by Value (desc by default), breaking ties
// by label ascending for determinism.
func sortBuckets(b []AggBucket, ascending bool, byLabel bool) {
	sort.SliceStable(b, func(i, j int) bool {
		if byLabel {
			return b[i].Label < b[j].Label
		}
		if b[i].Value != b[j].Value {
			if ascending {
				return b[i].Value < b[j].Value
			}
			return b[i].Value > b[j].Value
		}
		return b[i].Label < b[j].Label
	})
}

// formulaReads is the formula fields a query reads in each row (filters it,
// groups by or adds up): the cells that can have run out of working out.
func formulaReads(fields []*model.Field, filters []Filter, read ...*model.Field) []string {
	var ids []string
	add := func(fld *model.Field) {
		if fld != nil && fld.Type == model.FieldFormula {
			ids = append(ids, fld.Id.String())
		}
	}
	for _, f := range filters {
		add(resolveField(fields, f.Field))
	}
	for _, fld := range read {
		add(fld)
	}
	return ids
}

// readsUnfinished is whether a row's cells at ids include a formula that ran
// out of working out: an answer that reads it falls short.
func readsUnfinished(values map[string]interface{}, ids []string) bool {
	for _, id := range ids {
		if formula.Unfinished(values[id]) {
			return true
		}
	}
	return false
}

// maxAggregateScanRows bounds how many rows a single aggregation loads, so a
// huge table can't run away with memory or time. Beyond this the result is
// marked as based on a partial scan.
const maxAggregateScanRows = 5000

// isASCIIPrintable reports whether s is pure printable ASCII. We only push a
// filter's value down to the DB when this holds, because Postgres ILIKE case
// folding and Go's strings.ToLower agree for ASCII but can diverge on some
// Unicode — and the pushdown MUST match a superset of the in-memory filter
// (dropping a row the engine would keep would be a wrong answer). Non-ASCII
// values simply aren't pushed; those rows are filtered in memory as before.
func isASCIIPrintable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// pushableLikeTerms extracts the substrings that can be SAFELY pushed to the DB
// as a coarse `values::text ILIKE '%term%'` pre-filter, from the exact typed
// filters the engine will still apply in memory. Only `eq` and `contains` with
// a non-empty, ASCII, resolvable-field value qualify: for those, "a cell label
// equals/contains V" implies "the row's JSONB text contains V", so ILIKE on V
// returns a SUPERSET of the true matches — never fewer. Every other operator
// (ne, numeric comparisons, empty/not_empty), empty values, and unknown fields
// (which the engine treats as a no-op) are left to the in-memory filter, so the
// result is provably identical to a full scan — just reached with fewer rows.
func pushableLikeTerms(fields []*model.Field, filters []Filter) []string {
	var terms []string
	for _, f := range filters {
		op := FilterOp(strings.ToLower(strings.TrimSpace(f.Op)))
		if op != FilterEq && op != FilterContains {
			continue
		}
		v := strings.TrimSpace(f.Value)
		if v == "" || !isASCIIPrintable(v) {
			continue
		}
		rf := resolveField(fields, f.Field)
		if rf == nil {
			continue // unknown field is a no-op in matchFilter; must not narrow
		}
		if rf.Type == model.FieldFormula {
			continue // worked out on read, so not in the stored values to match
		}
		terms = append(terms, v)
	}
	return terms
}

// scanRowsForQuery loads the rows a filter-bearing query needs, bounded by the
// scan cap, and reports whether it reached the end of the (candidate) set. It
// is the single, generic row-loading path shared by AggregateTable and
// ExecutePlan: when the filters yield DB-pushable terms it pages the narrowed
// superset via ListRowsFiltered (so a filtered aggregation over a large table
// completes instead of truncating); otherwise it uses the bundle's first page
// and pages the remainder unfiltered, exactly as before. Either way the caller
// re-applies the exact filters in memory, so results never diverge.
func scanRowsForQuery(ctx context.Context, tableID uuid.UUID, actor Actor, bundle *TableBundle, filters []Filter) ([]*model.Row, bool, error) {
	terms := pushableLikeTerms(bundle.Fields, filters)

	// No safe pushdown: keep the historical path (bundle page + unfiltered paging).
	if len(terms) == 0 {
		rows := bundle.Rows
		if !bundle.RowsTruncated {
			return rows, true, nil
		}
		offset := len(rows)
		for len(rows) < maxAggregateScanRows {
			page, perr := ListRows(ctx, tableID, actor, 500, offset)
			if perr != nil {
				return nil, false, perr
			}
			if len(page) == 0 {
				return rows, true, nil
			}
			rows = append(rows, page...)
			offset += len(page)
		}
		if len(rows) > maxAggregateScanRows {
			rows = rows[:maxAggregateScanRows]
		}
		return rows, false, nil
	}

	// Pushdown path: page only the DB-narrowed candidate superset.
	var rows []*model.Row
	offset := 0
	scannedAll := false
	for len(rows) < maxAggregateScanRows {
		page, perr := ListRowsFiltered(ctx, tableID, actor, terms, 500, offset)
		if perr != nil {
			return nil, false, perr
		}
		if len(page) == 0 {
			scannedAll = true
			break
		}
		rows = append(rows, page...)
		offset += len(page)
	}
	if len(rows) > maxAggregateScanRows {
		rows = rows[:maxAggregateScanRows]
	}
	return rows, scannedAll, nil
}

// AggregateTable loads a table's fields + rows (permission-checked, bounded) and
// runs Aggregate over them. It is the entry point tools call.
func AggregateTable(ctx context.Context, tableID uuid.UUID, actor Actor, spec QuerySpec) (*AggResult, *model.DataTable, error) {
	bundle, err := GetBundle(ctx, tableID, actor)
	if err != nil {
		return nil, nil, err
	}

	rows, scannedAll, serr := scanRowsForQuery(ctx, tableID, actor, bundle, spec.Filters)
	if serr != nil {
		return nil, nil, serr
	}

	res, aerr := Aggregate(bundle.Fields, rows, spec)
	if aerr != nil {
		return nil, nil, aerr
	}
	if !scannedAll {
		res.Truncated = true
	}
	return res, bundle.Table, nil
}
