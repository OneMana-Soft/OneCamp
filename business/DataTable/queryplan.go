package business

// queryplan.go — a generic, deterministic, INSPECTABLE query plan over ONE
// table's rows. It generalizes the single-metric QuerySpec (aggregate.go) into
// a small pipeline the model can describe and a human can read/edit before it
// runs:
//
//   filter rows → group → compute one OR MORE metrics per group →
//   filter groups (having) → optional share-of-total → sort → limit
//
// Why this exists: the single-op query_table answers "count bugs per assignee".
// Real questions are multi-step — "top 5 assignees by OPEN bugs, with each as a
// % of the total, and their avg age" — which previously fell back to the model
// eyeballing a 25-row sample (where hallucinated numbers happen). A QueryPlan
// expresses that as a typed, deterministic program: the model never writes SQL
// or runs code, and the plan itself is returned as an artifact so the answer is
// explainable and reproducible.
//
// It is intentionally SINGLE-TABLE (no joins): the row model is a JSON cell bag,
// so cross-table joins belong in a future connector/pushdown layer, not here.
// Everything reuses aggregate.go's audited helpers (resolveField, matchFilter,
// cellLabels, cellNumber, accumulator), so there is no second, divergent data
// path. RunPlan is pure (fields+rows+plan → result) for exhaustive unit tests;
// ExecutePlan wraps it with the same permission-checked, bounded row loading as
// AggregateTable.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// PlanMetric is one aggregated measure computed per group. Label is optional; a
// stable default ("count", "sum_amount", …) is derived when omitted so having/
// sort/share can reference it and the result reads cleanly.
type PlanMetric struct {
	Op         AggOp  `json:"aggregate"`
	ValueField string `json:"value_field,omitempty"`
	Label      string `json:"label,omitempty"`
}

// HavingCond filters GROUPS by a computed metric value (e.g. keep groups whose
// count > 10). Op is a numeric comparison; Metric references a metric label.
type HavingCond struct {
	Metric string   `json:"metric"`
	Op     FilterOp `json:"op"`
	Value  float64  `json:"value"`
}

// QueryPlan is the full, inspectable pipeline. Every field is optional except
// Metrics; an empty GroupBy yields a single "Total" group (so the plan also
// subsumes scalar answers). It is JSON-round-trippable so it can be echoed back
// to the user, edited, and re-run to the identical result.
type QueryPlan struct {
	Filters   []Filter     `json:"filters,omitempty"`
	GroupBy   string       `json:"group_by,omitempty"`
	Metrics   []PlanMetric `json:"metrics"`
	Having    []HavingCond `json:"having,omitempty"`
	SortBy    string       `json:"sort_by,omitempty"` // a metric label; default = first metric
	Ascending bool         `json:"ascending,omitempty"`
	Limit     int          `json:"limit,omitempty"`
	ShareOf   string       `json:"share_of,omitempty"` // a metric label to also emit as % of its total
}

// PlanBucket is one group's row in the result: its label, every metric's value,
// the matched row count, and (when ShareOf is set) that metric's share of the
// grand total as a percentage.
type PlanBucket struct {
	Label    string             `json:"label"`
	Metrics  map[string]float64 `json:"metrics"`
	Count    int                `json:"count"`
	SharePct *float64           `json:"share_pct,omitempty"`
}

// PlanResult is the deterministic outcome, ready to summarize/chart. Metrics is
// the ordered list of metric labels (so a renderer keeps column order stable).
type PlanResult struct {
	GroupByLabel   string       `json:"group_by,omitempty"`
	GroupByType    string       `json:"group_by_type,omitempty"`
	Metrics        []string     `json:"metrics"`
	ShareOf        string       `json:"share_of,omitempty"`
	Buckets        []PlanBucket `json:"buckets"`
	MatchedRows    int          `json:"matched_rows"`
	ScannedRows    int          `json:"scanned_rows"`
	DistinctGroups int          `json:"distinct_groups"`
	// Truncated: the answer leaves something out (as AggResult.Truncated).
	Truncated bool `json:"truncated"`
}

// maxPlanMetrics bounds a single plan so it can't fan out into an unbounded
// number of accumulators per group.
const maxPlanMetrics = 10

// metricLabel is the stable identifier for a metric: its explicit Label, else
// "count", else "<op>_<value-field-name>" (lowercased, spaces→_). Used as the
// map key and for having/sort/share references.
func metricLabel(m PlanMetric, valueField *model.Field) string {
	if l := strings.TrimSpace(m.Label); l != "" {
		return l
	}
	op := AggOp(strings.ToLower(strings.TrimSpace(string(m.Op))))
	if op == "" {
		op = OpCount
	}
	if op == OpCount || valueField == nil {
		return string(op)
	}
	return string(op) + "_" + strings.ToLower(strings.ReplaceAll(strings.TrimSpace(valueField.Name), " ", "_"))
}

// RunPlan is the pure engine: validate the plan against the table's fields,
// filter + group + fold every metric, apply having, share, sort, and limit.
// Never errors on data shape (bad cells are skipped); errors only on an
// unusable plan (no metrics, unknown op, missing/mismatched columns).
func RunPlan(fields []*model.Field, rows []*model.Row, plan QueryPlan) (*PlanResult, error) {
	if len(plan.Metrics) == 0 {
		return nil, fmt.Errorf("a plan needs at least one metric (e.g. count, or sum of a number column)")
	}
	if len(plan.Metrics) > maxPlanMetrics {
		return nil, fmt.Errorf("too many metrics (max %d)", maxPlanMetrics)
	}
	if len(plan.Filters) > maxFilters {
		return nil, fmt.Errorf("too many filters (max %d)", maxFilters)
	}

	// Resolve group-by (optional).
	var groupField *model.Field
	if strings.TrimSpace(plan.GroupBy) != "" {
		if groupField = resolveField(fields, plan.GroupBy); groupField == nil {
			return nil, fmt.Errorf("group-by column %q not found", plan.GroupBy)
		}
	}

	// Resolve + label each metric, keeping order and detecting duplicate labels.
	ops := make([]AggOp, len(plan.Metrics))
	valueFields := make([]*model.Field, len(plan.Metrics))
	labels := make([]string, len(plan.Metrics))
	seenLabel := map[string]bool{}
	for i, m := range plan.Metrics {
		op := AggOp(strings.ToLower(strings.TrimSpace(string(m.Op))))
		if op == "" {
			op = OpCount
		}
		if !ValidAggOp(string(op)) {
			return nil, fmt.Errorf("unsupported aggregate %q (use count, sum, avg, min or max)", m.Op)
		}
		var vf *model.Field
		if op != OpCount {
			if strings.TrimSpace(m.ValueField) == "" {
				return nil, fmt.Errorf("%s needs a value_field (a number column to aggregate)", op)
			}
			if vf = resolveField(fields, m.ValueField); vf == nil {
				return nil, fmt.Errorf("value column %q not found", m.ValueField)
			}
		}
		lbl := metricLabel(m, vf)
		if seenLabel[lbl] {
			return nil, fmt.Errorf("duplicate metric label %q — set a distinct \"label\" on each metric", lbl)
		}
		seenLabel[lbl] = true
		ops[i], valueFields[i], labels[i] = op, vf, lbl
	}

	// Fold rows into per-bucket, per-metric accumulators.
	type bucketAgg struct {
		count int
		accs  []*accumulator // one per metric, index-aligned with labels
	}
	buckets := map[string]*bucketAgg{}
	order := []string{}
	matched := 0
	reads := formulaReads(fields, plan.Filters, append([]*model.Field{groupField}, valueFields...)...)
	short := false

	for _, r := range rows {
		values := parseRowValues(r.Values)
		short = short || readsUnfinished(values, reads)

		skip := false
		for _, f := range plan.Filters {
			if !matchFilter(values, fields, f) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		matched++

		var groupLabels []string
		if groupField == nil {
			groupLabels = []string{"Total"}
		} else {
			groupLabels = cellLabels(values[groupField.Id.String()])
			if len(groupLabels) == 0 {
				groupLabels = []string{"(empty)"}
			}
		}

		for _, glbl := range groupLabels {
			b := buckets[glbl]
			if b == nil {
				if len(order) >= maxDistinctGroups {
					continue
				}
				b = &bucketAgg{accs: make([]*accumulator, len(labels))}
				for i := range b.accs {
					b.accs[i] = &accumulator{}
				}
				buckets[glbl] = b
				order = append(order, glbl)
			}
			b.count++
			for i, vf := range valueFields {
				b.accs[i].count++
				if vf != nil {
					if num, ok := cellNumber(values[vf.Id.String()]); ok {
						b.accs[i].addValue(num)
					}
				}
			}
		}
	}

	// Materialize buckets in first-seen order.
	out := make([]PlanBucket, 0, len(order))
	for _, glbl := range order {
		b := buckets[glbl]
		mv := make(map[string]float64, len(labels))
		for i, lbl := range labels {
			mv[lbl] = b.accs[i].result(ops[i])
		}
		out = append(out, PlanBucket{Label: glbl, Metrics: mv, Count: b.count})
	}

	// Having: drop groups whose metric fails the numeric comparison.
	if len(plan.Having) > 0 {
		out = applyHaving(out, plan.Having)
	}
	distinct := len(out)

	// Share-of-total for a chosen metric (computed over the surviving groups).
	if share := strings.TrimSpace(plan.ShareOf); share != "" {
		applyShare(out, share)
	}

	// Sort: chronological for a date group-by, else by the sort metric (default
	// first metric), desc by default, label tiebreak for determinism.
	sortBy := strings.TrimSpace(plan.SortBy)
	if sortBy == "" {
		sortBy = labels[0]
	}
	chronological := groupField != nil && groupField.Type == model.FieldDate
	sortPlanBuckets(out, sortBy, plan.Ascending, chronological)

	// Limit (keep most-recent window for a time series, top-N otherwise).
	truncated := false
	limit := plan.Limit
	if limit <= 0 {
		limit = defaultBucketLimit
	}
	if limit > maxBucketLimit {
		limit = maxBucketLimit
	}
	if len(out) > limit {
		if chronological {
			out = out[len(out)-limit:]
		} else {
			out = out[:limit]
		}
		truncated = true
	}

	res := &PlanResult{
		Metrics:        labels,
		ShareOf:        strings.TrimSpace(plan.ShareOf),
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
	return res, nil
}

// applyHaving keeps only buckets satisfying every having condition (AND). An
// unknown metric label makes the condition a no-op (never silently drops all).
func applyHaving(buckets []PlanBucket, conds []HavingCond) []PlanBucket {
	kept := buckets[:0:0]
	for _, b := range buckets {
		ok := true
		for _, c := range conds {
			v, present := b.Metrics[strings.TrimSpace(c.Metric)]
			if !present {
				continue // unknown metric → no-op
			}
			if !compareNumber(v, FilterOp(strings.ToLower(strings.TrimSpace(string(c.Op)))), c.Value) {
				ok = false
				break
			}
		}
		if ok {
			kept = append(kept, b)
		}
	}
	return kept
}

// compareNumber applies a numeric having comparison. Unknown ops keep the row.
func compareNumber(v float64, op FilterOp, target float64) bool {
	switch op {
	case FilterGt:
		return v > target
	case FilterGte:
		return v >= target
	case FilterLt:
		return v < target
	case FilterLte:
		return v <= target
	case FilterEq:
		return v == target
	case FilterNe:
		return v != target
	default:
		return true
	}
}

// applyShare sets SharePct on each bucket as its share-metric value over the
// grand total of that metric (0 when the total is 0). Mutates in place.
func applyShare(buckets []PlanBucket, metric string) {
	metric = strings.TrimSpace(metric)
	var total float64
	for _, b := range buckets {
		total += b.Metrics[metric]
	}
	for i := range buckets {
		pct := 0.0
		if total != 0 {
			pct = buckets[i].Metrics[metric] / total * 100
		}
		p := pct
		buckets[i].SharePct = &p
	}
}

// sortPlanBuckets orders buckets by a metric (or chronologically by label for a
// time series), with a stable label tiebreak.
func sortPlanBuckets(b []PlanBucket, sortBy string, ascending, byLabel bool) {
	sort.SliceStable(b, func(i, j int) bool {
		if byLabel {
			return b[i].Label < b[j].Label
		}
		vi, vj := b[i].Metrics[sortBy], b[j].Metrics[sortBy]
		if vi != vj {
			if ascending {
				return vi < vj
			}
			return vi > vj
		}
		return b[i].Label < b[j].Label
	})
}

// ExecutePlan loads a table's fields + rows (permission-checked, bounded exactly
// like AggregateTable) and runs RunPlan. It is the entry point the query_plan
// tool calls. Returns the result + the table (for its display name).
func ExecutePlan(ctx context.Context, tableID uuid.UUID, actor Actor, plan QueryPlan) (*PlanResult, *model.DataTable, error) {
	bundle, err := GetBundle(ctx, tableID, actor)
	if err != nil {
		return nil, nil, err
	}

	rows, scannedAll, serr := scanRowsForQuery(ctx, tableID, actor, bundle, plan.Filters)
	if serr != nil {
		return nil, nil, serr
	}

	res, rerr := RunPlan(bundle.Fields, rows, plan)
	if rerr != nil {
		return nil, nil, rerr
	}
	if !scannedAll {
		res.Truncated = true
	}
	return res, bundle.Table, nil
}
