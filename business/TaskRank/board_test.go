package taskrank

import (
	"fmt"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func place(id string, created time.Time, custom string, rank ...float64) *dgraphStruct.DgraphTask {
	t := task(id, created, rank...)
	t.Uid = "0x" + id
	if custom != "" {
		t.CustomStatus = &custom
	}
	return t
}

// The order is the same whatever order the tasks came in, ties included:
// a board page must be the start of the next, bigger one.
func TestSortIsTheSameWhateverTheInputOrder(t *testing.T) {
	a, b, c, d := task("a", t0), task("b", t0), task("c", t0.Add(time.Hour)), task("d", t0, -float64(t0.UnixMilli()))
	want := ids([]*dgraphStruct.DgraphTask{c, a, b, d})
	for _, in := range [][]*dgraphStruct.DgraphTask{{a, b, c, d}, {d, c, b, a}, {b, d, a, c}} {
		Sort(in)
		if got := ids(in); got != want {
			t.Fatalf("order = %s, want %s", got, want)
		}
	}
}

// Each part of a column (its own status, each custom status) gets its own
// first n, in board order; n of 0 keeps them all.
func TestWindowKeepsTheFirstNOfEachPart(t *testing.T) {
	var col []*dgraphStruct.DgraphTask
	for i := 0; i < 5; i++ {
		col = append(col, place(fmt.Sprintf("own%d", i), t0.Add(time.Duration(10+i)*time.Hour), ""))
	}
	col = append(col, place("ready0", t0.Add(time.Hour), "ready"), place("ready1", t0, "ready"))
	partOf := func(t *dgraphStruct.DgraphTask) string {
		if t.CustomStatus != nil {
			return *t.CustomStatus
		}
		return ""
	}
	if got := ids(Window(col, 2, partOf)); got != "own4,own3,ready0,ready1" {
		t.Fatalf("window of 2 = %s", got)
	}
	if got := len(Window(col, 0, partOf)); got != 7 {
		t.Fatalf("window of 0 kept %d, want all 7", got)
	}
}

// Pick windows open columns, keeps closed ones as read (the newest
// closedLimit), and Fill swaps in the cards in that order, dropping a card
// that wasn't read.
func TestPickAndFill(t *testing.T) {
	places := map[string]*dgraphStruct.BoardPlaces{
		"todo": {Places: []*dgraphStruct.DgraphTask{place("t1", t0, ""), place("t2", t0.Add(time.Hour), ""), place("t3", t0.Add(2*time.Hour), "")}, Count: 3},
		"done": {Places: []*dgraphStruct.DgraphTask{place("d1", t0, ""), place("d2", t0.Add(time.Hour), "")}, Count: 9},
	}
	picks, uids := Pick(places, 2, func(*dgraphStruct.DgraphTask) string { return "" })
	if got := ids(picks["todo"]); got != "t3,t2" {
		t.Fatalf("todo picks %s", got)
	}
	if got := ids(picks["done"]); got != "d2,d1" {
		t.Fatalf("done picks %s", got)
	}
	if len(uids) != 4 {
		t.Fatalf("uids %v", uids)
	}
	cards := []*dgraphStruct.DgraphTask{{Uuid: "t2", Name: "Two"}, {Uuid: "t3", Name: "Three"}, {Uuid: "d1", Name: "Old"}}
	filled := Fill(picks, cards)
	if got := ids(filled["todo"]); got != "t3,t2" || filled["todo"][0].Name != "Three" {
		t.Fatalf("todo cards %s", got)
	}
	if got := ids(filled["done"]); got != "d1" {
		t.Fatalf("done cards %s, want d1 (d2 was not read)", got)
	}
}
