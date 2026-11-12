package business

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// CAL51-TB: a task block's task says how many open tasks it waits on (the
// graph counts only those in the reader's projects) and names the first, to
// a reader who may read that one by the same check as any task; anyone else
// is told the count alone.
func TestABlocksTaskNamesItsFirstBlockerOnlyToAReaderWhoMayReadIt(t *testing.T) {
	const ben = "0xben"
	member := &dgraphStruct.DgraphProject{Uuid: "p-1", Name: "Launch", IsProjectMember: 1}
	outsider := &dgraphStruct.DgraphProject{Uuid: "p-2", Name: "Marketing"}
	blocker := &dgraphStruct.DgraphTask{Uuid: "B0000000-0000-0000-0000-00000000000B", Name: "Sign off the copy", Status: "todo", Project: member}
	hidden := &dgraphStruct.DgraphTask{Uuid: "c0000000-0000-0000-0000-00000000000c", Name: "Budget", Status: "todo", Project: outsider}
	tasks := []*dgraphStruct.DgraphTask{
		{Uuid: "a0000000-0000-0000-0000-00000000000a", Name: "Write the brief", Status: "in_progress", Project: member, BlockedOpen: 2, BlockedBy: []*dgraphStruct.DgraphTask{blocker}},
		{Uuid: "d0000000-0000-0000-0000-00000000000d", Name: "Ship it", Status: "todo", Project: member, BlockedOpen: 1, BlockedBy: []*dgraphStruct.DgraphTask{hidden}},
		{Uuid: "e0000000-0000-0000-0000-00000000000e", Name: "Plan", Status: "todo", Project: member},
		{Uuid: "f0000000-0000-0000-0000-00000000000f", Name: "Not Ben's", Status: "todo", Project: outsider, BlockedOpen: 1, BlockedBy: []*dgraphStruct.DgraphTask{blocker}},
	}
	got := readableOf(tasks, ben)

	a := got["a0000000-0000-0000-0000-00000000000a"]
	if a.Name != "Write the brief" || a.BlockedOpen != 2 || a.FirstBlocker == nil || a.FirstBlocker.UUID != "b0000000-0000-0000-0000-00000000000b" || a.FirstBlocker.Name != "Sign off the copy" {
		t.Fatalf("a readable blocker is named: %+v (%+v)", a, a.FirstBlocker)
	}
	if d := got["d0000000-0000-0000-0000-00000000000d"]; d.BlockedOpen != 1 || d.FirstBlocker != nil {
		t.Fatalf("a blocker Ben may not read is counted, never named: %+v", d)
	}
	if e := got["e0000000-0000-0000-0000-00000000000e"]; e.BlockedOpen != 0 || e.FirstBlocker != nil {
		t.Fatalf("waiting on nothing: %+v", e)
	}
	if _, ok := got["f0000000-0000-0000-0000-00000000000f"]; ok {
		t.Fatal("a task Ben may not read says nothing, its blockers neither")
	}
}
