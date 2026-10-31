package business

import (
	"slices"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// These tests pin the SAFETY contract of the DB pushdown: a filter may only
// give a term (rowFilters.terms) when a coarse `values::text ILIKE '%term%'`
// pre-filter is guaranteed to return a SUPERSET of what the in-memory filter
// keeps. If it gave a term for an operator or a value where that isn't true,
// a filtered aggregation over a large table could silently drop real
// matches. The in-memory filter stays authoritative; these guard the
// optimization that feeds it.

// termsOf is the terms filters over fields push down, read in UTC.
func termsOf(t *testing.T, fields []*model.Field, filters ...Filter) []string {
	t.Helper()
	rf, err := newRowFilters(fields, filters, reading{})
	if err != nil {
		t.Fatalf("%v: %v", filters, err)
	}
	return rf.terms()
}

func TestPushableLikeTerms_OnlyEqAndContains(t *testing.T) {
	fields, _, _ := buildDeals()
	won := fld("Won", model.FieldCheckbox)
	due := fld("Due", model.FieldDate)
	stage := fld("Phase", model.FieldSelect)
	stage.Config = `{"options":[{"id":"o_talk","label":"Talking"}]}`
	fields = append(fields, won, due, stage)

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
		// A number is stored as one: "1,000" is 1000 there.
		{"a number never pushes", Filter{Field: "Amount", Op: "eq", Value: "1,000"}, nil},
		// A tick is stored as true or false, whatever word asked for it.
		{"a checkbox never pushes", Filter{Field: "Won", Op: "eq", Value: "yes"}, nil},
		// A choice is stored as its label, whichever way it was named.
		{"an option's id pushes its label", Filter{Field: "Phase", Op: "eq", Value: "o_talk"}, []string{"Talking"}},
		// A stored date is its day, so a month or a day it falls in is in it.
		{"a date's day pushes", Filter{Field: "Due", Op: "eq", Value: "2026-10"}, []string{"2026-10"}},
		{"a date after a day never pushes", Filter{Field: "Due", Op: "gt", Value: "2026-10-01"}, nil},
		// A stored cell's JSON writes a quote as \" and a backslash as \\.
		{"a quote never pushes", Filter{Field: "Stage", Op: "eq", Value: `Say "won"`}, nil},
		{"a backslash never pushes", Filter{Field: "Stage", Op: "contains", Value: `a\b`}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := termsOf(t, fields, c.filter); !slices.Equal(got, c.want) {
				t.Fatalf("terms = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPushableLikeTerms_MixedFiltersKeepOnlySafe(t *testing.T) {
	fields, _, _ := buildDeals()
	got := termsOf(t, fields,
		Filter{Field: "Stage", Op: "eq", Value: "Won"},    // pushable
		Filter{Field: "Amount", Op: "gt", Value: "100"},   // not pushable (numeric)
		Filter{Field: "Tags", Op: "contains", Value: "e"}, // pushable
	)
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
	terms := termsOf(t, fields, spec.Filters...)
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
