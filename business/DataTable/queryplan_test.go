package business

import (
	"testing"
)

// planBucket finds a bucket by label in a PlanResult.
func planBucket(res *PlanResult, label string) (PlanBucket, bool) {
	for _, b := range res.Buckets {
		if b.Label == label {
			return b, true
		}
	}
	return PlanBucket{}, false
}

// A plan with several metrics computes each one per group in one pass.
func TestRunPlan_MultiMetric(t *testing.T) {
	fields, rows, _ := buildDeals()
	res, err := RunPlan(fields, rows, QueryPlan{
		GroupBy: "Stage",
		Metrics: []PlanMetric{
			{Op: OpCount},
			{Op: OpSum, ValueField: "Amount"},
			{Op: OpAvg, ValueField: "Amount"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Metrics; len(got) != 3 || got[0] != "count" || got[1] != "sum_amount" || got[2] != "avg_amount" {
		t.Fatalf("metric labels = %v", got)
	}
	won, ok := planBucket(res, "Won")
	if !ok {
		t.Fatal("no Won bucket")
	}
	if won.Metrics["count"] != 2 || won.Metrics["sum_amount"] != 300 || won.Metrics["avg_amount"] != 150 {
		t.Errorf("Won metrics = %+v, want count2/sum300/avg150", won.Metrics)
	}
}

// Having filters GROUPS by a computed metric value.
func TestRunPlan_Having(t *testing.T) {
	fields, rows, _ := buildDeals()
	res, err := RunPlan(fields, rows, QueryPlan{
		GroupBy: "Stage",
		Metrics: []PlanMetric{{Op: OpCount}},
		Having:  []HavingCond{{Metric: "count", Op: FilterGte, Value: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Only "Won" has count >= 2; Lost/Open have 1 each.
	if len(res.Buckets) != 1 || res.Buckets[0].Label != "Won" {
		t.Fatalf("having gte 2 → %+v, want only Won", res.Buckets)
	}
}

// ShareOf emits each group's percentage of the metric total.
func TestRunPlan_ShareOfTotal(t *testing.T) {
	fields, rows, _ := buildDeals()
	res, err := RunPlan(fields, rows, QueryPlan{
		GroupBy: "Stage",
		Metrics: []PlanMetric{{Op: OpSum, ValueField: "Amount"}},
		ShareOf: "sum_amount",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Total amount = 100+200+50+300 = 650. Open = 300 → ~46.15%.
	open, ok := planBucket(res, "Open")
	if !ok || open.SharePct == nil {
		t.Fatal("no Open bucket / share not set")
	}
	if got := *open.SharePct; got < 46.1 || got > 46.2 {
		t.Errorf("Open share = %.2f, want ~46.15", got)
	}
}

// Sort by a chosen metric orders the groups; limit keeps the top-N.
func TestRunPlan_SortByMetricAndLimit(t *testing.T) {
	fields, rows, _ := buildDeals()
	res, err := RunPlan(fields, rows, QueryPlan{
		GroupBy: "Stage",
		Metrics: []PlanMetric{{Op: OpSum, ValueField: "Amount"}},
		SortBy:  "sum_amount",
		Limit:   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Sums: Open 300, Won 300, Lost 50 → top 2 are the 300s, Lost dropped.
	if len(res.Buckets) != 2 || !res.Truncated {
		t.Fatalf("limit 2 → %d buckets (truncated=%v)", len(res.Buckets), res.Truncated)
	}
	if _, ok := planBucket(res, "Lost"); ok {
		t.Error("Lost (50) should have been dropped by limit 2")
	}
}

// A person/array group-by explodes so each participant is counted.
func TestRunPlan_ArrayGroupBy(t *testing.T) {
	fields, rows, _ := buildDeals()
	res, err := RunPlan(fields, rows, QueryPlan{
		GroupBy: "Owner",
		Metrics: []PlanMetric{{Op: OpCount}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Alice appears in 3 rows (Won, Lost, Open), Bob in 2 (Won, Open).
	alice, _ := planBucket(res, "Alice")
	bob, _ := planBucket(res, "Bob")
	if alice.Metrics["count"] != 3 || bob.Metrics["count"] != 2 {
		t.Errorf("Alice/Bob = %v/%v, want 3/2", alice.Metrics["count"], bob.Metrics["count"])
	}
}

// No group-by yields a single Total group (scalar answer).
func TestRunPlan_TotalNoGroup(t *testing.T) {
	fields, rows, _ := buildDeals()
	res, err := RunPlan(fields, rows, QueryPlan{Metrics: []PlanMetric{{Op: OpSum, ValueField: "Amount"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Buckets) != 1 || res.Buckets[0].Label != "Total" || res.Buckets[0].Metrics["sum_amount"] != 650 {
		t.Fatalf("total = %+v, want single Total=650", res.Buckets)
	}
}

func TestRunPlan_ValidationErrors(t *testing.T) {
	fields, rows, _ := buildDeals()
	cases := []struct {
		name string
		plan QueryPlan
	}{
		{"no metrics", QueryPlan{GroupBy: "Stage"}},
		{"unknown group", QueryPlan{GroupBy: "Nope", Metrics: []PlanMetric{{Op: OpCount}}}},
		{"sum without value field", QueryPlan{Metrics: []PlanMetric{{Op: OpSum}}}},
		{"unknown value column", QueryPlan{Metrics: []PlanMetric{{Op: OpSum, ValueField: "Nope"}}}},
		{"duplicate label", QueryPlan{Metrics: []PlanMetric{{Op: OpCount, Label: "x"}, {Op: OpSum, ValueField: "Amount", Label: "x"}}}},
	}
	for _, c := range cases {
		if _, err := RunPlan(fields, rows, c.plan); err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
	}
}
