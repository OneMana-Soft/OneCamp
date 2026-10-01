package business

import "testing"

// planMetricLabel is the stable key that having/share_of/sort_by reference, so
// its derivation must be predictable. RunPlan itself needs a DB; this pins the
// pure label logic + the having-op whitelist.

func TestPlanMetricLabel(t *testing.T) {
	cases := []struct {
		m    PlanMetric
		col  string
		want string
	}{
		{PlanMetric{Op: "count"}, "", "count"},
		{PlanMetric{Op: "sum", ValueField: "Amount"}, "Amount", "sum_amount"},
		{PlanMetric{Op: "avg", ValueField: "Deal Size"}, "Deal Size", "avg_deal_size"},
		{PlanMetric{Op: "sum", ValueField: "Amount", Label: "revenue"}, "Amount", "revenue"},
		{PlanMetric{Op: ""}, "", "count"}, // empty op defaults to count
	}
	for _, c := range cases {
		if got := planMetricLabel(c.m, c.col); got != c.want {
			t.Fatalf("planMetricLabel(%+v,%q) = %q, want %q", c.m, c.col, got, c.want)
		}
	}
}

func TestHavingOpsWhitelist(t *testing.T) {
	for _, ok := range []string{"gt", "gte", "lt", "lte", "eq", "ne"} {
		if _, present := validHavingOps[ok]; !present {
			t.Fatalf("expected having op %q to be allowed", ok)
		}
	}
	for _, bad := range []string{"contains", "empty", "like", "", "; DROP"} {
		if _, present := validHavingOps[bad]; present {
			t.Fatalf("op %q must NOT be a valid having op", bad)
		}
	}
}

func TestIndexOf(t *testing.T) {
	ss := []string{"count", "sum_amount", "avg_age"}
	if indexOf(ss, "sum_amount") != 1 {
		t.Fatal("expected index 1")
	}
	if indexOf(ss, "missing") != -1 {
		t.Fatal("expected -1 for missing")
	}
}
