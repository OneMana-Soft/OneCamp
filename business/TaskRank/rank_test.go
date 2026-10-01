package taskrank

import (
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func task(id string, created time.Time, rank ...float64) *dgraphStruct.DgraphTask {
	t := &dgraphStruct.DgraphTask{Uuid: id, CreatedAt: &created}
	if len(rank) > 0 {
		t.Rank = &rank[0]
	}
	return t
}

func ids(tasks []*dgraphStruct.DgraphTask) string {
	var b []string
	for _, t := range tasks {
		b = append(b, t.Uuid)
	}
	return strings.Join(b, ",")
}

var t0 = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

// A board nobody has rearranged must look exactly as it did: newest first.
func TestUnrankedColumnKeepsNewestFirst(t *testing.T) {
	col := []*dgraphStruct.DgraphTask{
		task("old", t0),
		task("new", t0.Add(2*time.Hour)),
		task("mid", t0.Add(time.Hour)),
	}
	Sort(col)
	if got := ids(col); got != "new,mid,old" {
		t.Fatalf("order = %s, want new,mid,old", got)
	}
}

// The reported bug: a card dropped between two others must stay there after
// the board is fetched again.
func TestDropBetweenStaysAfterRefetch(t *testing.T) {
	a, b, c := task("a", t0.Add(3*time.Hour)), task("b", t0.Add(2*time.Hour)), task("c", t0.Add(time.Hour))
	// Drag c between a and b.
	rank, ok := Between(a, b)
	if !ok {
		t.Fatal("no room between two tasks created an hour apart")
	}
	c.Rank = &rank
	col := []*dgraphStruct.DgraphTask{a, b, c}
	Sort(col)
	if got := ids(col); got != "a,c,b" {
		t.Fatalf("order = %s, want a,c,b", got)
	}
}

// A task created after a move still arrives at the top.
func TestNewTaskArrivesOnTopOfMovedOnes(t *testing.T) {
	a, b := task("a", t0), task("b", t0.Add(time.Hour))
	r, _ := Between(nil, b) // a moved to the top
	a.Rank = &r
	fresh := task("fresh", t0.Add(24*time.Hour))
	col := []*dgraphStruct.DgraphTask{a, b, fresh}
	Sort(col)
	if got := ids(col); got != "fresh,a,b" {
		t.Fatalf("order = %s, want fresh,a,b", got)
	}
}

func TestEnds(t *testing.T) {
	a := task("a", t0, 10)
	if r, ok := Between(nil, a); !ok || r >= 10 {
		t.Fatalf("top of column: %v %v", r, ok)
	}
	if r, ok := Between(a, nil); !ok || r <= 10 {
		t.Fatalf("bottom of column: %v %v", r, ok)
	}
	if _, ok := Between(nil, nil); !ok {
		t.Fatal("an empty column always has room")
	}
}

// Tasks made in the same millisecond (tasks extracted from a conversation)
// leave no room between them; that must ask for a renumber, not collide.
func TestNoRoomAsksForRenumber(t *testing.T) {
	a, b := task("a", t0), task("b", t0)
	if _, ok := Between(a, b); ok {
		t.Fatal("equal ranks must report no room")
	}
	// Neighbours out of order mean a stale view; also a renumber.
	if _, ok := Between(task("x", t0, 5), task("y", t0, 1)); ok {
		t.Fatal("reversed neighbours must report no room")
	}
}

// Dropping into the same gap again and again ends in a renumber that keeps
// every card where the person put it.
func TestRepeatedDropsIntoOneGap(t *testing.T) {
	top, bottom := task("top", t0, 0), task("bottom", t0, 1)
	col := []*dgraphStruct.DgraphTask{top, bottom}
	above := top
	renumbered := false
	for i := 0; i < 200; i++ {
		m := task("m"+string(rune('A'+i%26))+string(rune('a'+i/26)), t0)
		r, ok := Between(above, bottom)
		if !ok {
			col = Renumber(col, m, above, bottom)
			renumbered = true
		} else {
			m.Rank = &r
			col = append(col, m)
		}
		above = m
		Sort(col)
		if col[len(col)-1].Uuid != "bottom" || col[0].Uuid != "top" {
			t.Fatalf("drop %d disturbed the ends: %s", i, ids(col))
		}
		if col[len(col)-2] != m {
			t.Fatalf("drop %d did not land right above bottom: %s", i, ids(col))
		}
	}
	if !renumbered {
		t.Fatal("200 drops into one gap never needed a renumber; the test is not exercising it")
	}
}

func TestRenumberPlacesAndSpaces(t *testing.T) {
	a, b, c := task("a", t0, 1), task("b", t0, 1), task("c", t0, 1)
	moved := task("m", t0)
	out := Renumber([]*dgraphStruct.DgraphTask{a, b, c, moved}, moved, b, c)
	if got := ids(out); got != "a,b,m,c" {
		t.Fatalf("order = %s, want a,b,m,c", got)
	}
	for i := 1; i < len(out); i++ {
		if *out[i].Rank-*out[i-1].Rank != Step {
			t.Fatalf("ranks not evenly spaced: %v then %v", *out[i-1].Rank, *out[i].Rank)
		}
	}
	if *out[0].Rank != 1 {
		t.Fatalf("the first card moved: rank %v, want 1", *out[0].Rank)
	}
}

// My Tasks mixes projects: a neighbour from another project is not in the
// column being renumbered, so its rank decides the place.
func TestRenumberWithNeighbourFromAnotherProject(t *testing.T) {
	a, b := task("a", t0, 0), task("b", t0, 100)
	other := task("other", t0, 50)
	moved := task("m", t0)
	out := Renumber([]*dgraphStruct.DgraphTask{a, b}, moved, other, nil)
	if got := ids(out); got != "a,m,b" {
		t.Fatalf("order = %s, want a,m,b", got)
	}
	out = Renumber([]*dgraphStruct.DgraphTask{task("a", t0, 0), task("b", t0, 100)}, task("m", t0), nil, task("o", t0, 150))
	if got := ids(out); got != "a,b,m" {
		t.Fatalf("order = %s, want a,b,m", got)
	}
}
