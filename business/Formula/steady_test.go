package formula

import (
	"slices"
	"testing"
)

// A formula is kept by the Computed Store (airtable-03) only when it gives the
// same value whoever reads it and whenever: never the moment, the reader's
// zone, or a date.
func TestSteadyKeepsOnlyWhatNoMomentOrZoneMoves(t *testing.T) {
	fields := []Field{
		{ID: "budget", Name: "Budget", Kind: KindNumber},
		{ID: "name", Name: "Name", Kind: KindText},
		{ID: "due", Name: "Due", Kind: KindDate},
		{ID: "paid", Name: "Paid", Kind: KindNumber}, // a rollup: steadyField answers
	}
	add := func(id, src string) { fields = append(fields, Field{ID: id, Name: id, IsFormula: true, Formula: src}) }
	add("double", "{Budget} * 2")
	add("label", `IF({Budget} > 5, UPPER({Name}) & "!", "small")`)
	add("onDouble", "{double} + 1")
	add("today", "TODAY()")
	add("onToday", "IF({today}, 1, 0)")
	add("year", "YEAR({Due})")
	add("dueText", "CONCATENATE({Due})")
	add("parsed", `YEAR("2026-01-01")`)
	add("onPaid", "{Paid} + {Budget}")
	add("broken", "{Budget} +")
	p := Compile(fields)
	paidSteady := true
	steadyField := func(ref string) bool { return ref != "paid" || paidSteady }
	for id, want := range map[string]bool{
		"double": true, "label": true, "onDouble": true, "onPaid": true,
		"today": false, "onToday": false, "year": false, "dueText": false, "parsed": false, "broken": false,
	} {
		if got := p.Steady(id, steadyField); got != want {
			t.Errorf("Steady(%s) = %v, want %v", id, got, want)
		}
	}
	paidSteady = false
	if p.Steady("onPaid", steadyField) {
		t.Errorf("a formula over a rollup that isn't kept is kept")
	}
}

// Reads is what one formula reads, so the Computed Store can say which tables
// each formula's values reach from the one plan of their table (airtable-03).
func TestReadsIsWhatOneFormulaReads(t *testing.T) {
	fields := []Field{
		{ID: "budget", Name: "Budget", Kind: KindNumber},
		{ID: "vendors", Name: "Vendors", Kind: KindText, Linked: []Field{{ID: "paid", Name: "Paid", Kind: KindNumber}}},
	}
	add := func(id, src string) { fields = append(fields, Field{ID: id, Name: id, IsFormula: true, Formula: src}) }
	add("double", "{Budget} * 2")
	add("owed", "{double} & {Vendors}.{Paid}")
	add("blank", "")
	add("self", "{self} + 1")
	add("missing", "{Nowhere} + 1")
	add("broken", "{Budget} +")
	p := Compile(fields)
	for id, want := range map[string][]string{
		"double": {"budget"},
		"owed":   {"double", "vendors", LinkedRef("vendors", "paid")}, // the link, and the field through it
		"blank":  nil,
		"self":   {"self"},
	} {
		if got, ok := p.Reads(id); !ok || !slices.Equal(got, want) {
			t.Errorf("Reads(%s) = %v, %v; want %v, true", id, got, ok, want)
		}
	}
	for _, id := range []string{"missing", "broken", "budget", "nope"} {
		if got, ok := p.Reads(id); ok {
			t.Errorf("Reads(%s) = %v, true; want false", id, got)
		}
	}
}
