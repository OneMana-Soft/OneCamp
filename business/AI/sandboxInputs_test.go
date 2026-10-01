package business

import (
	"strings"
	"testing"

	codesandbox "github.com/akashc777/OneCamp/business/CodeSandbox"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	dtmodel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

func TestStringifyCell(t *testing.T) {
	cases := []struct {
		in   interface{}
		want string
	}{
		{nil, ""},
		{"hello", "hello"},
		{true, "true"},
		{float64(42), "42"},
		{float64(3.5), "3.5"},
		{[]interface{}{"a", "b"}, "a; b"},
		{map[string]interface{}{"label": "Alice", "id": "u1"}, "Alice"},
		{map[string]interface{}{"id": "u1"}, "u1"},
	}
	for _, c := range cases {
		if got := stringifyCell(c.in); got != c.want {
			t.Errorf("stringifyCell(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTableRowsToInputFile(t *testing.T) {
	f1 := &dtmodel.Field{Id: uuid.New(), Name: "Stage", Type: dtmodel.FieldSelect}
	f2 := &dtmodel.Field{Id: uuid.New(), Name: "Amount", Type: dtmodel.FieldNumber}
	fields := []*dtmodel.Field{f1, f2}
	row := func(stage string, amt string) *dtmodel.Row {
		return &dtmodel.Row{Id: uuid.New(), Values: `{"` + f1.Id.String() + `":"` + stage + `","` + f2.Id.String() + `":` + amt + `}`}
	}
	rows := []*dtmodel.Row{row("Won", "100"), row("Lost", "50")}

	f := tableRowsToInputFile("deals", fields, rows, 0)
	if f.Name != "deals" || f.Format != codesandbox.FormatCSV {
		t.Fatalf("unexpected file meta: %+v", f)
	}
	csv := string(f.Bytes)
	if !strings.HasPrefix(csv, "Stage,Amount\n") {
		t.Fatalf("header should be field names: %q", csv)
	}
	if !strings.Contains(csv, "Won,100") || !strings.Contains(csv, "Lost,50") {
		t.Fatalf("rows not serialized: %q", csv)
	}

	// maxRows caps the number of data rows.
	capped := string(tableRowsToInputFile("deals", fields, rows, 1).Bytes)
	if strings.Contains(capped, "Lost") {
		t.Errorf("maxRows=1 should drop the 2nd row: %q", capped)
	}
}

func TestAggResultToInputFile(t *testing.T) {
	res := &dataTableBusiness.AggResult{
		Op:              dataTableBusiness.OpSum,
		GroupByLabel:    "Stage",
		ValueFieldLabel: "Amount",
		Buckets: []dataTableBusiness.AggBucket{
			{Label: "Won", Value: 300, Count: 2},
			{Label: "Lost", Value: 50, Count: 1},
		},
	}
	csv := string(aggResultToInputFile("summary", res).Bytes)
	if !strings.HasPrefix(csv, "Stage,sum_Amount,count\n") {
		t.Fatalf("header wrong: %q", csv)
	}
	if !strings.Contains(csv, "Won,300,2") || !strings.Contains(csv, "Lost,50,1") {
		t.Fatalf("bucket rows wrong: %q", csv)
	}

	// Count aggregation with no value field → header uses the op name.
	countRes := &dataTableBusiness.AggResult{Op: dataTableBusiness.OpCount, GroupByLabel: "Stage",
		Buckets: []dataTableBusiness.AggBucket{{Label: "Won", Value: 2, Count: 2}}}
	if h := string(aggResultToInputFile("c", countRes).Bytes); !strings.HasPrefix(h, "Stage,count,count\n") {
		t.Fatalf("count header wrong: %q", h)
	}

	// Nil-safe.
	if got := string(aggResultToInputFile("x", nil).Bytes); !strings.HasPrefix(got, "group,") {
		t.Fatalf("nil result should still produce a header: %q", got)
	}
}
