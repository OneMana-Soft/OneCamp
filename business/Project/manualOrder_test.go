package business

import (
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// L-1: the list's Manual sort pages through the project's one order: the
// tasks a page shows, by uid, in that order; a filter that reads them back
// by those uids; and the page put back in order, whatever order they came in.
func TestAManualPage(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	mk := func(uid string, rank *float64, minutes int) *dgraphStruct.DgraphTask {
		c := at.Add(time.Duration(minutes) * time.Minute)
		return &dgraphStruct.DgraphTask{Uid: uid, Uuid: "u" + uid, Rank: rank, CreatedAt: &c}
	}
	r := func(f float64) *float64 { return &f }
	// Never moved: newest first, before ranked ones at 0 and above.
	places := []*dgraphStruct.DgraphTask{mk("0x3", r(10), 0), mk("0x1", nil, 1), mk("0x2", nil, 2), mk("0x4", r(5), 0)}
	uidsOf := func(ts []*dgraphStruct.DgraphTask) string {
		var out []string
		for _, x := range ts {
			out = append(out, x.Uid)
		}
		return strings.Join(out, ",")
	}
	if got := uidsOf(manualPage(places, 2, 1, false)); got != "0x4,0x3" {
		t.Fatalf("page 2 is %v", got)
	}
	if got := uidsOf(manualPage(places, 2, 0, true)); got != "0x2,0x1,0x4,0x3" {
		t.Fatalf("all is %v", got)
	}
	if past := manualPage(places, 2, 5, false); len(past) != 0 {
		t.Fatalf("past the end is %v", past)
	}
	if f := uidFilter("eq(task_status, \"todo\")", []string{"0x4", "0x3"}); f != `eq(task_status, "todo") AND uid(0x4, 0x3)` {
		t.Fatalf("filter %q", f)
	}
	if f := uidFilter("", nil); f != "uid(0x0)" {
		t.Fatalf("no tasks: %q", f)
	}
	// The list's read carries no uid: the page is put back by task uuid.
	got := []*dgraphStruct.DgraphTask{{Uuid: "U3"}, {Uuid: "u4"}}
	inOrder(got, []*dgraphStruct.DgraphTask{{Uid: "0x4", Uuid: "u4"}, {Uid: "0x3", Uuid: "u3"}})
	if got[0].Uuid != "u4" || got[1].Uuid != "U3" {
		t.Fatalf("put back as %s,%s", got[0].Uuid, got[1].Uuid)
	}
}
