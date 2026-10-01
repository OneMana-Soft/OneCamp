package business

// aggregate.go — the deterministic, READ-ONLY aggregation pushdown for external
// data sources. It is the external-DB analog of the native Tables aggregation
// engine: the agent/assistant describes WHAT it wants as a typed AggSpec (group
// by a column, count/sum/avg/min/max another, with filters + a limit) and this
// builds a single, safe, parameterized SQL statement and runs it inside a
// read-only transaction with a statement timeout. The model never writes SQL.
//
// Injection safety has two independent layers:
//   - IDENTIFIERS (schema/table/column names) are validated against the source's
//     live introspected schema — an unknown name is rejected, never sent — and
//     then quoted with pq.QuoteIdentifier.
//   - VALUES are always passed as bound parameters ($1, $2, …), never
//     interpolated, and cast to the target column's type server-side.

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// AggFilter is one row-level predicate pushed to the external DB. Field is a
// column name (validated against the schema).
type AggFilter struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

// AggSpec is the typed, deterministic aggregation to push down. Table may be
// "schema.name" or just "name" (resolved against introspection; ambiguous bare
// names are rejected). Op defaults to count.
type AggSpec struct {
	Table      string      `json:"table"`
	GroupBy    string      `json:"group_by"`
	Op         string      `json:"aggregate"`
	ValueField string      `json:"value_field"`
	Filters    []AggFilter `json:"filters"`
	Limit      int         `json:"limit"`
	Ascending  bool        `json:"ascending"`
}

// AggBucket is one group in the result (Value == Count for a count aggregate).
type AggBucket struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
	Count int     `json:"count"`
}

// AggResult is the pushed-down aggregation outcome, ready to summarize/chart.
type AggResult struct {
	Table       string      `json:"table"`
	GroupBy     string      `json:"group_by,omitempty"`
	GroupByType string      `json:"group_by_type,omitempty"`
	Op          string      `json:"aggregate"`
	ValueField  string      `json:"value_field,omitempty"`
	Buckets     []AggBucket `json:"buckets"`
	Truncated   bool        `json:"truncated"`
}

// bounds for a single external aggregation.
const (
	defaultAggLimit = 50
	maxAggLimit     = 200
	maxAggFilters   = 25
	aggQueryTimeout = 20 // seconds (context)
	aggStatementMS  = 15000
)

var validAggOps = map[string]bool{"count": true, "sum": true, "avg": true, "min": true, "max": true}

var validFilterOps = map[string]bool{
	"eq": true, "ne": true, "contains": true, "gt": true, "gte": true,
	"lt": true, "lte": true, "empty": true, "not_empty": true,
}

// RunAggregate validates the spec against the source's live schema, builds a
// single parameterized read-only SQL statement, and runs it. Errors on an
// unusable spec (unknown table/column, non-numeric measure, bad op) so the
// model gets actionable feedback rather than a hard failure.
func (c *sqlConnector) RunAggregate(ctx context.Context, tables []TableSchema, spec AggSpec) (*AggResult, error) {
	d := c.dialect
	op := strings.ToLower(strings.TrimSpace(spec.Op))
	if op == "" {
		op = "count"
	}
	if !validAggOps[op] {
		return nil, fmt.Errorf("unsupported aggregate %q (use count, sum, avg, min or max)", spec.Op)
	}
	if len(spec.Filters) > maxAggFilters {
		return nil, fmt.Errorf("too many filters (max %d)", maxAggFilters)
	}

	// Resolve + validate the table and its columns against the (caller-supplied,
	// possibly cached) introspected schema — the sole source of legal identifiers.
	tbl, terr := resolveTable(tables, spec.Table)
	if terr != nil {
		return nil, terr
	}
	resolveCol := func(ref string) (string, string, bool) {
		col, ok := findColumn(tbl, ref)
		if !ok {
			return "", "", false
		}
		return col.Name, col.DataType, true
	}

	// Group-by (optional). A date column groups by day so a trend is readable.
	var groupExpr, groupType string
	if strings.TrimSpace(spec.GroupBy) != "" {
		name, dtype, ok := resolveCol(spec.GroupBy)
		if !ok {
			return nil, fmt.Errorf("group-by column %q not found in %s", spec.GroupBy, tbl.Name)
		}
		groupType = dtype
		if dtype == "date" {
			groupExpr = d.DateTruncDay(d.QuoteIdent(name))
		} else {
			groupExpr = d.QuoteIdent(name)
		}
	}

	// Measure column (required for non-count, must be numeric).
	var valueName string
	if op != "count" {
		name, dtype, ok := resolveCol(spec.ValueField)
		if !ok {
			return nil, fmt.Errorf("value column %q not found in %s", spec.ValueField, tbl.Name)
		}
		if dtype != "number" {
			return nil, fmt.Errorf("%s needs a numeric column; %q is %s", op, name, dtype)
		}
		valueName = name
	}

	// Aggregate expression.
	aggExpr := "COUNT(*)"
	if op != "count" {
		aggExpr = strings.ToUpper(op) + "(" + d.QuoteIdent(valueName) + ")"
	}

	// WHERE from filters (bound params; identifiers validated + quoted).
	var where []string
	var args []interface{}
	argN := 1
	for _, f := range spec.Filters {
		fop := strings.ToLower(strings.TrimSpace(f.Op))
		if !validFilterOps[fop] {
			return nil, fmt.Errorf("unsupported filter op %q", f.Op)
		}
		name, dtype, ok := resolveCol(f.Field)
		if !ok {
			return nil, fmt.Errorf("filter column %q not found in %s", f.Field, tbl.Name)
		}
		clause, used := buildWhereClause(d, d.QuoteIdent(name), dtype, fop, f.Value, &argN)
		if clause == "" {
			continue
		}
		if used {
			args = append(args, f.Value)
		}
		where = append(where, clause)
	}

	// Assemble. Table is schema-qualified + quoted.
	from := d.QuoteIdent(tbl.Schema) + "." + d.QuoteIdent(tbl.Name)
	limit := spec.Limit
	if limit <= 0 {
		limit = defaultAggLimit
	}
	if limit > maxAggLimit {
		limit = maxAggLimit
	}

	var sb strings.Builder
	if groupExpr == "" {
		sb.WriteString("SELECT 'Total' AS grp, " + aggExpr + " AS val, COUNT(*) AS cnt FROM " + from)
		if len(where) > 0 {
			sb.WriteString(" WHERE " + strings.Join(where, " AND "))
		}
	} else {
		sb.WriteString("SELECT COALESCE(" + d.CastText(groupExpr) + ", '(empty)') AS grp, " +
			aggExpr + " AS val, COUNT(*) AS cnt FROM " + from)
		if len(where) > 0 {
			sb.WriteString(" WHERE " + strings.Join(where, " AND "))
		}
		sb.WriteString(" GROUP BY 1")
		// A date group-by reads as a chronological trend; everything else by
		// magnitude (biggest first, unless ascending was requested).
		if groupType == "date" {
			sb.WriteString(" ORDER BY 1 ASC")
		} else if spec.Ascending {
			sb.WriteString(" ORDER BY 2 ASC")
		} else {
			sb.WriteString(" ORDER BY 2 DESC")
		}
	}
	sb.WriteString(" LIMIT " + strconv.Itoa(limit+1)) // +1 to detect truncation

	res, rerr := c.execAggregate(ctx, sb.String(), args, limit)
	if rerr != nil {
		return nil, rerr
	}
	res.Table = tbl.Schema + "." + tbl.Name
	res.Op = op
	res.ValueField = valueName
	if groupExpr != "" {
		res.GroupBy = spec.GroupBy
		res.GroupByType = groupType
	}
	return res, nil
}

// execAggregate runs the built statement in a read-only tx with a statement
// timeout and scans buckets, flagging truncation when more than limit groups
// came back (we asked for limit+1).
func (c *sqlConnector) execAggregate(ctx context.Context, query string, args []interface{}, limit int) (*AggResult, error) {
	cctx, cancel := context.WithTimeout(ctx, aggQueryTimeout*1e9)
	defer cancel()

	// Every failure below comes from the external engine, so it is wrapped as an
	// external error: the detail is logged server-side and the client gets the
	// stable sanitized message (see connError.go).
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

	out := &AggResult{Buckets: []AggBucket{}}
	for rows.Next() {
		var label sql.NullString
		var val sql.NullFloat64
		var cnt int64
		if err := rows.Scan(&label, &val, &cnt); err != nil {
			return nil, externalErr(opQuery, err)
		}
		if len(out.Buckets) >= limit {
			out.Truncated = true
			break
		}
		out.Buckets = append(out.Buckets, AggBucket{
			Label: nullStr(label, "(empty)"),
			Value: val.Float64,
			Count: int(cnt),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, externalErr(opQuery, err)
	}
	return out, nil
}

// buildWhereClause returns a single predicate for (col,dtype,op,value) plus
// whether it consumes the next bound parameter. Numeric/date comparisons cast
// the bound (text) parameter to the column's type server-side. empty/not_empty
// consume no parameter.
func buildWhereClause(d Dialect, col, dtype, op, value string, argN *int) (string, bool) {
	place := func() string {
		p := d.Placeholder(*argN)
		*argN++
		return p
	}
	textCol := d.CastText(col)
	switch op {
	case "empty":
		return "(" + col + " IS NULL OR " + textCol + " = '')", false
	case "not_empty":
		return "(" + col + " IS NOT NULL AND " + textCol + " <> '')", false
	case "eq":
		return textCol + " = " + place(), true
	case "ne":
		return "(" + col + " IS NULL OR " + textCol + " <> " + place() + ")", true
	case "contains":
		// case-insensitive substring on the text form (dialect-specific).
		return d.ContainsExpr(textCol, place()), true
	case "gt", "gte", "lt", "lte":
		sym := map[string]string{"gt": ">", "gte": ">=", "lt": "<", "lte": "<="}[op]
		p := place()
		switch dtype {
		case "number":
			p = d.CastNumericParam(p)
		case "date":
			p = d.CastDateParam(p)
		}
		return col + " " + sym + " " + p, true
	default:
		return "", false
	}
}

// resolveTable finds the introspected table matching ref ("schema.name" or
// "name"). A bare name that is ambiguous across schemas is rejected so a query
// can never hit the wrong table.
func resolveTable(tables []TableSchema, ref string) (*TableSchema, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("a table is required")
	}
	if i := strings.Index(ref, "."); i >= 0 {
		schema := strings.ToLower(strings.TrimSpace(ref[:i]))
		name := strings.ToLower(strings.TrimSpace(ref[i+1:]))
		for idx := range tables {
			if strings.ToLower(tables[idx].Schema) == schema && strings.ToLower(tables[idx].Name) == name {
				return &tables[idx], nil
			}
		}
		return nil, fmt.Errorf("table %q not found", ref)
	}
	name := strings.ToLower(ref)
	var match *TableSchema
	count := 0
	for idx := range tables {
		if strings.ToLower(tables[idx].Name) == name {
			match = &tables[idx]
			count++
		}
	}
	if count == 0 {
		return nil, fmt.Errorf("table %q not found", ref)
	}
	if count > 1 {
		return nil, fmt.Errorf("table %q is ambiguous; qualify it as schema.%s", ref, ref)
	}
	return match, nil
}

// findColumn resolves a column by case-insensitive name within a table.
func findColumn(tbl *TableSchema, ref string) (ColumnSchema, bool) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	for _, c := range tbl.Columns {
		if strings.ToLower(c.Name) == ref {
			return c, true
		}
	}
	return ColumnSchema{}, false
}

func nullStr(s sql.NullString, fallback string) string {
	if !s.Valid || strings.TrimSpace(s.String) == "" {
		return fallback
	}
	return s.String
}
