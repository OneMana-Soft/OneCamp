package business

import (
	"encoding/json"
	"strings"
	"testing"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	tableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// These lock the MODEL-FACING output of query_table: the human breakdown plus
// the ready-to-render ```chart block the model is told to echo. The chart is
// built from the real aggregated numbers here (not hand-written by the model),
// so verifying this text is verifying what actually renders.

// extractChartJSON pulls the JSON body out of the single ```chart block in s,
// or "" when there is none.
func extractChartJSON(s string) string {
	i := strings.Index(s, "```chart")
	if i < 0 {
		return ""
	}
	rest := s[i+len("```chart"):]
	rest = strings.TrimLeft(rest, "\n")
	j := strings.Index(rest, "```")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

func TestRenderAggregateResult_CountByGroupIncludesChart(t *testing.T) {
	res := &dataTableBusiness.AggResult{
		Op:           dataTableBusiness.OpCount,
		GroupByLabel: "Stage",
		Buckets: []dataTableBusiness.AggBucket{
			{Label: "Won", Value: 2, Count: 2},
			{Label: "Lost", Value: 1, Count: 1},
		},
		MatchedRows: 3,
	}
	out := renderAggregateResult("Deals", res, dataTableBusiness.QuerySpec{})

	for _, want := range []string{`Table "Deals"`, "count by Stage", "• Won: 2", "• Lost: 1", "```chart"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	// The embedded chart must be valid JSON with the real labels/values.
	body := extractChartJSON(out)
	var spec struct {
		Type   string `json:"type"`
		Labels []string
		Series []struct {
			Name   string
			Values []float64
		}
	}
	if err := json.Unmarshal([]byte(body), &spec); err != nil {
		t.Fatalf("chart block is not valid JSON: %v (%q)", err, body)
	}
	if spec.Type != "bar" {
		t.Errorf("non-date group should be a bar, got %q", spec.Type)
	}
	if len(spec.Labels) != 2 || spec.Labels[0] != "Won" {
		t.Errorf("labels = %v", spec.Labels)
	}
	if len(spec.Series) != 1 || len(spec.Series[0].Values) != 2 || spec.Series[0].Values[0] != 2 {
		t.Errorf("series = %+v", spec.Series)
	}
}

func TestRenderAggregateResult_SumMetricLabel(t *testing.T) {
	res := &dataTableBusiness.AggResult{
		Op:              dataTableBusiness.OpSum,
		GroupByLabel:    "Owner",
		ValueFieldLabel: "Amount",
		Buckets: []dataTableBusiness.AggBucket{
			{Label: "Alice", Value: 100},
			{Label: "Bob", Value: 200},
		},
		MatchedRows: 2,
	}
	out := renderAggregateResult("Deals", res, dataTableBusiness.QuerySpec{})
	if !strings.Contains(out, "sum of Amount by Owner") {
		t.Errorf("expected metric label 'sum of Amount by Owner', got:\n%s", out)
	}
}

func TestRenderAggregateResult_DateGroupIsLine(t *testing.T) {
	res := &dataTableBusiness.AggResult{
		Op:           dataTableBusiness.OpCount,
		GroupByLabel: "Day",
		GroupByType:  "date",
		Buckets: []dataTableBusiness.AggBucket{
			{Label: "2026-03-01", Value: 1},
			{Label: "2026-03-02", Value: 3},
		},
		MatchedRows: 4,
	}
	out := renderAggregateResult("Signups", res, dataTableBusiness.QuerySpec{})
	body := extractChartJSON(out)
	if !strings.Contains(body, `"type":"line"`) {
		t.Errorf("a date group-by must render a line chart, got: %s", body)
	}
}

func TestRenderAggregateResult_SingleTotalHasNoChart(t *testing.T) {
	res := &dataTableBusiness.AggResult{
		Op:      dataTableBusiness.OpAvg,
		Buckets: []dataTableBusiness.AggBucket{{Label: "Total", Value: 42}},
		// no GroupByLabel — a scalar
		MatchedRows: 10,
	}
	out := renderAggregateResult("Deals", res, dataTableBusiness.QuerySpec{})
	if strings.Contains(out, "```chart") {
		t.Errorf("a single Total bucket must not include a chart, got:\n%s", out)
	}
	if !strings.Contains(out, "• Total: 42") {
		t.Errorf("expected the scalar value line, got:\n%s", out)
	}
}

func TestRenderAggregateResult_TruncationNote(t *testing.T) {
	res := &dataTableBusiness.AggResult{
		Op:           dataTableBusiness.OpCount,
		GroupByLabel: "Tag",
		Buckets:      []dataTableBusiness.AggBucket{{Label: "a", Value: 5}, {Label: "b", Value: 3}},
		MatchedRows:  8,
		ScannedRows:  8,
		Truncated:    true,
	}
	out := renderAggregateResult("T", res, dataTableBusiness.QuerySpec{})
	if !strings.Contains(out, "partial scan") {
		t.Errorf("expected a truncation note, got:\n%s", out)
	}
}

func TestRenderAggregateResult_NoRows(t *testing.T) {
	res := &dataTableBusiness.AggResult{Op: dataTableBusiness.OpCount, GroupByLabel: "Stage"}
	out := renderAggregateResult("Deals", res, dataTableBusiness.QuerySpec{})
	if !strings.Contains(out, "No rows matched") || strings.Contains(out, "```chart") {
		t.Errorf("empty result should say no rows and have no chart, got:\n%s", out)
	}
}

// link_table_rows reads its rows as a JSON array of ids, and names what it
// can't read.
func TestRowIDsParam(t *testing.T) {
	ids, err := rowIDsParam(` ["3f0c5c64-1c1f-4b0e-9a55-4a3d6e1f7a10", " 8a7c1e2d-0b3f-4c5d-8e9f-0a1b2c3d4e5f "] `, "add")
	if err != nil || len(ids) != 2 || ids[1].String() != "8a7c1e2d-0b3f-4c5d-8e9f-0a1b2c3d4e5f" {
		t.Errorf("two ids: %v, %v", ids, err)
	}
	for _, raw := range []string{"", "[]", "  "} {
		if ids, err := rowIDsParam(raw, "add"); err != nil || ids != nil {
			t.Errorf("%q: %v, %v", raw, ids, err)
		}
	}
	if _, err := rowIDsParam(`["Acme"]`, "add"); err == nil || !strings.Contains(err.Error(), `"Acme" isn't a row id`) {
		t.Errorf("a name: %v", err)
	}
	if _, err := rowIDsParam(`"3f0c5c64-1c1f-4b0e-9a55-4a3d6e1f7a10"`, "remove"); err == nil || !strings.Contains(err.Error(), "remove must be a JSON array") {
		t.Errorf("not a list: %v", err)
	}
}

// The table tools change only the links a table's own relations make: a
// field showing another table's links is refused, naming where to change
// them, so the table a call names is the one whose links change.
func TestTableToolsChangeOnlyTheirTablesLinks(t *testing.T) {
	vendor := &tableModel.Field{Id: uuid.New(), Name: "Vendor", Type: "relation", Config: `{"relation_target":"table","table_id":"v","inverse":"b"}`}
	back := &tableModel.Field{Id: uuid.New(), Name: "Budget", Type: "relation",
		Config: `{"relation_target":"table","table_id":"budget-table","inverse_of":"` + vendor.Id.String() + `","table_name":"Budget"}`}
	name := &tableModel.Field{Id: uuid.New(), Name: "Name", Type: "text", Config: "{}"}
	fields := []*tableModel.Field{vendor, back, name}
	if err := othersLinks(fields, map[string]bool{vendor.Id.String(): true, name.Id.String(): true}, "then"); err != nil {
		t.Errorf("a table's own relation and a name: %v", err)
	}
	err := othersLinks(fields, map[string]bool{name.Id.String(): true, back.Id.String(): true}, "make the row without it, then link it from there")
	want := `the "Budget" field shows the links the table "Budget" makes to this one; make the row without it, then link it from there with link_table_rows: table_uuid budget-table, field_uuid ` + vendor.Id.String()
	if err == nil || err.Error() != want {
		t.Errorf("another table's links:\n got %v\nwant %s", err, want)
	}
	if err := othersLinks(fields, map[string]bool{uuid.New().String(): true}, "then"); err != nil {
		t.Errorf("a field it doesn't have is left to the business to refuse: %v", err)
	}
}
