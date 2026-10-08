package business

import (
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// helpers to build a small in-memory table for the pure aggregation engine.

func fld(name, ftype string) *model.Field {
	return &model.Field{Id: uuid.New(), Name: name, Type: ftype}
}

func row(values string) *model.Row {
	return &model.Row{Id: uuid.New(), Values: values}
}

// buildDeals returns a fields+rows fixture: Stage (select), Amount (number),
// Owner (person array), Tags (multi_select array).
func buildDeals() ([]*model.Field, []*model.Row, map[string]*model.Field) {
	stage := fld("Stage", model.FieldSelect)
	amount := fld("Amount", model.FieldNumber)
	owner := fld("Owner", model.FieldPerson)
	tags := fld("Tags", model.FieldMultiSelect)
	fields := []*model.Field{stage, amount, owner, tags}
	byName := map[string]*model.Field{"Stage": stage, "Amount": amount, "Owner": owner, "Tags": tags}

	mk := func(stageV string, amt string, owners string, tagsV string) *model.Row {
		return row(`{"` + stage.Id.String() + `":"` + stageV + `","` +
			amount.Id.String() + `":` + amt + `,"` +
			owner.Id.String() + `":` + owners + `,"` +
			tags.Id.String() + `":` + tagsV + `}`)
	}
	rows := []*model.Row{
		mk("Won", "100", `["Alice"]`, `["enterprise","priority"]`),
		mk("Won", "200", `["Bob"]`, `["smb"]`),
		mk("Lost", "50", `["Alice"]`, `["enterprise"]`),
		mk("Open", "300", `["Alice","Bob"]`, `[]`),
	}
	return fields, rows, byName
}

func bucketByLabel(res *AggResult, label string) (AggBucket, bool) {
	for _, b := range res.Buckets {
		if b.Label == label {
			return b, true
		}
	}
	return AggBucket{}, false
}

func TestAggregate_CountByGroup(t *testing.T) {
	fields, rows, _ := buildDeals()
	res, err := Aggregate(fields, rows, QuerySpec{GroupBy: "Stage", Op: OpCount})
	if err != nil {
		t.Fatal(err)
	}
	if res.MatchedRows != 4 || res.ScannedRows != 4 {
		t.Fatalf("matched/scanned = %d/%d, want 4/4", res.MatchedRows, res.ScannedRows)
	}
	won, _ := bucketByLabel(res, "Won")
	if won.Value != 2 || won.Count != 2 {
		t.Errorf("Won count = %v (count %d), want 2", won.Value, won.Count)
	}
	// Default sort is by value desc: Won (2) should be first.
	if res.Buckets[0].Label != "Won" {
		t.Errorf("first bucket = %q, want Won (desc by count)", res.Buckets[0].Label)
	}
}

func TestAggregate_SumByGroup(t *testing.T) {
	fields, rows, _ := buildDeals()
	res, err := Aggregate(fields, rows, QuerySpec{GroupBy: "Stage", Op: OpSum, ValueField: "Amount"})
	if err != nil {
		t.Fatal(err)
	}
	won, _ := bucketByLabel(res, "Won")
	if won.Value != 300 { // 100 + 200
		t.Errorf("Won sum = %v, want 300", won.Value)
	}
	if res.ValueFieldLabel != "Amount" || res.GroupByLabel != "Stage" {
		t.Errorf("labels = %q/%q", res.GroupByLabel, res.ValueFieldLabel)
	}
}

func TestAggregate_AvgMinMax(t *testing.T) {
	fields, rows, _ := buildDeals()
	avg, err := Aggregate(fields, rows, QuerySpec{Op: OpAvg, ValueField: "Amount"})
	if err != nil {
		t.Fatal(err)
	}
	// No group-by => single "Total" bucket; avg of 100,200,50,300 = 162.5
	total, ok := bucketByLabel(avg, "Total")
	if !ok || total.Value != 162.5 {
		t.Errorf("avg total = %v, want 162.5", total.Value)
	}
	mx, _ := Aggregate(fields, rows, QuerySpec{Op: OpMax, ValueField: "Amount"})
	if b, _ := bucketByLabel(mx, "Total"); b.Value != 300 {
		t.Errorf("max = %v, want 300", b.Value)
	}
	mn, _ := Aggregate(fields, rows, QuerySpec{Op: OpMin, ValueField: "Amount"})
	if b, _ := bucketByLabel(mn, "Total"); b.Value != 50 {
		t.Errorf("min = %v, want 50", b.Value)
	}
}

func TestAggregate_ExplodesArrayGroups(t *testing.T) {
	fields, rows, _ := buildDeals()
	res, err := Aggregate(fields, rows, QuerySpec{GroupBy: "Owner", Op: OpCount})
	if err != nil {
		t.Fatal(err)
	}
	// Alice appears in rows 1,3,4 => 3; Bob in rows 2,4 => 2.
	alice, _ := bucketByLabel(res, "Alice")
	bob, _ := bucketByLabel(res, "Bob")
	if alice.Value != 3 {
		t.Errorf("Alice = %v, want 3", alice.Value)
	}
	if bob.Value != 2 {
		t.Errorf("Bob = %v, want 2", bob.Value)
	}
}

func TestAggregate_Filters(t *testing.T) {
	fields, rows, _ := buildDeals()
	// Only Won deals, summed by owner.
	res, err := Aggregate(fields, rows, QuerySpec{
		GroupBy:    "Owner",
		Op:         OpSum,
		ValueField: "Amount",
		Filters:    []Filter{{Field: "Stage", Op: "eq", Value: "Won"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.MatchedRows != 2 {
		t.Fatalf("matched = %d, want 2", res.MatchedRows)
	}
	alice, _ := bucketByLabel(res, "Alice")
	bob, _ := bucketByLabel(res, "Bob")
	if alice.Value != 100 || bob.Value != 200 {
		t.Errorf("Alice/Bob = %v/%v, want 100/200", alice.Value, bob.Value)
	}
}

func TestAggregate_NumericFilter(t *testing.T) {
	fields, rows, _ := buildDeals()
	res, err := Aggregate(fields, rows, QuerySpec{
		Op:      OpCount,
		Filters: []Filter{{Field: "Amount", Op: "gte", Value: "200"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := bucketByLabel(res, "Total"); b.Value != 2 { // 200 and 300
		t.Errorf("count >=200 = %v, want 2", b.Value)
	}
}

func TestAggregate_EmptyGroupLabel(t *testing.T) {
	fields, rows, _ := buildDeals()
	// Group by Tags: row 4 has empty tags => "(empty)".
	res, err := Aggregate(fields, rows, QuerySpec{GroupBy: "Tags", Op: OpCount})
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := bucketByLabel(res, "(empty)"); !ok || b.Value != 1 {
		t.Errorf("(empty) tags bucket = %v (ok=%v), want 1", b.Value, ok)
	}
	ent, _ := bucketByLabel(res, "enterprise")
	if ent.Value != 2 {
		t.Errorf("enterprise = %v, want 2", ent.Value)
	}
}

func TestAggregate_DateGroupIsChronological(t *testing.T) {
	day := fld("Day", model.FieldDate)
	amount := fld("Amount", model.FieldNumber)
	fields := []*model.Field{day, amount}
	mk := func(d, amt string) *model.Row {
		return row(`{"` + day.Id.String() + `":"` + d + `","` + amount.Id.String() + `":` + amt + `}`)
	}
	// Intentionally out of order, with a high-value early date to prove it is
	// NOT sorted by value.
	rows := []*model.Row{
		mk("2026-03-02", "5"),
		mk("2026-03-01", "100"),
		mk("2026-03-03", "7"),
	}
	res, err := Aggregate(fields, rows, QuerySpec{GroupBy: "Day", Op: OpSum, ValueField: "Amount"})
	if err != nil {
		t.Fatal(err)
	}
	if res.GroupByType != model.FieldDate {
		t.Fatalf("GroupByType = %q, want date", res.GroupByType)
	}
	got := []string{res.Buckets[0].Label, res.Buckets[1].Label, res.Buckets[2].Label}
	want := []string{"2026-03-01", "2026-03-02", "2026-03-03"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chronological order = %v, want %v", got, want)
		}
	}
}

func TestAggregate_DateTruncationKeepsMostRecent(t *testing.T) {
	day := fld("Day", model.FieldDate)
	fields := []*model.Field{day}
	var rows []*model.Row
	for _, d := range []string{"2026-01-01", "2026-01-02", "2026-01-03", "2026-01-04"} {
		rows = append(rows, row(`{"`+day.Id.String()+`":"`+d+`"}`))
	}
	res, err := Aggregate(fields, rows, QuerySpec{GroupBy: "Day", Op: OpCount, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Buckets) != 2 || !res.Truncated {
		t.Fatalf("want 2 truncated buckets, got %d (truncated=%v)", len(res.Buckets), res.Truncated)
	}
	// Should keep the two most recent days, still chronological.
	if res.Buckets[0].Label != "2026-01-03" || res.Buckets[1].Label != "2026-01-04" {
		t.Errorf("kept %q,%q; want the two most recent days", res.Buckets[0].Label, res.Buckets[1].Label)
	}
}

func TestAggregate_Validation(t *testing.T) {
	fields, rows, _ := buildDeals()
	if _, err := Aggregate(fields, rows, QuerySpec{Op: OpSum}); err == nil {
		t.Error("sum without value_field should error")
	}
	if _, err := Aggregate(fields, rows, QuerySpec{GroupBy: "Nope", Op: OpCount}); err == nil {
		t.Error("unknown group-by column should error")
	}
	if _, err := Aggregate(fields, rows, QuerySpec{Op: "median", ValueField: "Amount"}); err == nil {
		t.Error("unsupported op should error")
	}
	// Abusive filter list is rejected (DoS guard on the public endpoint).
	many := make([]Filter, maxFilters+1)
	for i := range many {
		many[i] = Filter{Field: "Stage", Op: "eq", Value: "Won"}
	}
	if _, err := Aggregate(fields, rows, QuerySpec{Op: OpCount, Filters: many}); err == nil {
		t.Error("more than maxFilters should error")
	}
	// Exactly the cap is allowed.
	ok := make([]Filter, maxFilters)
	for i := range ok {
		ok[i] = Filter{Field: "Stage", Op: "not_empty"}
	}
	if _, err := Aggregate(fields, rows, QuerySpec{Op: OpCount, Filters: ok}); err != nil {
		t.Errorf("exactly maxFilters should be allowed, got %v", err)
	}
}

func TestAggregate_LimitAndSort(t *testing.T) {
	stage := fld("K", model.FieldSelect)
	fields := []*model.Field{stage}
	var rows []*model.Row
	// 3 x A, 2 x B, 1 x C
	for i := 0; i < 3; i++ {
		rows = append(rows, row(`{"`+stage.Id.String()+`":"A"}`))
	}
	for i := 0; i < 2; i++ {
		rows = append(rows, row(`{"`+stage.Id.String()+`":"B"}`))
	}
	rows = append(rows, row(`{"`+stage.Id.String()+`":"C"}`))

	res, err := Aggregate(fields, rows, QuerySpec{GroupBy: "K", Op: OpCount, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Buckets) != 2 || !res.Truncated {
		t.Fatalf("expected 2 buckets + truncated, got %d (truncated=%v)", len(res.Buckets), res.Truncated)
	}
	if res.Buckets[0].Label != "A" || res.Buckets[1].Label != "B" {
		t.Errorf("top-2 by count = %q,%q; want A,B", res.Buckets[0].Label, res.Buckets[1].Label)
	}
	if res.DistinctGroups != 3 {
		t.Errorf("distinct = %d, want 3", res.DistinctGroups)
	}
}

// A number's label is as people write it, short of a huge or a tiny one,
// which is in exponent form instead of hundreds of digits.
func TestNumberLabels(t *testing.T) {
	for f, want := range map[float64]string{
		1500000: "1500000", -2.5: "-2.5", 0.0000001: "0.0000001", 1e20: "100000000000000000000",
		1e21: "1e+21", 1e308: "1e+308", -1e300: "-1e+300", 1e-300: "1e-300", 0: "0",
	} {
		if got := formatFloat(f); got != want {
			t.Errorf("formatFloat(%g) = %q, want %q", f, got, want)
		}
	}
	if got := cellLabels([]interface{}{1e308, "Live", 2.0}); strings.Join(got, ",") != "1e+308,Live,2" {
		t.Errorf("a list's labels: %q", got)
	}
}
