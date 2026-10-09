package business

// AI tool executors for Tables: they let an agent (or the assistant) list
// tables, read a table's structure + rows, and create/update rows. Each runs AS
// the acting user — the DataTable business layer re-checks that user's
// visibility/permission on every call, so an agent can never read or write a
// table its owner couldn't.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	tableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// registerTableExecutors wires the table tools. Called from RegisterToolExecutors.
func registerTableExecutors() {
	ai.RegisterExecutor("list_tables", executeListTables)
	ai.RegisterExecutor("read_table", executeReadTable)
	ai.RegisterExecutor("query_table", executeQueryTable)
	ai.RegisterExecutor("query_plan", executeQueryPlan)
	ai.RegisterExecutor("create_table_row", executeCreateTableRow)
	ai.RegisterExecutor("update_table_row", executeUpdateTableRow)
	ai.RegisterExecutor("link_table_rows", executeLinkTableRows)
}

// tableActor builds a DataTable actor for the acting user, carrying their admin
// flag so the permission model resolves correctly. The context it returns is
// the one to read and write with: when an agent run acts for someone other
// than its sponsor, it also names that person (forAskerToo).
func tableActor(ctx context.Context, userUUID string) (context.Context, dataTableBusiness.Actor, error) {
	userInfo, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return ctx, dataTableBusiness.Actor{}, fmt.Errorf("failed to look up user: %w", err)
	}
	ctx, err = forAskerToo(ctx)
	if err != nil {
		return ctx, dataTableBusiness.Actor{}, err
	}
	return ctx, dataTableBusiness.Actor{
		UserID:  userInfo.UserPostgresInfo.Id,
		IsAdmin: userInfo.UserPostgresInfo.IsAdmin,
	}, nil
}

// executeListTables lists the tables the acting user can see.
func executeListTables(ctx context.Context, _ ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	ctx, actor, err := tableActor(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}
	tables, err := dataTableBusiness.ListTables(ctx, actor)
	if err != nil {
		return "", nil, fmt.Errorf("failed to list tables")
	}
	// Only the tables the asker may view too, when someone else asked.
	visible, err := askerTables(ctx)
	if err != nil {
		return "", nil, err
	}
	if visible != nil {
		kept := tables[:0:0]
		for _, t := range tables {
			if t != nil && visible[t.Id.String()] {
				kept = append(kept, t)
			}
		}
		tables = kept
	}
	if len(tables) == 0 {
		return "You have no tables yet.", nil, nil
	}
	var b strings.Builder
	b.WriteString("Tables you can access:\n")
	for _, t := range tables {
		b.WriteString(fmt.Sprintf("- %s (id: %s, %s)\n", t.Name, t.Id, t.Visibility))
	}
	return strings.TrimSpace(b.String()), nil, nil
}

// executeReadTable returns a table's columns and a sample of its rows so the
// model can understand the schema (field ids) before writing.
func executeReadTable(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	tableID, err := uuid.Parse(strings.TrimSpace(action.Params["table_uuid"]))
	if err != nil {
		return "", nil, fmt.Errorf("a valid table_uuid is required")
	}
	userInfo, uerr := getUserInfoForExecutor(ctx, userUUID)
	if uerr != nil {
		return "", nil, fmt.Errorf("failed to look up user: %w", uerr)
	}
	actor := dataTableBusiness.Actor{
		UserID:  userInfo.UserPostgresInfo.Id,
		IsAdmin: userInfo.UserPostgresInfo.IsAdmin,
	}
	ctx, aerr := forAskerToo(ctx)
	if aerr != nil {
		return "", nil, aerr
	}
	bundle, berr := dataTableBusiness.GetBundle(ctx, tableID, actor)
	if berr != nil {
		if dataTableBusiness.IsForbidden(berr) {
			return "", nil, fmt.Errorf("you don't have access to this table")
		}
		if dataTableBusiness.IsNotFound(berr) {
			return "", nil, fmt.Errorf("table not found")
		}
		return "", nil, fmt.Errorf("failed to read table")
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Table %q (id: %s)\n\nColumns (use the field id as the key in row values):\n", bundle.Table.Name, bundle.Table.Id))
	for _, f := range bundle.Fields {
		b.WriteString(fmt.Sprintf("- %s [%s] id: %s\n", f.Name, f.Type, f.Id))
	}

	const sample = 25
	b.WriteString(fmt.Sprintf("\nRows (%d total, showing up to %d):\n", len(bundle.Rows), sample))
	for i, r := range bundle.Rows {
		if i >= sample {
			break
		}
		b.WriteString(fmt.Sprintf("- row id: %s values: %s\n", r.Id, compactValues(r.Values)))
	}

	// Attach the workspace's agreed definitions/conventions (glossary memory)
	// the user may see, so the model applies corrected, "make it stick" meanings
	// when it goes on to build a query_plan/query_table against this table —
	// closing the loop where a recorded correction keeps shaping the numbers.
	if defs := workspaceDefinitionsBlock(ctx, userInfo); defs != "" {
		b.WriteString("\n\n")
		b.WriteString(defs)
	}
	return strings.TrimSpace(b.String()), nil, nil
}

// maxTableDefinitions bounds how many workspace definitions we attach to a
// read_table result so a large glossary can't crowd out the schema/sample.
const maxTableDefinitions = 12

// workspaceDefinitionsBlock returns the workspace's durable definitions /
// conventions (glossary memory — the corrected, agreed facts a user or agent
// recorded, e.g. "a qualified lead means Stage is SQL or later") that the
// acting user may see, formatted as a compact, high-signal block. Permission
// scoped via ListWorkspaceMemory. Best-effort: returns "" on any error or when
// there are no definitions, so read_table degrades cleanly.
func workspaceDefinitionsBlock(ctx context.Context, userInfo *userModels.UserInfo) string {
	resp, err := ListWorkspaceMemory(ctx, userInfo,
		[]string{memoryModels.KindGlossary},
		[]string{memoryModels.StatusOpen}, maxTableDefinitions, "")
	if err != nil || resp == nil || len(resp.Items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Workspace definitions to apply when querying (agreed conventions — use these exact meanings; do not redefine them):\n")
	wrote := false
	for _, it := range resp.Items {
		line := strings.TrimSpace(it.Content)
		if line == "" {
			continue
		}
		if len(line) > 240 {
			line = line[:240] + "…"
		}
		b.WriteString("- ")
		b.WriteString(line)
		b.WriteString("\n")
		wrote = true
	}
	if !wrote {
		return ""
	}
	return strings.TrimSpace(b.String())
}

// executeQueryTable answers a data question by aggregating a table's rows
// (count/sum/avg/min/max, optionally grouped by a column and filtered) and
// returns both a readable breakdown AND a ready-to-render ```chart block built
// from the REAL aggregated numbers. This is OneCamp's safe "answer straight from
// the data": the model never writes SQL or runs code — it only describes a
// typed QuerySpec, and the deterministic engine computes it over rows the
// acting user is permitted to see. Because the chart is emitted here from the
// actual result (not left to the model to hand-write), the visualization is
// reliable rather than best-effort. Read-only.
func executeQueryTable(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	tableID, err := uuid.Parse(strings.TrimSpace(action.Params["table_uuid"]))
	if err != nil {
		return "", nil, fmt.Errorf("a valid table_uuid is required (use list_tables/read_table to find it)")
	}
	ctx, actor, err := tableActor(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}

	spec := dataTableBusiness.QuerySpec{
		GroupBy:    strings.TrimSpace(action.Params["group_by"]),
		Op:         dataTableBusiness.AggOp(strings.ToLower(strings.TrimSpace(action.Params["aggregate"]))),
		ValueField: strings.TrimSpace(action.Params["value_field"]),
		Ascending:  strings.EqualFold(strings.TrimSpace(action.Params["order"]), "asc"),
		Limit:      parseIntDefault(action.Params["limit"], 0),
	}
	if filters, ferr := parseFiltersParam(action.Params["filters"]); ferr != nil {
		return "", nil, ferr
	} else {
		spec.Filters = filters
	}

	res, table, aerr := dataTableBusiness.AggregateTable(ctx, tableID, actor, spec)
	if aerr != nil {
		if dataTableBusiness.IsForbidden(aerr) {
			return "", nil, fmt.Errorf("you don't have access to this table")
		}
		if dataTableBusiness.IsNotFound(aerr) {
			return "", nil, fmt.Errorf("table not found")
		}
		// A spec problem (bad column / missing value field) is actionable
		// feedback the model should read and correct, not a hard run failure.
		return "Could not run that query: " + aerr.Error() + ". Use read_table to see the exact column names and types, then try again.", nil, nil
	}

	return renderAggregateResult(table.Name, res, spec), nil, nil
}

// executeQueryPlan answers a MULTI-STEP data question by running a typed,
// inspectable query plan over a table (filter → group → one-or-more metrics →
// having → share-of-total → sort → limit). Like query_table it is deterministic
// and read-only — the model describes a plan, never SQL or code, and the engine
// computes it over rows the acting user may see. Unlike query_table it supports
// several metrics at once, group filters, and % of total, so questions such as
// "top 5 owners by won amount, each as a share of the total, with their avg
// deal size" are answered from the real numbers instead of a sampled guess. The
// PLAN is echoed back so the answer is explainable and reproducible.
func executeQueryPlan(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	tableID, err := uuid.Parse(strings.TrimSpace(action.Params["table_uuid"]))
	if err != nil {
		return "", nil, fmt.Errorf("a valid table_uuid is required (use list_tables/read_table to find it)")
	}
	ctx, actor, err := tableActor(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}

	rawPlan := strings.TrimSpace(action.Params["plan"])
	if rawPlan == "" {
		return "", nil, fmt.Errorf("a plan is required: a JSON object like {\"group_by\":\"Stage\",\"metrics\":[{\"aggregate\":\"count\"}],\"limit\":5}")
	}
	var plan dataTableBusiness.QueryPlan
	if jerr := json.Unmarshal([]byte(rawPlan), &plan); jerr != nil {
		return "", nil, fmt.Errorf("plan must be a JSON object with metrics[]: %v", jerr)
	}

	res, table, rerr := dataTableBusiness.ExecutePlan(ctx, tableID, actor, plan)
	if rerr != nil {
		if dataTableBusiness.IsForbidden(rerr) {
			return "", nil, fmt.Errorf("you don't have access to this table")
		}
		if dataTableBusiness.IsNotFound(rerr) {
			return "", nil, fmt.Errorf("table not found")
		}
		// A plan problem (bad column / missing value field) is actionable
		// feedback for the model to correct, not a hard run failure.
		return "Could not run that plan: " + rerr.Error() + ". Use read_table to see the exact column names and types, then try again.", nil, nil
	}
	return renderPlanResult(tableID, table.Name, res, plan), nil, nil
}

// renderPlanResult formats a plan result as a compact multi-metric breakdown, a
// chart built from the real numbers, and an echo of the plan that produced it
// (so the answer is inspectable + reproducible). The echoed envelope carries
// the table_uuid so the FE card can re-run an edited plan against the same
// table via the authenticated /tables/{id}/query-plan endpoint.
func renderPlanResult(tableID uuid.UUID, tableName string, res *dataTableBusiness.PlanResult, plan dataTableBusiness.QueryPlan) string {
	var b strings.Builder
	scope := "overall"
	if res.GroupByLabel != "" {
		scope = "by " + res.GroupByLabel
	}
	b.WriteString(fmt.Sprintf("Table %q — %s (from %d matching row(s)", tableName, scope, res.MatchedRows))
	if res.Truncated {
		b.WriteString(fmt.Sprintf("; note: based on a partial scan of %d rows / top results", res.ScannedRows))
	}
	b.WriteString("):\n")

	if len(res.Buckets) == 0 {
		b.WriteString("No rows matched.\n")
		return strings.TrimSpace(b.String())
	}

	for _, bucket := range res.Buckets {
		parts := make([]string, 0, len(res.Metrics)+1)
		for _, m := range res.Metrics {
			parts = append(parts, fmt.Sprintf("%s %s", m, trimNum(bucket.Metrics[m])))
		}
		if bucket.SharePct != nil {
			parts = append(parts, fmt.Sprintf("%.1f%% of total", *bucket.SharePct))
		}
		b.WriteString(fmt.Sprintf("• %s: %s\n", bucket.Label, strings.Join(parts, ", ")))
	}

	// Chart the primary (sort) metric across groups when there's a shape to show.
	if len(res.Buckets) > 1 && res.GroupByLabel != "" {
		primary := strings.TrimSpace(plan.SortBy)
		if primary == "" && len(res.Metrics) > 0 {
			primary = res.Metrics[0]
		}
		if chart := buildPlanChartBlock(primary+" "+scope, res, primary); chart != "" {
			b.WriteString("\nTo visualize this, include exactly this chart in your reply:\n")
			b.WriteString(chart)
		}
	}

	// Echo the plan (with the table name + id) so the FE renders it as an
	// inspectable, RE-RUNNABLE "Query plan" card: the id lets the card call the
	// authenticated /tables/{id}/query-plan endpoint after a human edits a knob.
	envelope := map[string]interface{}{"table": tableName, "table_uuid": tableID.String(), "plan": plan}
	if planJSON, err := json.Marshal(envelope); err == nil {
		b.WriteString("\nThe plan that produced this (deterministic, re-runnable):\n```queryplan\n")
		b.Write(planJSON)
		b.WriteString("\n```")
	}
	return strings.TrimSpace(b.String())
}

// buildPlanChartBlock renders a ```chart block for one metric across the plan's
// groups (line for a date group-by, bar otherwise), from the real values.
func buildPlanChartBlock(title string, res *dataTableBusiness.PlanResult, metric string) string {
	if metric == "" {
		return ""
	}
	labels := make([]string, 0, len(res.Buckets))
	values := make([]float64, 0, len(res.Buckets))
	for _, bucket := range res.Buckets {
		labels = append(labels, bucket.Label)
		values = append(values, bucket.Metrics[metric])
	}
	chartType := "bar"
	if res.GroupByType == "date" {
		chartType = "line"
	}
	spec := map[string]interface{}{
		"type":   chartType,
		"title":  title,
		"labels": labels,
		"series": []map[string]interface{}{{"name": metric, "values": values}},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	return "```chart\n" + string(raw) + "\n```"
}

// renderAggregateResult formats an aggregation into a compact human breakdown
// plus a ready-to-render chart block built from the real numbers.
func renderAggregateResult(tableName string, res *dataTableBusiness.AggResult, spec dataTableBusiness.QuerySpec) string {
	metric := string(res.Op)
	if res.ValueFieldLabel != "" {
		metric += " of " + res.ValueFieldLabel
	}
	by := ""
	if res.GroupByLabel != "" {
		by = " by " + res.GroupByLabel
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Table %q — %s%s (from %d matching row(s)", tableName, metric, by, res.MatchedRows))
	if res.Truncated {
		b.WriteString(fmt.Sprintf("; note: based on a partial scan of %d rows / top %d groups", res.ScannedRows, len(res.Buckets)))
	}
	b.WriteString("):\n")

	if len(res.Buckets) == 0 {
		b.WriteString("No rows matched.\n")
		return strings.TrimSpace(b.String())
	}

	for _, bucket := range res.Buckets {
		b.WriteString(fmt.Sprintf("• %s: %s\n", bucket.Label, trimNum(bucket.Value)))
	}

	// A single-group ("Total") result is a scalar, not worth a chart.
	if len(res.Buckets) > 1 && res.GroupByLabel != "" {
		if chart := buildAggregateChartBlock(metric+by, res); chart != "" {
			b.WriteString("\nTo visualize this, include exactly this chart in your reply:\n")
			b.WriteString(chart)
		}
	}
	return strings.TrimSpace(b.String())
}

// buildAggregateChartBlock renders a ```chart fenced block (see the FE
// AgentChart renderer) from the aggregation buckets. A date group-by is drawn
// as a line (a trend); everything else as a bar (a breakdown).
func buildAggregateChartBlock(title string, res *dataTableBusiness.AggResult) string {
	labels := make([]string, 0, len(res.Buckets))
	values := make([]float64, 0, len(res.Buckets))
	for _, bucket := range res.Buckets {
		labels = append(labels, bucket.Label)
		values = append(values, bucket.Value)
	}
	chartType := "bar"
	if res.GroupByType == "date" {
		chartType = "line" // a date group-by is a trend over time
	}
	spec := map[string]interface{}{
		"type":   chartType,
		"title":  title,
		"labels": labels,
		"series": []map[string]interface{}{{"name": string(res.Op), "values": values}},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	return "```chart\n" + string(raw) + "\n```"
}

// parseFiltersParam parses the optional filters JSON array of {field,op,value}.
func parseFiltersParam(raw string) ([]dataTableBusiness.Filter, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	var f []dataTableBusiness.Filter
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return nil, fmt.Errorf("filters must be a JSON array of {\"field\":..,\"op\":..,\"value\":..}")
	}
	return f, nil
}

// trimNum formats an aggregate value without trailing zeros.
func trimNum(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// executeCreateTableRow creates a row from a JSON object of field id -> value.
func executeCreateTableRow(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	tableID, err := uuid.Parse(strings.TrimSpace(action.Params["table_uuid"]))
	if err != nil {
		return "", nil, fmt.Errorf("a valid table_uuid is required")
	}
	values, perr := parseValuesParam(action.Params["values"])
	if perr != nil {
		return "", nil, perr
	}
	ctx, actor, err := tableActor(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}
	named := make(map[string]bool, len(values))
	for id := range values {
		named[id] = true
	}
	if err := ownLinks(ctx, tableID, named, actor, "make the row without it, then link it from there"); err != nil {
		return "", nil, err
	}
	row, cerr := dataTableBusiness.CreateRow(ctx, tableID, dataTableBusiness.RowInput{Values: values}, actor)
	if cerr != nil {
		return "", nil, mapTableErr(cerr, "create row")
	}
	return fmt.Sprintf("Created row %s.", row.Id), map[string]string{"row_uuid": row.Id.String()}, nil
}

// executeUpdateTableRow replaces a row's values.
func executeUpdateTableRow(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	tableID, err := uuid.Parse(strings.TrimSpace(action.Params["table_uuid"]))
	if err != nil {
		return "", nil, fmt.Errorf("a valid table_uuid is required")
	}
	rowID, rerr := uuid.Parse(strings.TrimSpace(action.Params["row_uuid"]))
	if rerr != nil {
		return "", nil, fmt.Errorf("a valid row_uuid is required")
	}
	values, perr := parseValuesParam(action.Params["values"])
	if perr != nil {
		return "", nil, perr
	}
	ctx, actor, err := tableActor(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}
	linkCells := linkFieldsNamed(ctx, tableID, values, actor)
	if _, uerr := dataTableBusiness.PatchRow(ctx, tableID, rowID, values, actor); uerr != nil {
		return "", nil, mapTableErr(uerr, "update row")
	}
	if len(linkCells) > 0 {
		return fmt.Sprintf("Updated row %s. Its links in %s are as they were: change them with link_table_rows.", rowID, strings.Join(linkCells, ", ")), nil, nil
	}
	return fmt.Sprintf("Updated row %s.", rowID), nil, nil
}

// linkFieldsNamed is the names of the fields among values that link to a
// table's rows, which a row update leaves as they are.
func linkFieldsNamed(ctx context.Context, tableID uuid.UUID, values map[string]interface{}, actor dataTableBusiness.Actor) []string {
	fields, err := dataTableBusiness.ListTableFields(ctx, tableID, actor)
	if err != nil {
		return nil
	}
	var names []string
	for _, f := range fields {
		var cfg struct {
			Target string `json:"relation_target"`
		}
		_ = json.Unmarshal([]byte(f.Config), &cfg)
		if _, named := values[f.Id.String()]; named && f.Type == "relation" && cfg.Target == "table" {
			names = append(names, fmt.Sprintf("%q", f.Name))
		}
	}
	return names
}

// executeLinkTableRows links a row to rows of the table a field links to,
// and unlinks it from others.
func executeLinkTableRows(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	var ids [3]uuid.UUID
	for i, name := range []string{"table_uuid", "row_uuid", "field_uuid"} {
		id, err := uuid.Parse(strings.TrimSpace(action.Params[name]))
		if err != nil {
			return "", nil, fmt.Errorf("a valid %s is required", name)
		}
		ids[i] = id
	}
	add, err := rowIDsParam(action.Params["add"], "add")
	if err != nil {
		return "", nil, err
	}
	remove, err := rowIDsParam(action.Params["remove"], "remove")
	if err != nil {
		return "", nil, err
	}
	if len(add)+len(remove) == 0 {
		return "", nil, fmt.Errorf("give the ids of rows to add or remove")
	}
	ctx, actor, err := tableActor(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}
	if err := ownLinks(ctx, ids[0], map[string]bool{ids[2].String(): true}, actor, "change them from there, for each linked row,"); err != nil {
		return "", nil, err
	}
	_, done, err := dataTableBusiness.ChangeLinks(ctx, ids[0], ids[1], ids[2], add, remove, actor)
	if err != nil {
		return "", nil, mapTableErr(err, "change the links")
	}
	return fmt.Sprintf("Row %s: %d links made and %d removed.", ids[1], done.Added, done.Removed), nil, nil
}

// ownLinks refuses fields among ids showing the links another table's
// relations make: those links are that table's, and changed from its side.
// The table tools change only links a table's own relations make, so the
// table a call names is the one whose links change, and the one MCP checks
// the caller may change. then says what to do from there.
func ownLinks(ctx context.Context, tableID uuid.UUID, ids map[string]bool, actor dataTableBusiness.Actor, then string) error {
	fields, err := dataTableBusiness.ListTableFields(ctx, tableID, actor)
	if err != nil {
		return mapTableErr(err, "read the table's fields")
	}
	return othersLinks(fields, ids, then)
}

// othersLinks is ownLinks' answer for a table with fields, as readers get
// them.
func othersLinks(fields []*tableModel.Field, ids map[string]bool, then string) error {
	for _, f := range fields {
		if !ids[f.Id.String()] {
			continue
		}
		var cfg struct {
			InverseOf string `json:"inverse_of"`
			Table     string `json:"table_id"`
			TableName string `json:"table_name"`
		}
		_ = json.Unmarshal([]byte(f.Config), &cfg)
		if cfg.InverseOf != "" {
			other := "another table"
			if cfg.TableName != "" {
				other = fmt.Sprintf("the table %q", cfg.TableName)
			}
			return fmt.Errorf("the %q field shows the links %s makes to this one; %s with link_table_rows: table_uuid %s, field_uuid %s",
				f.Name, other, then, cfg.Table, cfg.InverseOf)
		}
	}
	return nil
}

// rowIDsParam reads a JSON array of row ids, given as name.
func rowIDsParam(raw, name string) ([]uuid.UUID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	var items []string
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("%s must be a JSON array of row ids", name)
	}
	out := make([]uuid.UUID, 0, len(items))
	for _, it := range items {
		id, err := uuid.Parse(strings.TrimSpace(it))
		if err != nil {
			return nil, fmt.Errorf("%s: %q isn't a row id", name, it)
		}
		out = append(out, id)
	}
	return out, nil
}

// parseValuesParam parses the agent-provided JSON object string of field id ->
// value. An empty string is treated as no values.
func parseValuesParam(raw string) (map[string]interface{}, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]interface{}{}, nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("values must be a JSON object of field id -> value")
	}
	return m, nil
}

// mapTableErr maps a DataTable business error to a friendly executor error,
// with what was wrong, as the API says it, so an agent can put it right.
func mapTableErr(err error, op string) error {
	if dataTableBusiness.IsForbidden(err) {
		return fmt.Errorf("you don't have access to this table")
	}
	if dataTableBusiness.IsNotFound(err) {
		return fmt.Errorf("table not found")
	}
	return fmt.Errorf("failed to %s: %v", op, err)
}

// compactValues trims a row values blob for compact display in tool output.
func compactValues(values string) string {
	values = strings.TrimSpace(values)
	if values == "" {
		return "{}"
	}
	const max = 300
	if len(values) > max {
		return values[:max] + "…"
	}
	return values
}
