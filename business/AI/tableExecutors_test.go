package business

import (
	"encoding/json"
	"strings"
	"testing"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
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
