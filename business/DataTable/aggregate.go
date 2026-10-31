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
	"sort"
	"strings"
	"time"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	formula "github.com/akashc777/OneCamp/business/Formula"
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
	// at is how its filters read (rowFilters.go); its zone is also where a
	// time a row was made or changed falls on a day, grouped by (dayOf).
	at reading
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
	// the limit, rows past the scan cap, formula or rollup values that
	// weren't worked out (formula.Unfinished), or links past those a cell
	// shows (readsShort).
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

// cellNumber attempts to read a numeric value from a cell (number column, or a
// numeric string, read as formula.ParseNumber reads one in en-US's signs).
// Returns (value, true) only on success. Text was read by dropping its
// commas, so "1,5" summed as 15, and "NaN" as a number no answer could hold.
// Totals and formulas read cells so; a filter reads them as the grid does
// (rowFilters.go), where a number written as text is no number.
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
		return formula.ParseNumber(t, formula.PlainSigns)
	}
	return 0, false
}

// isDateField is whether a field's cells are dates: a date, a rollup's
// earliest or latest, or when a row was made or last changed.
func isDateField(f *model.Field) bool {
	return f != nil && kindOfField(f) == formula.KindDate
}

// dayOf is a date cell's label as the day it falls on, "2006-01-02": a date
// as it is, a time with its zone (when a row was made or changed, RFC 3339)
// as its day in loc, and one written without a zone as its own day. Anything
// else is given back as it is.
func dayOf(label string, loc *time.Location) string {
	label = strings.TrimSpace(label)
	if t, err := time.Parse(time.RFC3339, label); err == nil {
		if loc == nil {
			loc = time.UTC
		}
		return t.In(loc).Format(time.DateOnly)
	}
	if len(label) > len(time.DateOnly) && label[len(time.DateOnly)] == 'T' {
		label = label[:len(time.DateOnly)]
	}
	if _, err := time.Parse(time.DateOnly, label); err == nil {
		return label
	}
	return label
}

// groupLabels is the groups a row's cell puts it in, grouped by field: its
// labels, a date's as the day it falls on in loc, or "(empty)".
func groupLabels(field *model.Field, cell interface{}, loc *time.Location) []string {
	labels := fieldTypes.Labels(cell)
	if isDateField(field) {
		for i, l := range labels {
			labels[i] = dayOf(l, loc)
		}
	}
	if len(labels) == 0 {
		return []string{"(empty)"}
	}
	return labels
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
	filters, err := newRowFilters(fields, spec.Filters, spec.at)
	if err != nil {
		return nil, err
	}

	buckets := map[string]*accumulator{}
	order := []string{} // first-seen order for stable tie-breaks
	matched := 0
	reads := computedReads(fields, spec.Filters, groupField, valueField)
	short := false

	for _, r := range rows {
		values := parseRowValues(r.Values)
		short = short || readsShort(values, reads)

		// Row-level filters (AND).
		if ok, err := filters.meets(r, values); err != nil {
			return nil, err
		} else if !ok {
			continue
		}
		matched++

		// Determine the bucket label(s) for this row.
		var labels []string
		if groupField == nil {
			labels = []string{"Total"}
		} else {
			labels = groupLabels(groupField, values[groupField.Id.String()], spec.at.zone)
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
	chronological := isDateField(groupField)
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
		res.GroupByType = groupByType(groupField)
	}
	if valueField != nil {
		res.ValueFieldLabel = valueField.Name
	}
	return res, nil
}

// groupByType is a group-by column's type as a result names it: "date" for
// any whose cells are dates (a row's times among them), which a chart draws
// as a trend, and the field's own type for the rest.
func groupByType(f *model.Field) string {
	if isDateField(f) {
		return model.FieldDate
	}
	return f.Type
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

// computedReads is the fields a query reads in each row (filters it, groups
// by or adds up) whose cells each read works out (workedOutOnRead): formulas,
// rollups, links to tables' rows or to items, named for the reader, and what
// is read from the row itself.
func computedReads(fields []*model.Field, filters []Filter, read ...*model.Field) []string {
	var ids []string
	add := func(fld *model.Field) {
		if workedOutOnRead(fld) {
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

// readsShort is whether a row's cells at ids leave something out: a formula
// or a rollup that wasn't worked out (formula.Unfinished), or a link cell
// with more links than it shows. An answer that reads one falls short.
func readsShort(values map[string]interface{}, ids []string) bool {
	for _, id := range ids {
		if formula.Unfinished(values[id]) || fieldTypes.HasMoreRef(values[id]) {
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

// queryPage is how many rows a query reads at a time.
const queryPage = 500

// tableForQuery is the table a query reads, which the actor must be able to
// view, and its fields.
func tableForQuery(ctx context.Context, tableID uuid.UUID, actor Actor) (*model.DataTable, []*model.Field, error) {
	t, err := loadViewable(ctx, tableID, actor)
	if err != nil {
		return nil, nil, err
	}
	fields, err := model.ListFields(ctx, tableID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load fields")
	}
	return t, fields, nil
}

// scanRowsForQuery loads the rows a query reads, a page at a time up to the
// scan cap, and reports whether it reached the end of them. Each page has
// only the computed cells the query reads worked out (read, by id, and what
// those read). It is the one row-loading path behind AggregateTable and
// ExecutePlan: given terms the database can match (rowFilters.terms), it
// pages only the rows holding them, a superset of the matches, so a filtered
// query over a large table completes instead of stopping short. The caller
// applies the exact filters either way, so the answer is the same as a full
// scan's.
func scanRowsForQuery(ctx context.Context, tableID uuid.UUID, actor Actor, fields []*model.Field, terms, read []string) ([]*model.Row, bool, error) {
	ctx = asViewer(ctx, actor)
	var rows []*model.Row
	for len(rows) < maxAggregateScanRows {
		var page []*model.Row
		var err error
		if len(terms) == 0 {
			page, err = model.ListRows(ctx, tableID, queryPage, len(rows))
		} else {
			page, err = model.ListRowsFiltered(ctx, tableID, terms, queryPage, len(rows))
		}
		if err != nil {
			return nil, false, fmt.Errorf("failed to load rows")
		}
		withComputedFor(ctx, fields, page, read)
		rows = append(rows, page...)
		if len(page) < queryPage {
			return rows, true, nil
		}
	}
	return rows[:maxAggregateScanRows], false, nil
}

// AggregateTable loads a table's fields + rows (permission-checked, bounded) and
// runs Aggregate over them. It is the entry point tools call.
func AggregateTable(ctx context.Context, tableID uuid.UUID, actor Actor, spec QuerySpec) (*AggResult, *model.DataTable, error) {
	t, fields, err := tableForQuery(ctx, tableID, actor)
	if err != nil {
		return nil, nil, err
	}
	// Filters it can't read are refused before any row is.
	spec.at = readingFor(ctx, actor, fields, spec.Filters)
	filters, err := newRowFilters(fields, spec.Filters, spec.at)
	if err != nil {
		return nil, nil, err
	}
	read := computedReads(fields, spec.Filters, resolveField(fields, spec.GroupBy), resolveField(fields, spec.ValueField))
	rows, scannedAll, serr := scanRowsForQuery(ctx, tableID, actor, fields, filters.terms(), read)
	if serr != nil {
		return nil, nil, serr
	}

	res, aerr := Aggregate(fields, rows, spec)
	if aerr != nil {
		return nil, nil, aerr
	}
	if !scannedAll {
		res.Truncated = true
	}
	return res, t, nil
}
