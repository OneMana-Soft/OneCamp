package business

import (
	"reflect"
	"strings"
	"testing"
)

// Today's twelve kinds are registered, in the column menu's order, with the
// cells they have: formulas and rollups aren't stored, selects hold options.
func TestTodaysKindsAreRegistered(t *testing.T) {
	var got []string
	for _, k := range Kinds.All() {
		got = append(got, k.ID)
	}
	want := []string{"text", "number", "select", "multi_select", "date", "checkbox", "person", "url", "email", "relation", "formula", "rollup"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds %v, want %v", got, want)
	}
	for _, id := range want {
		k, ok := Kinds.Lookup(id)
		if !ok || !Kinds.Valid(id) {
			t.Errorf("%s: registered %v, valid %v", id, ok, Kinds.Valid(id))
		}
		if stored := id != "formula" && id != "rollup"; k.Stored != stored {
			t.Errorf("%s stored %v, want %v", id, k.Stored, stored)
		}
		if opts := id == "select" || id == "multi_select"; k.HasOptions != opts {
			t.Errorf("%s has options %v, want %v", id, k.HasOptions, opts)
		}
	}
	if _, ok := Kinds.Lookup("lookup"); ok {
		t.Error("a kind nobody built is registered")
	}
}

// At day 0 nothing is checked: the cells come back as they went in.
func TestCheckKeepsEveryCellAtDayZero(t *testing.T) {
	in := map[string]any{"a": "12 kg", "b": []any{"x"}, "c": nil}
	out, err := Kinds.Check(nil, in, Input{New: true, Complete: true})
	if err != nil || !reflect.DeepEqual(out, in) {
		t.Errorf("Check gave %v, %v", out, err)
	}
}

// A kind with no id, or one registered twice, stops the build's tests.
func TestRegisteringAKindTwicePanics(t *testing.T) {
	r := &Registry{}
	r.Register(Kind{ID: "x"})
	for _, k := range []Kind{{ID: "x"}, {}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("registering %+v didn't panic", k)
				}
			}()
			r.Register(k)
		}()
	}
}

// A number's label is as people write it, short of a huge or a tiny one,
// which is in exponent form instead of hundreds of digits.
func TestNumberLabels(t *testing.T) {
	for f, want := range map[float64]string{
		1500000: "1500000", -2.5: "-2.5", 0.0000001: "0.0000001", 1e20: "100000000000000000000",
		1e21: "1e+21", 1e308: "1e+308", -1e300: "-1e+300", 1e-300: "1e-300", 0: "0",
	} {
		if got := Labels(f); len(got) != 1 || got[0] != want {
			t.Errorf("Labels(%g) = %q, want %q", f, got, want)
		}
	}
	if got := Labels([]any{1e308, "Live", 2.0}); strings.Join(got, ",") != "1e+308,Live,2" {
		t.Errorf("a list's labels: %q", got)
	}
}
