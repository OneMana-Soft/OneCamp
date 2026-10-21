package taskrank

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// L-1: a selection dropped between two tasks keeps its own order, evenly
// spaced in the gap; at an end, a Step apart going outward.
func TestSeveralBetweenTwo(t *testing.T) {
	a, b := task("a", t0, 0), task("b", t0, 1024)
	got, ok := BetweenMany(a, b, 3)
	if !ok || len(got) != 3 || got[0] != 256 || got[1] != 512 || got[2] != 768 {
		t.Fatalf("%v %v", got, ok)
	}
	top, ok := BetweenMany(nil, a, 2)
	if !ok || top[0] != -2*Step || top[1] != -Step {
		t.Fatalf("at the top: %v", top)
	}
	end, ok := BetweenMany(b, nil, 2)
	if !ok || end[0] != 1024+Step || end[1] != 1024+2*Step {
		t.Fatalf("at the end: %v", end)
	}
	if _, ok := BetweenMany(task("x", t0, 1), task("y", t0, 1.0000000000000002), 3); ok {
		t.Fatal("no room was taken as room")
	}
	if one, ok := BetweenMany(a, b, 1); !ok || one[0] != 512 {
		t.Fatalf("one is the gap's midpoint: %v", one)
	}
}

// L-1: renumbering places a selection, in its order, where it was dropped.
func TestRenumberPlacesASelection(t *testing.T) {
	a, b, c := task("a", t0, 1), task("b", t0, 1), task("c", t0, 1)
	m1, m2 := task("m1", t0), task("m2", t0, 5)
	out := RenumberMany([]*dgraphStruct.DgraphTask{a, m2, b, c}, []*dgraphStruct.DgraphTask{m1, m2}, b, c)
	if got := ids(out); got != "a,b,m1,m2,c" {
		t.Fatalf("order = %s, want a,b,m1,m2,c", got)
	}
	for i := 1; i < len(out); i++ {
		if *out[i].Rank-*out[i-1].Rank != Step {
			t.Fatalf("not evenly spaced at %d", i)
		}
	}
}
