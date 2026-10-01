package business

import (
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// These tests pin the SAFETY contract of the DB pushdown: pushableLikeTerms may
// only ever emit a term when a coarse `values::text ILIKE '%term%'` pre-filter
// is guaranteed to return a SUPERSET of what the in-memory engine matches. If
// it emitted a term for an operator/value where that isn't true, a filtered
// aggregation over a large table could silently drop real matches. The pure
// engine stays authoritative; these guard the optimization that feeds it.

func TestPushableLikeTerms_OnlyEqAndContains(t *testing.T) {
	fields, _, _ := buildDeals()

	cases := []struct {
		name   string
		filter Filter
		want   []string
	}{
		{"eq pushes", Filter{Field: "Stage", Op: "eq", Value: "Won"}, []string{"Won"}},
		{"contains pushes", Filter{Field: "Tags", Op: "contains", Value: "enter"}, []string{"enter"}},
		{"ne is complement, never push", Filter{Field: "Stage", Op: "ne", Value: "Won"}, nil},
		{"gt is numeric, never push", Filter{Field: "Amount", Op: "gt", Value: "100"}, nil},
		{"lte is numeric, never push", Filter{Field: "Amount", Op: "lte", Value: "100"}, nil},
		{"empty never push", Filter{Field: "Stage", Op: "empty", Value: ""}, nil},
		{"not_empty never push", Filter{Field: "Stage", Op: "not_empty", Value: ""}, nil},
		{"blank value never push", Filter{Field: "Stage", Op: "eq", Value: "   "}, nil},
		{"unknown field never push", Filter{Field: "Nope", Op: "eq", Value: "Won"}, nil},
		{"non-ascii value never push", Filter{Field: "Stage", Op: "eq", Value: "café"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pushableLikeTerms(fields, []Filter{c.filter})
			if len(got) != len(c.want) {
				t.Fatalf("terms = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("terms = %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestPushableLikeTerms_MixedFiltersKeepOnlySafe(t *testing.T) {
	fields, _, _ := buildDeals()
	filters := []Filter{
		{Field: "Stage", Op: "eq", Value: "Won"},    // pushable
		{Field: "Amount", Op: "gt", Value: "100"},   // not pushable (numeric)
		{Field: "Tags", Op: "contains", Value: "e"}, // pushable
	}
	got := pushableLikeTerms(fields, filters)
	if len(got) != 2 || got[0] != "Won" || got[1] != "e" {
		t.Fatalf("expected [Won e], got %v", got)
	}
}

// The optimization must never change the in-memory result. Emulating the DB
// superset pre-filter with the same ILIKE-substring semantics, then running the
// pure engine over the narrowed rows, must equal running it over all rows.
func TestPushdownSupersetPreservesResult(t *testing.T) {
	fields, rows, _ := buildDeals()
	spec := QuerySpec{
		Filters:    []Filter{{Field: "Stage", Op: "eq", Value: "Won"}},
		GroupBy:    "Owner",
		Op:         OpSum,
		ValueField: "Amount",
	}

	full, err := Aggregate(fields, rows, spec)
	if err != nil {
		t.Fatalf("full aggregate err: %v", err)
	}

	// Superset pre-filter: keep rows whose raw JSONB text contains every term
	// (case-insensitive), mirroring model.ListRowsFiltered's ILIKE narrowing.
	terms := pushableLikeTerms(fields, spec.Filters)
	narrowed := filterRowsBySubstrings(rows, terms)
	if len(narrowed) >= len(rows) {
		t.Fatalf("expected the pre-filter to narrow rows (got %d of %d)", len(narrowed), len(rows))
	}

	pushed, err := Aggregate(fields, narrowed, spec)
	if err != nil {
		t.Fatalf("pushed aggregate err: %v", err)
	}

	if len(full.Buckets) != len(pushed.Buckets) || full.MatchedRows != pushed.MatchedRows {
		t.Fatalf("pushdown changed shape: full=%+v pushed=%+v", full, pushed)
	}
	for _, fb := range full.Buckets {
		pb, ok := bucketByLabel(pushed, fb.Label)
		if !ok || pb.Value != fb.Value || pb.Count != fb.Count {
			t.Fatalf("bucket %q diverged: full=%+v pushed=%+v", fb.Label, fb, pb)
		}
	}
}

// filterRowsBySubstrings mirrors the DB ILIKE superset filter in memory (for the
// equivalence test only): keep a row when its raw values text contains every
// term, case-insensitively.
func filterRowsBySubstrings(rows []*model.Row, terms []string) []*model.Row {
	if len(terms) == 0 {
		return rows
	}
	var out []*model.Row
	for _, r := range rows {
		hay := toLowerASCII(r.Values)
		ok := true
		for _, term := range terms {
			if !containsFold(hay, toLowerASCII(term)) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, r)
		}
	}
	return out
}

func toLowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func containsFold(hay, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
