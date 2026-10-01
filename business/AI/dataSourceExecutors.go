package business

// AI tool executors for external Data Sources: they let an agent (or the
// assistant) discover the read-only external databases it may query, inspect
// their schema, and run a typed, deterministic aggregation — the same governed
// shape as the native Tables tools, but pushed down to the external DB. Each
// runs AS the acting user: the DataSource business layer re-checks that user's
// per-source visibility (private = creator+admins, workspace = any member) on
// every call, so an agent can never query a source its owner couldn't. The
// model never writes SQL; it only describes a typed AggSpec.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	dataSourceBusiness "github.com/akashc777/OneCamp/business/DataSource"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// registerDataSourceExecutors wires the data-source tools. Called from
// RegisterToolExecutors alongside the table executors.
func registerDataSourceExecutors() {
	ai.RegisterExecutor("list_data_sources", executeListDataSources)
	ai.RegisterExecutor("read_data_source", executeReadDataSource)
	ai.RegisterExecutor("query_data_source", executeQueryDataSource)
	ai.RegisterExecutor("query_data_source_plan", executeQueryDataSourcePlan)
}

// dataSourceActor builds a DataSource actor for the acting user.
func dataSourceActor(ctx context.Context, userUUID string) (dataSourceBusiness.Actor, error) {
	userInfo, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return dataSourceBusiness.Actor{}, fmt.Errorf("failed to look up user: %w", err)
	}
	return dataSourceBusiness.Actor{
		UserID:  userInfo.UserPostgresInfo.Id,
		IsAdmin: userInfo.UserPostgresInfo.IsAdmin,
	}, nil
}

// executeListDataSources lists the external data sources the acting user may query.
func executeListDataSources(ctx context.Context, _ ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	actor, err := dataSourceActor(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}
	items, lerr := dataSourceBusiness.ListQueryable(ctx, actor)
	if lerr != nil {
		return "", nil, fmt.Errorf("failed to list data sources")
	}
	if len(items) == 0 {
		return "There are no external data sources you can query.", nil, nil
	}
	var b strings.Builder
	b.WriteString("External data sources you can query:\n")
	for _, d := range items {
		b.WriteString(fmt.Sprintf("- %s (id: %s, engine: %s)\n", d.Name, d.Id, d.Engine))
	}
	b.WriteString("\nUse read_data_source with an id to see its tables and columns.")
	return strings.TrimSpace(b.String()), nil, nil
}

// executeReadDataSource introspects a source's tables + columns so the model
// knows the exact table/column names before building a query.
func executeReadDataSource(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	id, err := uuid.Parse(strings.TrimSpace(action.Params["data_source_uuid"]))
	if err != nil {
		return "", nil, fmt.Errorf("a valid data_source_uuid is required (use list_data_sources to find it)")
	}
	actor, aerr := dataSourceActor(ctx, userUUID)
	if aerr != nil {
		return "", nil, aerr
	}
	tables, ierr := dataSourceBusiness.Introspect(ctx, id, actor)
	if ierr != nil {
		return "", nil, mapDataSourceErr(ierr)
	}
	if len(tables) == 0 {
		return "This data source exposes no tables.", nil, nil
	}

	const maxShow = 60
	var b strings.Builder
	b.WriteString("Tables in this data source (reference them as schema.table):\n")
	for i, t := range tables {
		if i >= maxShow {
			b.WriteString(fmt.Sprintf("… and %d more tables\n", len(tables)-maxShow))
			break
		}
		cols := make([]string, 0, len(t.Columns))
		for _, c := range t.Columns {
			cols = append(cols, fmt.Sprintf("%s [%s]", c.Name, c.DataType))
		}
		b.WriteString(fmt.Sprintf("- %s.%s: %s\n", t.Schema, t.Name, strings.Join(cols, ", ")))
	}
	return strings.TrimSpace(b.String()), nil, nil
}

// executeQueryDataSource runs a typed, deterministic aggregation against an
// external source and returns a readable breakdown plus a chart built from the
// real numbers. Read-only, permission-scoped, no SQL from the model.
func executeQueryDataSource(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	id, err := uuid.Parse(strings.TrimSpace(action.Params["data_source_uuid"]))
	if err != nil {
		return "", nil, fmt.Errorf("a valid data_source_uuid is required (use list_data_sources to find it)")
	}
	table := strings.TrimSpace(action.Params["table"])
	if table == "" {
		return "", nil, fmt.Errorf("a table is required (use read_data_source to see available tables)")
	}
	actor, aerr := dataSourceActor(ctx, userUUID)
	if aerr != nil {
		return "", nil, aerr
	}

	spec := dataSourceBusiness.AggSpec{
		Table:      table,
		GroupBy:    strings.TrimSpace(action.Params["group_by"]),
		Op:         strings.ToLower(strings.TrimSpace(action.Params["aggregate"])),
		ValueField: strings.TrimSpace(action.Params["value_field"]),
		Ascending:  strings.EqualFold(strings.TrimSpace(action.Params["order"]), "asc"),
		Limit:      parseIntDefault(action.Params["limit"], 0),
	}
	if raw := strings.TrimSpace(action.Params["filters"]); raw != "" && raw != "[]" {
		var f []dataSourceBusiness.AggFilter
		if jerr := json.Unmarshal([]byte(raw), &f); jerr != nil {
			return "", nil, fmt.Errorf("filters must be a JSON array of {\"field\":..,\"op\":..,\"value\":..}")
		}
		spec.Filters = f
	}

	res, sourceName, rerr := dataSourceBusiness.RunAggregate(ctx, id, spec, actor)
	if rerr != nil {
		if dataSourceBusiness.IsForbidden(rerr) || dataSourceBusiness.IsNotFound(rerr) {
			return "", nil, mapDataSourceErr(rerr)
		}
		// A spec/schema problem is actionable feedback for the model to correct.
		return "Could not run that query: " + rerr.Error() + ". Use read_data_source to see the exact table and column names, then try again.", nil, nil
	}
	return renderDataSourceResult(sourceName, res), nil, nil
}

// renderDataSourceResult formats the external aggregation as a compact
// breakdown plus a ready-to-render chart from the real numbers.
func renderDataSourceResult(sourceName string, res *dataSourceBusiness.AggResult) string {
	metric := res.Op
	if res.ValueField != "" {
		metric += " of " + res.ValueField
	}
	by := ""
	if res.GroupBy != "" {
		by = " by " + res.GroupBy
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Data source %q — table %s — %s%s", sourceName, res.Table, metric, by))
	if res.Truncated {
		b.WriteString(" (top results shown)")
	}
	b.WriteString(":\n")
	if len(res.Buckets) == 0 {
		b.WriteString("No rows matched.\n")
		return strings.TrimSpace(b.String())
	}
	for _, bucket := range res.Buckets {
		b.WriteString(fmt.Sprintf("• %s: %s\n", bucket.Label, trimNum(bucket.Value)))
	}

	if len(res.Buckets) > 1 && res.GroupBy != "" {
		if chart := buildDataSourceChartBlock(metric+by, res); chart != "" {
			b.WriteString("\nTo visualize this, include exactly this chart in your reply:\n")
			b.WriteString(chart)
		}
	}
	return strings.TrimSpace(b.String())
}

// buildDataSourceChartBlock renders a chart block from the external aggregation
// (line for a date group-by, bar otherwise), from the real values.
func buildDataSourceChartBlock(title string, res *dataSourceBusiness.AggResult) string {
	labels := make([]string, 0, len(res.Buckets))
	values := make([]float64, 0, len(res.Buckets))
	for _, bucket := range res.Buckets {
		labels = append(labels, bucket.Label)
		values = append(values, bucket.Value)
	}
	chartType := "bar"
	if res.GroupByType == "date" {
		chartType = "line"
	}
	spec := map[string]interface{}{
		"type":   chartType,
		"title":  title,
		"labels": labels,
		"series": []map[string]interface{}{{"name": res.Op, "values": values}},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	return "```chart\n" + string(raw) + "\n```"
}

// executeQueryDataSourcePlan runs a MULTI-STEP query plan against an external
// source (several metrics, having, share-of-total, sort, limit) and returns a
// readable breakdown plus a chart of the primary metric. Read-only,
// permission-scoped; the model describes a typed plan, never SQL.
func executeQueryDataSourcePlan(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	id, err := uuid.Parse(strings.TrimSpace(action.Params["data_source_uuid"]))
	if err != nil {
		return "", nil, fmt.Errorf("a valid data_source_uuid is required (use list_data_sources to find it)")
	}
	actor, aerr := dataSourceActor(ctx, userUUID)
	if aerr != nil {
		return "", nil, aerr
	}
	rawPlan := strings.TrimSpace(action.Params["plan"])
	if rawPlan == "" {
		return "", nil, fmt.Errorf("a plan is required: a JSON object like {\"table\":\"schema.deals\",\"group_by\":\"stage\",\"metrics\":[{\"aggregate\":\"count\"}],\"limit\":5}")
	}
	var plan dataSourceBusiness.QueryPlan
	if jerr := json.Unmarshal([]byte(rawPlan), &plan); jerr != nil {
		return "", nil, fmt.Errorf("plan must be a JSON object with a table and metrics[]: %v", jerr)
	}

	res, sourceName, rerr := dataSourceBusiness.RunPlan(ctx, id, plan, actor)
	if rerr != nil {
		if dataSourceBusiness.IsForbidden(rerr) || dataSourceBusiness.IsNotFound(rerr) {
			return "", nil, mapDataSourceErr(rerr)
		}
		return "Could not run that plan: " + rerr.Error() + ". Use read_data_source to see the exact table and column names, then try again.", nil, nil
	}
	return renderDataSourcePlanResult(id, sourceName, res, plan), nil, nil
}

// renderDataSourcePlanResult formats a multi-metric plan result as a compact
// breakdown, a chart of the primary (sort) metric from the real numbers, and an
// echo of the plan (with the data source id) so the FE renders it as an
// inspectable, RE-RUNNABLE "Query plan" card — the same steerable UX as native
// tables, but backed by /data-sources/{id}/query-plan.
func renderDataSourcePlanResult(id uuid.UUID, sourceName string, res *dataSourceBusiness.PlanResult, plan dataSourceBusiness.QueryPlan) string {
	var b strings.Builder
	scope := "overall"
	if res.GroupBy != "" {
		scope = "by " + res.GroupBy
	}
	b.WriteString(fmt.Sprintf("Data source %q — table %s — %s", sourceName, res.Table, scope))
	if res.Truncated {
		b.WriteString(" (top results shown)")
	}
	b.WriteString(":\n")
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

	if len(res.Buckets) > 1 && res.GroupBy != "" {
		primary := strings.TrimSpace(plan.SortBy)
		if primary == "" && len(res.Metrics) > 0 {
			primary = res.Metrics[0]
		}
		if chart := buildDataSourcePlanChartBlock(primary+" "+scope, res, primary); chart != "" {
			b.WriteString("\nTo visualize this, include exactly this chart in your reply:\n")
			b.WriteString(chart)
		}
	}

	// Echo the plan (with the data source id + name) so the FE renders it as an
	// inspectable, re-runnable card that re-runs against /data-sources/{id}/query-plan.
	envelope := map[string]interface{}{"data_source_uuid": id.String(), "source_name": sourceName, "plan": plan}
	if planJSON, err := json.Marshal(envelope); err == nil {
		b.WriteString("\nThe plan that produced this (deterministic, re-runnable):\n```queryplan\n")
		b.Write(planJSON)
		b.WriteString("\n```")
	}
	return strings.TrimSpace(b.String())
}

// buildDataSourcePlanChartBlock charts one metric across the plan's groups.
func buildDataSourcePlanChartBlock(title string, res *dataSourceBusiness.PlanResult, metric string) string {
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

// mapDataSourceErr maps a DataSource business error to a friendly executor error.
func mapDataSourceErr(err error) error {
	if dataSourceBusiness.IsForbidden(err) {
		return fmt.Errorf("you don't have access to this data source")
	}
	if dataSourceBusiness.IsNotFound(err) {
		return fmt.Errorf("data source not found")
	}
	return fmt.Errorf("failed to read data source")
}
