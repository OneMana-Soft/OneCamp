package business

// queryplan.go — the MULTI-STEP, deterministic, READ-ONLY query-plan pushdown
// for external data sources. It is the external-DB analog of the native Tables
// QueryPlan engine, giving the agent the same analytical depth over a warehouse
// that it has over native tables: filter -> group -> one-or-more metrics ->
// having (filter groups by a computed metric) -> share-of-total (%) -> sort ->
// limit, all expressed as a typed QueryPlan the model describes and a single
// parameterized SQL statement executes read-only. The model never writes SQL.
//
// Same two injection defenses as the single-metric aggregate: identifiers are
// validated against the source's introspected schema then quoted, and filter
// VALUES go through bound parameters. HAVING thresholds are float64 (parsed as
// numbers), so they are formatted as numeric literals — never string-
// interpolated user text.

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// PlanMetric is one aggregated measure per group. Label is optional; a stable
// default ("count", "sum_amount", …) is derived when omitted.
type PlanMetric struct {
	Op         string `json:"aggregate"`
	ValueField string `json:"value_field,omitempty"`
	Label      string `json:"label,omitempty"`
}

// HavingCond filters GROUPS by a computed metric value (e.g. keep groups whose
// count > 10). Op is a numeric comparison; Metric references a metric label.
type HavingCond struct {
	Metric string  `json:"metric"`
	Op     string  `json:"op"`
	Value  float64 `json:"value"`
}

// QueryPlan is the full, inspectable pipeline over one external table. Only
// Metrics is required; an empty GroupBy yields a single "Total" group.
type QueryPlan struct {
	Table     string       `json:"table"`
	Filters   []AggFilter  `json:"filters,omitempty"`
	GroupBy   string       `json:"group_by,omitempty"`
	Metrics   []PlanMetric `json:"metrics"`
	Having    []HavingCond `json:"having,omitempty"`
	SortBy    string       `json:"sort_by,omitempty"`
	Ascending bool         `json:"ascending,omitempty"`
	Limit     int          `json:"limit,omitempty"`
	ShareOf   string       `json:"share_of,omitempty"`
}

// PlanBucket is one group's row: its label, each metric value, matched count,
// and (when ShareOf is set) that metric's share of the grand total as a %.
type PlanBucket struct {
	Label    string             `json:"label"`
	Metrics  map[string]float64 `json:"metrics"`
	Count    int                `json:"count"`
	SharePct *float64           `json:"share_pct,omitempty"`
}

// PlanResult is the deterministic outcome, ready to summarize/chart.
type PlanResult struct {
	Table       string       `json:"table"`
	GroupBy     string       `json:"group_by,omitempty"`
	GroupByType string       `json:"group_by_type,omitempty"`
	Metrics     []string     `json:"metrics"`
	ShareOf     string       `json:"share_of,omitempty"`
	Buckets     []PlanBucket `json:"buckets"`
	Truncated   bool         `json:"truncated"`
}

const maxPlanMetrics = 10

var validHavingOps = map[string]string{"gt": ">", "gte": ">=", "lt": "<", "lte": "<=", "eq": "=", "ne": "<>"}

// planMetricLabel is the stable identifier for a metric: explicit Label, else
// "count", else "<op>_<value-field>".
func planMetricLabel(m PlanMetric, valueField string) string {
	if l := strings.TrimSpace(m.Label); l != "" {
		return l
	}
	op := strings.ToLower(strings.TrimSpace(m.Op))
	if op == "" {
		op = "count"
	}
	if op == "count" || valueField == "" {
		return op
	}
	return op + "_" + strings.ToLower(strings.ReplaceAll(strings.TrimSpace(valueField), " ", "_"))
}

// RunPlan validates the plan against the (caller-supplied) schema and executes a
// single parameterized read-only statement. Errors on an unusable plan (no
// metrics, unknown op/column, non-numeric measure) so the model can correct it.
func (c *sqlConnector) RunPlan(ctx context.Context, tables []TableSchema, plan QueryPlan) (*PlanResult, error) {
	d := c.dialect
	if len(plan.Metrics) == 0 {
		return nil, fmt.Errorf("a plan needs at least one metric (e.g. count, or sum of a number column)")
	}
	if len(plan.Metrics) > maxPlanMetrics {
		return nil, fmt.Errorf("too many metrics (max %d)", maxPlanMetrics)
	}
	if len(plan.Filters) > maxAggFilters {
		return nil, fmt.Errorf("too many filters (max %d)", maxAggFilters)
	}

	tbl, terr := resolveTable(tables, plan.Table)
	if terr != nil {
		return nil, terr
	}

	// Group-by (optional); a date column groups by day for readable trends.
	var groupExpr, groupType string
	if strings.TrimSpace(plan.GroupBy) != "" {
		col, ok := findColumn(tbl, plan.GroupBy)
		if !ok {
			return nil, fmt.Errorf("group-by column %q not found in %s", plan.GroupBy, tbl.Name)
		}
		groupType = col.DataType
		if col.DataType == "date" {
			groupExpr = d.DateTruncDay(d.QuoteIdent(col.Name))
		} else {
			groupExpr = d.QuoteIdent(col.Name)
		}
	}

	// Resolve + label + build the aggregate expression for each metric.
	labels := make([]string, len(plan.Metrics))
	aggExprs := make([]string, len(plan.Metrics))
	labelToAgg := map[string]string{}
	seen := map[string]bool{}
	for i, m := range plan.Metrics {
		op := strings.ToLower(strings.TrimSpace(m.Op))
		if op == "" {
			op = "count"
		}
		if !validAggOps[op] {
			return nil, fmt.Errorf("unsupported aggregate %q (use count, sum, avg, min or max)", m.Op)
		}
		valueName := ""
		if op != "count" {
			col, ok := findColumn(tbl, m.ValueField)
			if !ok {
				return nil, fmt.Errorf("value column %q not found in %s", m.ValueField, tbl.Name)
			}
			if col.DataType != "number" {
				return nil, fmt.Errorf("%s needs a numeric column; %q is %s", op, col.Name, col.DataType)
			}
			valueName = col.Name
		}
		lbl := planMetricLabel(m, valueName)
		if seen[lbl] {
			return nil, fmt.Errorf("duplicate metric label %q — set a distinct label on each metric", lbl)
		}
		seen[lbl] = true
		expr := "COUNT(*)"
		if op != "count" {
			expr = strings.ToUpper(op) + "(" + d.QuoteIdent(valueName) + ")"
		}
		labels[i], aggExprs[i], labelToAgg[lbl] = lbl, expr, expr
	}

	// WHERE (bound params).
	var where []string
	var args []interface{}
	argN := 1
	for _, f := range plan.Filters {
		fop := strings.ToLower(strings.TrimSpace(f.Op))
		if !validFilterOps[fop] {
			return nil, fmt.Errorf("unsupported filter op %q", f.Op)
		}
		col, ok := findColumn(tbl, f.Field)
		if !ok {
			return nil, fmt.Errorf("filter column %q not found in %s", f.Field, tbl.Name)
		}
		clause, used := buildWhereClause(d, d.QuoteIdent(col.Name), col.DataType, fop, f.Value, &argN)
		if clause == "" {
			continue
		}
		if used {
			args = append(args, f.Value)
		}
		where = append(where, clause)
	}

	// HAVING (thresholds are numeric literals; metric resolves to its aggExpr).
	var having []string
	for _, h := range plan.Having {
		sym, ok := validHavingOps[strings.ToLower(strings.TrimSpace(h.Op))]
		if !ok {
			continue // unknown having op → no-op, mirrors the native engine
		}
		aggExpr, ok := labelToAgg[strings.TrimSpace(h.Metric)]
		if !ok {
			continue // unknown metric → no-op
		}
		having = append(having, aggExpr+" "+sym+" "+strconv.FormatFloat(h.Value, 'f', -1, 64))
	}

	// share-of-total: a window SUM over ALL post-having groups (computed before
	// LIMIT), so the % is of the true total even when the result is truncated.
	shareLabel := strings.TrimSpace(plan.ShareOf)
	shareExpr := ""
	if shareLabel != "" {
		if agg, ok := labelToAgg[shareLabel]; ok {
			shareExpr = "(" + agg + ") / NULLIF(SUM(" + agg + ") OVER (), 0) * 100"
		} else {
			shareLabel = "" // referenced an unknown metric; drop it
		}
	}

	from := d.QuoteIdent(tbl.Schema) + "." + d.QuoteIdent(tbl.Name)
	limit := plan.Limit
	if limit <= 0 {
		limit = defaultAggLimit
	}
	if limit > maxAggLimit {
		limit = maxAggLimit
	}

	// SELECT list: grp, cnt, m0..mK, [share].
	var sel []string
	if groupExpr == "" {
		sel = append(sel, "'Total' AS grp")
	} else {
		sel = append(sel, "COALESCE("+d.CastText(groupExpr)+", '(empty)') AS grp")
	}
	sel = append(sel, "COUNT(*) AS cnt")
	for i, e := range aggExprs {
		sel = append(sel, e+" AS m"+strconv.Itoa(i))
	}
	if shareExpr != "" {
		sel = append(sel, shareExpr+" AS share_pct")
	}

	var sb strings.Builder
	sb.WriteString("SELECT " + strings.Join(sel, ", ") + " FROM " + from)
	if len(where) > 0 {
		sb.WriteString(" WHERE " + strings.Join(where, " AND "))
	}
	if groupExpr != "" {
		sb.WriteString(" GROUP BY 1")
	}
	if len(having) > 0 {
		sb.WriteString(" HAVING " + strings.Join(having, " AND "))
	}
	// Sort: chronological for a date group-by, else by the sort metric (default
	// first), desc unless ascending requested.
	if groupExpr != "" {
		if groupType == "date" {
			sb.WriteString(" ORDER BY 1 ASC")
		} else {
			sortLbl := strings.TrimSpace(plan.SortBy)
			if sortLbl == "" {
				sortLbl = labels[0]
			}
			idx := indexOf(labels, sortLbl)
			if idx < 0 {
				idx = 0
			}
			dir := "DESC"
			if plan.Ascending {
				dir = "ASC"
			}
			sb.WriteString(" ORDER BY m" + strconv.Itoa(idx) + " " + dir)
		}
	}
	sb.WriteString(" LIMIT " + strconv.Itoa(limit+1)) // +1 detects truncation

	return c.execPlan(ctx, sb.String(), args, labels, shareLabel != "", limit, plan, tbl, groupType)
}

// execPlan runs the built plan statement read-only and scans the dynamic column
// set (grp, cnt, one column per metric, optional share) into buckets.
func (c *sqlConnector) execPlan(ctx context.Context, query string, args []interface{}, labels []string, hasShare bool, limit int, plan QueryPlan, tbl *TableSchema, groupType string) (*PlanResult, error) {
	cctx, cancel := context.WithTimeout(ctx, aggQueryTimeout*1e9)
	defer cancel()

	// As in execAggregate: engine-side failures are wrapped as external errors so
	// only a stable sanitized message reaches the client.
	tx, err := c.db.BeginTx(cctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, externalErr(opQuery, err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only tx
	if stmt := c.dialect.TimeoutStmt(aggStatementMS); stmt != "" {
		if _, err := tx.ExecContext(cctx, stmt); err != nil {
			return nil, externalErr(opQuery, err)
		}
	}

	rows, err := tx.QueryContext(cctx, query, args...)
	if err != nil {
		return nil, externalErr(opQuery, err)
	}
	defer rows.Close()

	out := &PlanResult{
		Table:   tbl.Schema + "." + tbl.Name,
		Metrics: labels,
		Buckets: []PlanBucket{},
	}
	if strings.TrimSpace(plan.GroupBy) != "" {
		out.GroupBy = plan.GroupBy
		out.GroupByType = groupType
	}
	if strings.TrimSpace(plan.ShareOf) != "" && hasShare {
		out.ShareOf = strings.TrimSpace(plan.ShareOf)
	}

	for rows.Next() {
		var label sql.NullString
		var cnt int64
		metricVals := make([]sql.NullFloat64, len(labels))
		dest := make([]interface{}, 0, len(labels)+3)
		dest = append(dest, &label, &cnt)
		for i := range metricVals {
			dest = append(dest, &metricVals[i])
		}
		var share sql.NullFloat64
		if hasShare {
			dest = append(dest, &share)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, externalErr(opQuery, err)
		}
		if len(out.Buckets) >= limit {
			out.Truncated = true
			break
		}
		mv := make(map[string]float64, len(labels))
		for i, l := range labels {
			mv[l] = metricVals[i].Float64
		}
		b := PlanBucket{Label: nullStr(label, "(empty)"), Metrics: mv, Count: int(cnt)}
		if hasShare && share.Valid {
			s := share.Float64
			b.SharePct = &s
		}
		out.Buckets = append(out.Buckets, b)
	}
	if err := rows.Err(); err != nil {
		return nil, externalErr(opQuery, err)
	}
	return out, nil
}

func indexOf(ss []string, s string) int {
	for i, v := range ss {
		if v == s {
			return i
		}
	}
	return -1
}
